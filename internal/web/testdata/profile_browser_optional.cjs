"use strict";
// Optional real-browser acceptance runner for the Profile workflow.
//
// It is not part of the normal Go test path and it adds no dependency to the
// repository. Run it with:
//
//   PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
//   CHROME_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
//   node internal/web/testdata/profile_browser_optional.cjs
//
// The runner drives a real Chrome through Playwright against the synthetic
// console fixture in internal/web/testdata/profile-fixture. That fixture is
// not production code: it binds 127.0.0.1 only, it holds synthetic
// observations and a synthetic identity, it never reads a kubeconfig, never
// reads a client secret and never contacts a cluster. Plan and operation
// writes are captured in memory by the fixture only.
//
// Configuration (all optional):
//   DASHBOARD_BASE_URL      an already running synthetic fixture to test
//   PLAYWRIGHT_MODULE       path to a Playwright installation; the plain
//                           "playwright" module is used when it is unset
//   CHROME_BIN              path to the Chrome binary
//   PROFILE_FIXTURE_DIR     a different synthetic fixture source directory
//   KF_ADMIN_REPO_ROOT      the console repository the fixture renders from
//   PROFILE_FIXTURE_PORT    loopback port of the synthetic fixture
//   PROFILE_GATED_FIXTURE_PORT  loopback port of the fixture without the
//                           Profile deletion capability
//
// Without a Playwright module or without a usable Chrome the runner prints
// SKIP and exits successfully. Everything else is a failure: a fixture that
// cannot be built or started, and every failed check, exit non-zero.
//
// The checks follow the user visible contract only: accessible names, stable
// element ids and the rendered result. Where a contract is still moving, the
// runner resolves the element through a documented list of candidates and
// reports the candidates it tried, so a mismatch is reported instead of
// silently passing.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..");
const FIXTURE_SOURCE = process.env.PROFILE_FIXTURE_DIR || path.join(__dirname, "profile-fixture");
const DEFAULT_CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const FIXTURE_PORT = process.env.PROFILE_FIXTURE_PORT || "18768";
const GATED_FIXTURE_PORT = process.env.PROFILE_GATED_FIXTURE_PORT || "18769";

const DESKTOP = { width: 1280, height: 844 };
const MOBILE = { width: 390, height: 844 };

// The synthetic fixture observations the checks rely on. The values match the
// fixture program: a Profile carries its apiVersion, its owner kind, its
// configuration digest and every observed quota key.
const PROFILE_ALPHA = {
  name: "team-alpha",
  owner: "admin@example.org",
  apiVersion: "kubeflow.org/v1",
  ownerKind: "User",
  quotas: [
    { key: "requests.cpu", value: "4" },
    { key: "requests.memory", value: "8Gi" },
    { key: "pods", value: "0" }
  ],
  unmanaged: { key: "requests.ephemeral-storage", value: "5Gi" }
};
const PROFILE_BETA = {
  name: "team-beta",
  owner: "beta.owner@example.org",
  apiVersion: "kubeflow.org/v1",
  ownerKind: "User",
  quotas: [
    { key: "requests.cpu", value: "1" },
    { key: "requests.memory", value: "2Gi" },
    { key: "pods", value: "3" }
  ],
  unmanaged: { key: "requests.ephemeral-storage", value: "5Gi" }
};
const RESERVED_NAMESPACE = "team-delta";
const NEW_PROFILE = "team-gamma";
const EXPIRED_PLAN_PROFILE = "team-epsilon";
const NEW_PROFILE_OWNER = "Gamma.Owner@Example.org";
const NAMESPACE = "team-alpha";
const NOTEBOOK = { kind: "Notebook", name: "team-alpha-notebook-0", namespace: NAMESPACE, detail: "node: node-a" };
const CLAIM = { kind: "PersistentVolumeClaim", name: "team-alpha-claim", namespace: NAMESPACE, detail: "storageClass: standard" };
const POD = { kind: "Pod", name: "team-alpha-notebook-0-0", namespace: NAMESPACE, detail: "node: node-a" };
const NODE = { kind: "Node", name: "node-a", detail: "version: v1.31.4" };

// ---------------------------------------------------------------- utilities

function loadPlaywright() {
  const candidates = [process.env.PLAYWRIGHT_MODULE, "playwright"].filter(Boolean);
  for (const candidate of candidates) {
    try { return require(candidate); } catch { /* try the next candidate */ }
  }
  return null;
}

// buildFixture compiles the synthetic fixture into an approved temporary
// directory. The artifact never lands in the repository and it is removed
// again when the run ends. A fixture that cannot be built is a failure, not
// a skip: a missing asset must never be masked.
async function buildFixture() {
  const source = FIXTURE_SOURCE;
  if (!fs.existsSync(path.join(source, "main.go"))) throw new Error("the synthetic fixture source is missing in " + source);
  const artifactDir = fs.mkdtempSync(path.join(os.tmpdir(), "kf-admin-profile-fixture-"));
  const artifact = path.join(artifactDir, "profile-fixture");
  const inside = source === REPO_ROOT || source.startsWith(REPO_ROOT + path.sep);
  const cwd = inside ? REPO_ROOT : source;
  const target = inside ? "./" + path.relative(REPO_ROOT, source) : ".";
  await new Promise((resolve, reject) => {
    const build = spawn("go", ["build", "-o", artifact, target], { cwd, stdio: ["ignore", "pipe", "pipe"] });
    let output = "";
    build.stdout.on("data", chunk => { output += chunk.toString(); });
    build.stderr.on("data", chunk => { output += chunk.toString(); });
    build.on("error", error => { fs.rmSync(artifactDir, { recursive: true, force: true }); reject(error); });
    build.on("exit", code => {
      if (code === 0) resolve(artifact);
      else { fs.rmSync(artifactDir, { recursive: true, force: true }); reject(new Error("the synthetic fixture did not build: " + output.trim())); }
    });
  });
  return { artifact, artifactDir };
}

// startFixture boots the synthetic console fixture on a loopback port. It is
// offline by construction: the fixture never opens a kubeconfig and never
// talks to a cluster. A fixed port keeps the base URL stable; an occupied
// port falls back to an ephemeral one.
async function startFixture({ artifact, writes = true, deletion = true, port } = {}) {
  const launch = async requestedPort => {
    const child = spawn(artifact, [], {
      env: {
        ...process.env,
        KF_ADMIN_REPO_ROOT: process.env.KF_ADMIN_REPO_ROOT || REPO_ROOT,
        KF_ADMIN_WRITES: writes ? "1" : "0",
        KF_ADMIN_PROFILE_DELETION: deletion ? "1" : "0",
        KF_ADMIN_PORT: requestedPort || ""
      },
      stdio: ["ignore", "pipe", "pipe"]
    });
    const base = await new Promise((resolve, reject) => {
      let output = "";
      const timer = setTimeout(() => reject(new Error("the synthetic fixture did not become ready: " + output.trim())), 60000);
      child.stdout.on("data", chunk => {
        output += chunk.toString();
        const match = output.match(/FIXTURE_READY (http:\/\/127\.0\.0\.1:\d+)/);
        if (match) { clearTimeout(timer); resolve(match[1]); }
      });
      child.stderr.on("data", chunk => { output += chunk.toString(); });
      child.on("error", error => { clearTimeout(timer); reject(error); });
      child.on("exit", code => { clearTimeout(timer); reject(new Error("the synthetic fixture exited with " + code + ": " + output.trim())); });
    });
    return { base, stop: () => { try { child.kill("SIGTERM"); } catch { /* already gone */ } } };
  };
  if (!port) return launch("");
  try { return await launch(port); }
  catch (error) {
    // The requested port is busy: an ephemeral port keeps the run alive.
    console.log("NOTE  profile_browser_optional: port " + port + " is busy (" + error.message + "), using an ephemeral port");
    return launch("");
  }
}

// A locator candidate. A string is a CSS selector, {role, name} is an
// accessible name lookup and {text, tag} is a text content lookup.
function toLocator(page, spec) {
  if (typeof spec === "string") return page.locator(spec);
  if (spec.role) return page.getByRole(spec.role, spec.name === undefined ? undefined : { name: spec.name });
  if (spec.text) return page.locator(`${spec.tag || "*"}:has-text("${spec.text}")`);
  throw new Error("unsupported locator candidate");
}

// findVisible resolves the first visible element of a candidate list. With
// {soft: true} it returns null instead of throwing. A hard failure names
// every candidate it tried, so a moved contract is reported, not guessed.
async function findVisible(page, candidates, description, options = {}) {
  const tried = [];
  for (const spec of candidates) {
    const locator = toLocator(page, spec);
    tried.push(typeof spec === "string" ? spec : spec.role ? `role=${spec.role} name=${String(spec.name)}` : `text=${spec.text}`);
    const count = await locator.count().catch(() => 0);
    if (!count) continue;
    for (let index = 0; index < count; index++) {
      const item = locator.nth(index);
      if (await item.isVisible().catch(() => false)) return item;
    }
  }
  if (options.soft) return null;
  throw new Error(`selector contract not satisfied: ${description} (tried: ${tried.join(" | ")})`);
}

// attachCapture records the typed write requests the console sends. The
// console is the only writer; the fixture only records them. The answered
// plan is recorded too: its expiry is what the console judges the
// confirmation against, so the runner has to see it.
function attachCapture(page) {
  const sink = { plans: [], operations: [], planResponses: [] };
  page.on("request", request => {
    let route = "";
    try { route = new URL(request.url()).pathname; } catch { return; }
    if (request.method() !== "POST") return;
    if (route !== "/api/v1/plans" && route !== "/api/v1/operations") return;
    let body = null;
    const raw = request.postData();
    if (raw) { try { body = JSON.parse(raw); } catch { body = raw; } }
    const entry = { body, headers: request.headers() };
    if (route === "/api/v1/plans") sink.plans.push(entry); else sink.operations.push(entry);
  });
  page.on("response", response => {
    let route = "";
    try { route = new URL(response.url()).pathname; } catch { return; }
    if (route !== "/api/v1/plans") return;
    response.json()
      .then(body => { sink.planResponses.push({ status: response.status(), body }); })
      .catch(() => { /* an answer that is not JSON carries no expiry */ });
  });
  return sink;
}

function visibleText(locator) {
  return locator.innerText().catch(() => "");
}

// gotoTable opens a console page and waits for its table to render.
async function gotoTable(page, base, route, tableKey) {
  await page.goto(base + route, { waitUntil: "domcontentloaded" });
  const table = page.locator(`[data-table="${tableKey}"]`);
  await table.waitFor({ timeout: 15000 });
  await table.locator("tbody tr").first().waitFor({ timeout: 15000 });
}

// rowFor returns the table row that shows exactly the given resource name.
async function rowFor(page, tableKey, name) {
  const rows = page.locator(`[data-table="${tableKey}"] tbody tr`);
  const exact = rows.filter({ has: page.getByText(name, { exact: true }) });
  if (await exact.count()) return exact.first();
  const byControl = rows.filter({ has: page.getByRole("button", { name, exact: true }) });
  if (await byControl.count()) return byControl.first();
  const byText = rows.filter({ hasText: name });
  await byText.first().waitFor({ timeout: 10000 });
  return byText.first();
}

// ensureProfilesPage opens the Profiles page when it is not rendered yet.
async function ensureProfilesPage(page, base) {
  if (await page.locator('[data-table="profiles"] tbody tr').count()) return;
  await gotoTable(page, base, "/profiles", "profiles");
}

// openProfileDrawer opens the Profile management drawer in create mode.
async function openProfileDrawer(page, base) {
  await ensureProfilesPage(page, base);
  const control = await findVisible(page, [
    '[data-open-drawer="profile"]',
    { role: "button", name: /manage profile/i }
  ], "the Profile management control");
  await control.click();
  const drawer = page.locator("#profile-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  return drawer;
}

// openProfileSummary opens the scoped summary drawer of one Profile.
async function openProfileSummary(page, base, name) {
  await ensureProfilesPage(page, base);
  const row = await rowFor(page, "profiles", name);
  const control = await findVisible(row, [
    { role: "button", name, exact: true },
    `button:has-text("${name}")`
  ], `the ${name} profile name control`);
  await control.click();
  const drawer = page.locator("#profile-summary-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  return drawer;
}

// profileMessage returns the validation message the console shows for the
// Profile form: the live name message and the form error region.
async function profileMessage(drawer) {
  const parts = [];
  for (const selector of ["#profile-name-message", ".form-error"]) {
    const region = drawer.locator(selector);
    if (!(await region.count())) continue;
    for (let index = 0; index < await region.count(); index++) {
      const item = region.nth(index);
      if (!(await item.isVisible().catch(() => false))) continue;
      const text = (await item.innerText().catch(() => "")).trim();
      if (text) parts.push(text);
    }
  }
  return parts.join("\n");
}

// quotaInput returns the editable quota field of a managed quota key.
function quotaInput(drawer, key) {
  return drawer.locator(`#profile-form input[data-profile-quota="${key}"]`);
}

// extraQuotaInput returns the read-only field of a quota key the console does
// not manage.
function extraQuotaInput(drawer, key) {
  return drawer.locator(`#profile-extra-quota-fields input[data-profile-quota-extra="${key}"]`);
}

// observedProfile reads one observed Profile from the fixture inventory.
async function observedProfile(base, name) {
  const profiles = await fetchJson(base + "/api/v1/profiles");
  return profiles.resources.find(resource => resource.kind === "Profile" && resource.name === name) || null;
}

// quotaPair reports whether a drawer shows a quota key with its value. A
// quota list entry, a definition list entry and a plain key/value line are
// all accepted.
async function quotaPair(drawer, key, value) {
  return drawer.evaluate((node, payload) => {
    // A quota list entry names the key in a code element and keeps the value
    // next to it.
    for (const code of node.querySelectorAll("code")) {
      if (code.textContent.trim() !== payload.key) continue;
      const scope = code.parentElement || code;
      const own = scope.textContent.replace(code.textContent, "").trim();
      if (own === payload.value || own.includes(payload.value)) return { ok: true, how: "quota list" };
      const siblings = [...scope.querySelectorAll("*")].map(child => child.textContent.trim());
      if (siblings.includes(payload.value)) return { ok: true, how: "quota list" };
    }
    // A definition list entry names the key and keeps the value in the next
    // element.
    for (const term of node.querySelectorAll("dt")) {
      if (!term.textContent.includes(payload.key)) continue;
      const valueNode = term.nextElementSibling;
      if (valueNode && valueNode.textContent.trim() === payload.value) return { ok: true, how: "definition list" };
    }
    // A plain key/value line keeps both in one short text.
    for (const element of node.querySelectorAll("*")) {
      if (element.children.length > 4) continue;
      const text = (element.textContent || "").trim();
      if (!text || text.length > 200 || !text.includes(payload.key)) continue;
      const tokens = text.split(/[\s:]+/).filter(Boolean);
      if (tokens.includes(payload.value)) return { ok: true, how: "key/value line" };
    }
    return { ok: false, how: "not found" };
  }, { key, value });
}

// assertRightAligned proves that a drawer is the right hand side panel.
async function assertRightAligned(page, drawer, description) {
  const box = await drawer.boundingBox();
  assert.ok(box, description + " has no layout box");
  const viewport = page.viewportSize();
  assert.ok(Math.abs((box.x + box.width) - viewport.width) <= 2,
    `${description} is not anchored to the right edge (right=${Math.round(box.x + box.width)} viewport=${viewport.width})`);
}

async function fetchJson(url) {
  const response = await fetch(url);
  if (!response.ok) throw new Error("fixture request failed: " + response.status + " " + url);
  return response.json();
}

// waitForPlanResponse waits until the console received the answered plan
// request. The answer carries the expiry the console judges the confirmation
// against.
async function waitForPlanResponse(capture, count, timeout = 10000) {
  const deadline = Date.now() + timeout;
  while (capture.planResponses.length < count) {
    if (Date.now() > deadline) {
      throw new Error(`the console did not receive ${count} answered plan request(s)`);
    }
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  return capture.planResponses[count - 1];
}

// planClock reads the clock of the browser, the clock of the fixture and the
// expiry of the answered plan. The console judges a plan against the clock it
// runs on, so all three belong together: a plan is only live when its expiry
// is in the future of the browser clock, and the browser clock and the
// fixture clock have to agree.
async function planClock(context, answered) {
  const { page } = context;
  const body = (answered && answered.body) || {};
  const browserNow = await page.evaluate(() => Date.now());
  const expiresAt = new Date(body.expiresAt || "").getTime();
  const state = await fetchJson(context.base + "/__fixture").catch(() => null);
  const fixtureNow = state && state.clock ? new Date(state.clock.now).getTime() : NaN;
  const planTTLSeconds = (state && state.clock && state.clock.planTTLSeconds) || null;
  const report = [
    "browser now=" + new Date(browserNow).toISOString(),
    "fixture now=" + (Number.isFinite(fixtureNow) ? new Date(fixtureNow).toISOString() : "unknown"),
    "plan expires=" + (Number.isFinite(expiresAt) ? new Date(expiresAt).toISOString() : "unknown"),
    "fixture plan TTL=" + (planTTLSeconds === null ? "unknown" : planTTLSeconds + "s")
  ].join(" ");
  return {
    browserNow, expiresAt, fixtureNow, report, planTTLSeconds,
    skew: Number.isFinite(fixtureNow) ? fixtureNow - browserNow : NaN
  };
}

// assertLivePlan proves that the server handed out a plan the console may
// submit. The clocks are reported, so a server that answers with a stale
// expiry is named instead of guessed.
async function assertLivePlan(context, capture, count) {
  const answered = await waitForPlanResponse(capture, count);
  assert.ok([200, 201].includes(answered.status), "the console did not get a prepared plan from the server");
  const clock = await planClock(context, answered);
  console.log("      plan clock: " + clock.report);
  assert.ok(Number.isFinite(clock.expiresAt), "the prepared plan carries no usable expiry (" + clock.report + ")");
  assert.ok(clock.expiresAt > clock.browserNow,
    `the server handed out a plan that has already expired (${clock.report})`);
  if (Number.isFinite(clock.fixtureNow)) {
    assert.ok(Math.abs(clock.skew) <= 60000,
      `the browser clock and the fixture clock differ by ${Math.round(clock.skew / 1000)}s (${clock.report})`);
  }
  return clock;
}

// ------------------------------------------------------------------- checks

// The console is rendered by Go's html/template with a minimal Config
// projection and the signed-in identity. Nothing else may reach the browser.
async function checkRenderedProjection(context) {
  const { page } = context;
  const response = await page.goto(context.base + "/", { waitUntil: "domcontentloaded" });
  const html = await response.text();
  assert.ok(!html.includes("{{") && !html.includes("}}"), "the rendered page contains unexecuted template markup");
  const state = await fetchJson(context.base + "/__fixture").catch(() => null);
  const csrf = await page.locator('meta[name="csrf-token"]').getAttribute("content");
  assert.ok(csrf && csrf.trim(), "the CSRF token is not projected");
  if (state) assert.equal(csrf, state.identity.csrf, "the projected CSRF token is the session token");
  const scope = await page.locator('meta[name="notification-scope"]').getAttribute("content");
  assert.ok(scope && scope.trim(), "the notification scope is not projected");
  for (const name of ["notebook-writes", "static-storage", "pvc-expansion", "pod-restarts", "node-cordon", "profile-deletion"]) {
    const value = await page.locator(`meta[name="${name}"]`).getAttribute("content");
    assert.ok(value !== null, `the ${name} capability is not projected`);
  }
  const writes = await page.locator("body").getAttribute("data-writes");
  if (state) {
    assert.equal(writes, String(state.features.writesEnabled), "the write capability does not match the configuration");
    assert.equal(await page.locator('meta[name="profile-deletion"]').getAttribute("content"), String(state.features.profileDeletion),
      "the deletion capability does not match the configuration");
  }
  if (state) {
    for (const secret of state.forbiddenStrings) {
      assert.ok(!html.includes(secret), "the rendered page leaks a configuration secret: " + secret);
    }
  }
  const account = (await page.locator(".account span").first().innerText()).trim();
  assert.ok(account.length > 0, "the signed-in identity is not shown");
  if (state) assert.equal(account, state.identity.displayName, "the signed-in identity is not the projected one");
}

// A new Profile name is validated in the browser: every invalid name is
// refused with a visible message and never reaches the plan API.
async function checkInvalidProfileNames(context) {
  const { page, capture } = context;
  const cases = [
    { label: "uppercase letters", name: "Team-Gamma", pattern: /lowercase/i },
    { label: "a dot and an underscore", name: "team_gamma.one", pattern: /dots? or underscores|underscore|dot/i, tooltip: /hyphens/i },
    { label: "a trailing hyphen", name: "team-gamma-", pattern: /start and end|hyphen/i },
    { label: "64 characters", name: "t".repeat(64), pattern: /63|length|characters/i }
  ];
  for (const testCase of cases) {
    const drawer = await openProfileDrawer(page, context.base);
    const before = capture.plans.length;
    await drawer.locator("#profile-create-name").fill(testCase.name);
    await drawer.locator("#profile-create-owner").fill("owner@example.org");
    await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
    await page.waitForTimeout(150);
    const stored = await drawer.locator("#profile-create-name").inputValue();
    await drawer.locator('button[type="submit"]').click();
    await page.waitForTimeout(300);
    // A Profile name is a DNS-1123 label: the console never plans a name
    // that is longer than 63 characters.
    const overlong = capture.plans.slice(before).filter(plan => String((plan.body || {}).name || "").length > 63);
    assert.equal(overlong.length, 0, `the console planned a Profile name longer than 63 characters (${testCase.label})`);
    if (stored.length < testCase.name.length) {
      // The field itself refused the input: the console never accepted a name
      // that is too long.
      assert.ok(stored.length <= 63, `the console accepted an over-long name (${stored.length} characters)`);
      assert.ok(testCase.name.startsWith(stored), `the console silently rewrote the typed name "${testCase.name}"`);
    } else {
      assert.equal(stored, testCase.name, `the console silently rewrote the invalid name "${testCase.name}"`);
      assert.equal(capture.plans.length, before, `the invalid name "${testCase.name}" (${testCase.label}) still reached the plan API`);
      const message = await profileMessage(drawer);
      assert.match(message, testCase.pattern, `the refusal for "${testCase.name}" (${testCase.label}) is not explained to the user`);
      // The refused name carries the short reason as its tooltip and as its
      // own validity message.
      const title = (await drawer.locator("#profile-create-name").getAttribute("title")) || "";
      const nameMessage = (await drawer.locator("#profile-name-message").innerText()).trim();
      assert.ok(title.trim(), `the refused name "${testCase.name}" has no tooltip`);
      assert.equal(title.trim(), nameMessage, "the tooltip does not carry the validation message");
      if (testCase.tooltip) assert.match(title, testCase.tooltip, "the tooltip does not name the accepted characters");
    }
    await page.keyboard.press("Escape");
    await drawer.waitFor({ state: "hidden", timeout: 5000 });
    // A refused case can leave the reviewed plan dialog open.
    const dialog = page.locator("#plan-dialog");
    if (await dialog.isVisible().catch(() => false)) {
      await page.locator("#cancel-plan").click();
      await dialog.waitFor({ state: "hidden", timeout: 5000 });
    }
  }
}

// A name with a dot or an underscore is refused with a hyphenated
// suggestion. The typed name is never rewritten behind the administrator.
async function checkProfileNameSuggestion(context) {
  const { page, capture } = context;
  const typed = "team_gamma";
  const suggestion = "team-gamma";
  const drawer = await openProfileDrawer(page, context.base);
  const before = capture.plans.length;
  await drawer.locator("#profile-create-name").fill(typed);
  await drawer.locator("#profile-create-owner").fill("owner@example.org");
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await page.waitForTimeout(200);
  const suggested = await findVisible(page, ["#profile-name-suggestion"], "the name suggestion", { soft: true });
  assert.ok(suggested, "the console does not offer a suggestion for a dotted or underscored name");
  const suggestedText = await visibleText(suggested);
  assert.ok(suggestedText.includes(suggestion), `the console does not offer the hyphenated suggestion "${suggestion}" (shown: ${JSON.stringify(suggestedText)})`);
  assert.equal(await drawer.locator("#profile-create-name").inputValue(), typed, "the console silently rewrote the typed name");
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "a name with an underscore reached the plan API");
  assert.equal(await drawer.locator("#profile-create-name").inputValue(), typed, "the console silently rewrote the typed name");
  assert.match(await profileMessage(drawer), /dots? or underscores|underscore|dot/i, "the console does not explain the rejected characters");
  // The suggestion is only applied when the administrator asks for it.
  await drawer.locator("#profile-use-suggestion").click();
  await page.waitForTimeout(200);
  assert.equal(await drawer.locator("#profile-create-name").inputValue(), suggestion, "the suggested name is not applied on request");
  assert.equal(capture.plans.length, before, "applying the suggestion submitted a request");
}

// An existing Profile name is refused: the console never adopts an existing
// Profile.
async function checkDuplicateProfileRefused(context) {
  const { page, capture } = context;
  const drawer = await openProfileDrawer(page, context.base);
  const before = capture.plans.length;
  await drawer.locator("#profile-create-name").fill(PROFILE_ALPHA.name);
  await drawer.locator("#profile-create-owner").fill("owner@example.org");
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await page.waitForTimeout(150);
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "an existing Profile name reached the plan API");
  assert.match(await profileMessage(drawer), /already exists|exists|duplicate|taken|in use/i,
    "the console does not report the duplicate Profile name");
}

// An existing namespace reserves its name: a new Profile never adopts it.
async function checkReservedNamespaceRefused(context) {
  const { page, capture } = context;
  const drawer = await openProfileDrawer(page, context.base);
  const before = capture.plans.length;
  await drawer.locator("#profile-create-name").fill(RESERVED_NAMESPACE);
  await drawer.locator("#profile-create-owner").fill("owner@example.org");
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await page.waitForTimeout(150);
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "an existing namespace name reached the plan API");
  assert.match(await profileMessage(drawer), /already exists|exists|duplicate|taken|in use/i,
    "the console does not report that the namespace name is reserved");
}

// A valid, unique Profile name produces one typed plan request and one
// typed operation request. The fixture records them and mutates nothing.
async function checkValidProfileCreation(context) {
  const { page, capture } = context;
  const drawer = await openProfileDrawer(page, context.base);
  // The creation form requests an owner identity as an email address and
  // sends it exactly as entered, including its case.
  const ownerField = drawer.locator("#profile-create-owner");
  assert.ok(await ownerField.count(), "the creation form has no owner field");
  assert.equal(await ownerField.getAttribute("type"), "email", "the creation form does not request an owner email address");
  await drawer.locator("#profile-create-name").fill(NEW_PROFILE);
  await ownerField.fill(NEW_PROFILE_OWNER);
  await drawer.locator('#profile-form input[data-profile-quota="requests.cpu"]').fill("4");
  await drawer.locator('#profile-form input[data-profile-quota="requests.memory"]').fill("8Gi");
  await drawer.locator('#profile-form input[data-profile-quota="pods"]').fill("5");
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await drawer.locator('button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length, 1, "the console did not send exactly one plan request");
  const plan = capture.plans[0].body;
  assert.ok(plan && typeof plan === "object", "the plan request is not a JSON object");
  assert.equal(plan.kind, "profile.create", "the plan request does not use the typed create kind");
  assert.equal(plan.name, NEW_PROFILE, "the plan request does not carry the typed name");
  assert.equal(plan.owner, NEW_PROFILE_OWNER, "the plan request does not carry the owner identity exactly as entered");
  assert.ok(plan.reason && String(plan.reason).trim(), "the plan request does not carry the reason");
  assert.equal(plan.confirmName, undefined, "a creation request must not carry a deletion confirmation");
  assert.equal(plan.baseConfigDigest, undefined, "a creation request must not carry a base configuration digest");
  const allowed = new Set(["kind", "namespace", "name", "reason", "owner", "quota", "confirmName", "baseConfigDigest"]);
  for (const key of Object.keys(plan)) {
    assert.ok(allowed.has(key), "the plan request carries an unsupported field: " + key);
  }
  if (plan.quota !== undefined) {
    assert.deepEqual(plan.quota, { "requests.cpu": "4", "requests.memory": "8Gi", "pods": "5" }, "the plan request does not carry the typed quotas");
  }
  const preview = await dialog.innerText();
  assert.ok(preview.includes(NEW_PROFILE), "the plan preview does not show the reviewed name");
  assert.ok(preview.includes(NEW_PROFILE_OWNER), "the plan preview does not show the reviewed owner");
  // Nothing is submitted until the administrator reviewed the plan.
  assert.equal(capture.operations.length, 0, "the operation was submitted before the plan was reviewed");
  // The plan the server handed out has to be live for the clock the console
  // runs on. A stale expiry is a server problem and it is reported here.
  const clock = await assertLivePlan(context, capture, 1);
  // The submit control is enabled by the confirmation of the administrator
  // and by nothing else: the runner never forces it.
  const submit = page.locator("#submit-plan");
  assert.equal(await submit.isDisabled(), true, "the submit control is enabled before the plan is confirmed");
  await page.locator("#confirm-plan").check();
  await page.waitForTimeout(50);
  assert.equal(await submit.isDisabled(), false,
    "the console does not enable the submit control after the plan is confirmed (" + clock.report + ")");
  await submit.click();
  await page.waitForTimeout(400);
  assert.equal(capture.operations.length, 1, "the reviewed plan was not submitted exactly once");
  const operation = capture.operations[0].body;
  assert.ok(operation && typeof operation === "object", "the operation request is not a JSON object");
  assert.ok(operation.planID, "the operation request does not carry the plan id");
  const key = capture.operations[0].headers["idempotency-key"] || operation.idempotencyKey;
  assert.ok(key && String(key).length >= 16, "the operation request does not carry an idempotency key");
  // The fixture records the request and mutates nothing.
  const profiles = await fetchJson(context.base + "/api/v1/profiles");
  const names = profiles.resources.filter(resource => resource.kind === "Profile").map(resource => resource.name).sort();
  assert.deepEqual(names, ["team-alpha", "team-beta"], "the fixture inventory changed after a plan and operation");
}

// The Profile summary shows the observed Profile contract: its API version,
// its owner and every quota key.
async function checkProfileSummaryContents(context) {
  const { page } = context;
  const drawer = await openProfileSummary(page, context.base, PROFILE_ALPHA.name);
  const text = await visibleText(drawer);
  assert.ok(text.includes(PROFILE_ALPHA.apiVersion), `the Profile summary does not show the apiVersion ${PROFILE_ALPHA.apiVersion}`);
  assert.ok(text.includes(PROFILE_ALPHA.owner), `the Profile summary does not show the owner ${PROFILE_ALPHA.owner}`);
  for (const quota of PROFILE_ALPHA.quotas) {
    const pair = await quotaPair(drawer, quota.key, quota.value);
    assert.ok(pair.ok, `the Profile summary does not show ${quota.key} = ${quota.value} (${pair.how})`);
  }
  const unmanaged = await quotaPair(drawer, PROFILE_ALPHA.unmanaged.key, PROFILE_ALPHA.unmanaged.value);
  assert.ok(unmanaged.ok, `the Profile summary does not show ${PROFILE_ALPHA.unmanaged.key} = ${PROFILE_ALPHA.unmanaged.value} (${unmanaged.how})`);
}

// The Profile update form prefills the observed owner as a read-only value,
// the observed quota and the configuration precondition.
async function checkProfileUpdatePrefill(context) {
  const { page } = context;
  const observed = await observedProfile(context.base, PROFILE_ALPHA.name);
  assert.ok(observed, "the observed Profile is missing from the inventory");
  const digest = (observed.details || {})["configDigest"];
  assert.ok(digest, "the observed Profile carries no configuration digest");
  const drawer = await openProfileSummary(page, context.base, PROFILE_ALPHA.name);
  const edit = await findVisible(drawer, [
    { role: "button", name: /^edit profile$/i },
    { role: "button", name: /edit/i }
  ], "the Profile edit action");
  await edit.click();
  const form = page.locator("#profile-drawer");
  await form.waitFor({ state: "visible", timeout: 10000 });
  await page.waitForTimeout(200);

  // The update selector shows the Profile that is being updated.
  const selected = await form.locator("#profile-update-name").inputValue();
  assert.equal(selected, PROFILE_ALPHA.name, "the update selector does not show the updated Profile");

  // The current owner is shown, but it cannot be changed in place: the
  // upstream Profile controller rejects an owner transfer.
  const owner = form.locator("#profile-update-owner");
  assert.ok(await owner.count(), "the update form has no owner field");
  assert.equal(await owner.inputValue(), PROFILE_ALPHA.owner, "the update form is not prefilled with the current owner");
  assert.equal(await owner.evaluate(node => node.readOnly === true), true, "the current owner is editable in the update form");
  const ownerHint = await visibleText(form.locator("#profile-owner-hint"));
  assert.match(ownerHint, /owner transfer is unsupported|cannot transfer|read-only/i,
    "the console does not explain that an owner transfer is unsupported");

  // The configuration precondition travels with the request and is never
  // rendered as an editable value.
  const digestField = form.locator("#profile-update-digest");
  assert.ok(await digestField.count(), "the update form has no configuration precondition");
  assert.equal(await digestField.inputValue(), digest, "the update form does not carry the observed configuration precondition");

  // The supported quota keys are prefilled and stay editable.
  for (const quota of PROFILE_ALPHA.quotas) {
    const field = quotaInput(form, quota.key);
    assert.ok(await field.count(), `the update form has no field for the supported quota ${quota.key}`);
    assert.equal(await field.evaluate(node => node.readOnly === true), false, `the supported quota ${quota.key} is read-only in the update form`);
    assert.equal(await field.inputValue(), quota.value, `the update form is not prefilled with the observed ${quota.key}`);
  }
  // A quota key the console does not manage is shown, but never editable.
  const extra = extraQuotaInput(form, PROFILE_ALPHA.unmanaged.key);
  assert.ok(await extra.count(), `the update form does not show the unmanage quota key ${PROFILE_ALPHA.unmanaged.key}`);
  const extraState = await extra.evaluate(node => ({
    readOnly: node.readOnly === true,
    disabled: node.disabled === true,
    type: node.type,
    value: node.value
  }));
  assert.ok(extraState.readOnly || extraState.disabled || extraState.type === "hidden",
    "an unmanage quota key is editable in the update form");
  assert.equal(extraState.value, PROFILE_ALPHA.unmanaged.value, "the unmanage quota key does not keep its observed value");
}

// An update is refused while nothing would change. A real change is sent
// with the unchanged owner, the configuration precondition and every observed
// quota key.
async function checkProfileUpdateRoundTrip(context) {
  const { page, capture } = context;
  const observed = await observedProfile(context.base, PROFILE_BETA.name);
  assert.ok(observed, "the observed Profile is missing from the inventory");
  const digest = (observed.details || {})["configDigest"];
  const drawer = await openProfileDrawer(page, context.base);
  await drawer.locator("#profile-kind").selectOption("profile.update");
  await page.waitForTimeout(200);
  await drawer.locator("#profile-update-name").selectOption(PROFILE_BETA.name);
  await page.waitForTimeout(300);
  assert.equal(await drawer.locator("#profile-update-owner").inputValue(), PROFILE_BETA.owner, "the update form is not prefilled with the observed owner");

  // A no-op update is refused before any request is made.
  const before = capture.plans.length;
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "an update without a quota change reached the plan API");
  assert.match(await profileMessage(drawer), /nothing would change|no change/i,
    "the console does not report that the update would change nothing");

  // A real change is sent with the typed fields only.
  const cpu = quotaInput(drawer, "requests.cpu");
  await cpu.fill("2");
  await drawer.locator('button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length, before + 1, "the update did not produce exactly one plan request");
  const plan = capture.plans[capture.plans.length - 1].body;
  assert.ok(plan && typeof plan === "object", "the update plan request is not a JSON object");
  assert.equal(plan.kind, "profile.update", "the update request does not use the typed update kind");
  assert.equal(plan.name, PROFILE_BETA.name, "the update request does not name the Profile");
  assert.equal(plan.owner, PROFILE_BETA.owner, "the update request does not submit the unchanged owner");
  assert.equal(plan.baseConfigDigest, digest, "the update request does not carry the base configuration digest of the observed Profile");
  assert.equal(plan.namespace, undefined, "the update request carries a namespace");
  assert.equal(plan.confirmName, undefined, "an update request must not carry a deletion confirmation");
  assert.ok(plan.quota && typeof plan.quota === "object", "the update request does not carry the quota values");
  assert.equal(plan.quota["requests.cpu"], "2", "the update request does not carry the changed quota value");
  for (const quota of PROFILE_BETA.quotas.filter(item => item.key !== "requests.cpu")) {
    assert.equal(plan.quota[quota.key], quota.value, `the update request does not carry the unchanged quota ${quota.key}`);
  }
  assert.equal(plan.quota[PROFILE_BETA.unmanaged.key], PROFILE_BETA.unmanaged.value,
    "the update request does not return the unmanage quota key unchanged");
  // The server side before and after difference is shown before the
  // operation is submitted.
  const changes = page.locator("#plan-changes");
  assert.ok(await changes.count(), "the plan preview does not show the validated difference");
  const changesText = await visibleText(changes);
  assert.ok(changesText.includes("requests.cpu"), "the validated difference does not name the changed quota key");
  assert.ok(changesText.includes("2"), "the validated difference does not show the new quota value");
  assert.equal(capture.operations.length, 0, "the update was submitted before the plan was reviewed");
  // The update plan has to be live for the clock the console runs on.
  const clock = await assertLivePlan(context, capture, capture.plans.length);
  // The submit control is enabled by the confirmation and by nothing else.
  const submit = page.locator("#submit-plan");
  assert.equal(await submit.isDisabled(), true, "the submit control is enabled before the plan is confirmed");
  await page.locator("#confirm-plan").check();
  await page.waitForTimeout(50);
  assert.equal(await submit.isDisabled(), false,
    "the console does not enable the submit control after the plan is confirmed (" + clock.report + ")");
  await submit.click();
  await page.waitForTimeout(400);
  assert.equal(capture.operations.length, 1, "the reviewed update was not submitted exactly once");
}

// A plan that has already expired is never submitted. The expiry is
// simulated by the runner: the answer of the server is replaced with an
// expired one on the way to the browser, so neither the production server
// nor the console changes. The console keeps the submit control disabled and
// sends no operation, even though the administrator confirmed the plan.
async function checkExpiredPlanRefused(context) {
  const { page, capture } = context;
  const expiredAt = new Date(Date.now() - 60 * 1000).toISOString();
  const createdAt = new Date(Date.now() - 15 * 60 * 1000).toISOString();
  await page.route("**/api/v1/plans", async route => {
    const response = await route.fetch();
    let body = null;
    try { body = await response.json(); } catch { body = null; }
    if (body && typeof body === "object") {
      body.createdAt = createdAt;
      body.expiresAt = expiredAt;
    }
    await route.fulfill({ response, json: body });
  });
  const drawer = await openProfileDrawer(page, context.base);
  await drawer.locator("#profile-create-name").fill(EXPIRED_PLAN_PROFILE);
  await drawer.locator("#profile-create-owner").fill("Expired.Owner@Example.org");
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await drawer.locator('button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  // The expiry of the plan is visible to the administrator before the plan
  // is confirmed.
  const preview = await dialog.innerText();
  assert.match(preview, /expires/i, "the plan preview does not show the expiry of the plan");
  const submit = page.locator("#submit-plan");
  assert.equal(await submit.isDisabled(), true, "the submit control is enabled before the plan is confirmed");
  // The administrator confirms the plan for real. The console still refuses
  // to submit it, because its expiry has passed.
  await page.locator("#confirm-plan").check();
  await page.waitForTimeout(100);
  assert.equal(await submit.isDisabled(), true, "the console enables the submit control for a plan that has expired");
  assert.equal(capture.operations.length, 0, "an expired plan was submitted");
  // The expired plan is abandoned and nothing is sent afterwards either.
  await page.locator("#cancel-plan").click();
  await dialog.waitFor({ state: "hidden", timeout: 5000 });
  assert.equal(capture.operations.length, 0, "an expired plan was submitted after the dialog was closed");
}

// An observed quota of zero is a real value. The update form must accept it,
// and the request must carry it unchanged. A native range constraint must
// never block an update that only changes another quota key.
async function checkZeroQuotaUpdate(context) {
  const { page, capture } = context;
  const observed = await observedProfile(context.base, PROFILE_ALPHA.name);
  const digest = (observed.details || {})["configDigest"];
  const drawer = await openProfileDrawer(page, context.base);
  await drawer.locator("#profile-kind").selectOption("profile.update");
  await page.waitForTimeout(200);
  await drawer.locator("#profile-update-name").selectOption(PROFILE_ALPHA.name);
  await page.waitForTimeout(300);
  const pods = quotaInput(drawer, "pods");
  assert.equal(await pods.inputValue(), "0", "the observed pod quota of zero is not prefilled");
  // Every required field is filled before the form is judged: an empty
  // reason must not be mistaken for a quota problem.
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  const validity = await page.locator("#profile-form").evaluate(form => ({
    valid: form.checkValidity(),
    invalid: [...form.querySelectorAll("input,select")]
      .filter(field => !field.disabled && !field.validity.valid)
      .map(field => ({ name: field.name || field.id, value: field.value, min: field.min || null, underflow: field.validity.rangeUnderflow }))
  }));
  assert.ok(validity.valid, "the update form is blocked by native validation: " + JSON.stringify(validity.invalid));
  // The unchanged form is still refused by the console itself.
  const before = capture.plans.length;
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "an update without a quota change reached the plan API");
  assert.match(await profileMessage(drawer), /nothing would change|no change/i,
    "the console does not report that the update would change nothing");
  // A change of the CPU quota alone is sent with the observed zero.
  await quotaInput(drawer, "requests.cpu").fill("6");
  await drawer.locator('button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length, before + 1, "the update did not produce exactly one plan request");
  const plan = capture.plans[capture.plans.length - 1].body;
  assert.equal(plan.kind, "profile.update", "the update request does not use the typed update kind");
  assert.equal(plan.name, PROFILE_ALPHA.name, "the update request does not name the Profile");
  assert.equal(plan.owner, PROFILE_ALPHA.owner, "the update request does not submit the unchanged owner");
  assert.equal(plan.baseConfigDigest, digest, "the update request does not carry the base configuration digest");
  assert.equal(plan.namespace, undefined, "the update request carries a namespace");
  assert.ok(plan.quota && typeof plan.quota === "object", "the update request does not carry the quota values");
  assert.equal(plan.quota["pods"], "0", "the update request does not carry the observed pod quota of zero");
  assert.equal(plan.quota["requests.cpu"], "6", "the update request does not carry the changed CPU quota");
  assert.equal(plan.quota["requests.memory"], "8Gi", "the update request does not carry the observed memory quota");
  assert.equal(plan.quota[PROFILE_ALPHA.unmanaged.key], PROFILE_ALPHA.unmanaged.value,
    "the update request does not return the unmanage quota key unchanged");
}

// The destructive Profile action is offered only when the server projects
// the deletion capability.
async function checkDeletionCapabilityGated(context) {
  const { page } = context;
  const state = await fetchJson(context.base + "/__fixture").catch(() => null);
  const drawer = await openProfileSummary(page, context.base, PROFILE_ALPHA.name);
  const remove = await findVisible(drawer, [
    { role: "button", name: /delete profile/i },
    { role: "button", name: /^delete$/i }
  ], "the destructive Profile action", { soft: true });
  if (!state) {
    // The capability of an external fixture cannot be probed. What the
    // console offers is reported instead of guessing.
    console.log("NOTE  profile_browser_optional: the fixture does not expose its capabilities; the destructive Profile action is " + (remove ? "offered" : "not offered"));
    return;
  }
  if (!state.features.profileDeletion) {
    if (remove) {
      const disabled = await remove.isDisabled().catch(() => false);
      assert.ok(disabled, "the destructive Profile action is offered while the deletion capability is disabled");
    }
    return;
  }
  assert.ok(remove, "the Profile summary does not offer the destructive action while the deletion capability is enabled");
  assert.equal(await remove.isDisabled().catch(() => false), false, "the destructive Profile action is disabled while the capability is enabled");
}

// A destructive Profile action needs an explicit confirmation and shows its
// warning before anything is submitted. There is no separate namespace
// deletion: the only typed destructive action is profile.delete.
async function checkDestructiveConfirmation(context) {
  const { page, capture } = context;
  const drawer = await openProfileSummary(page, context.base, PROFILE_ALPHA.name);
  const namespaceDelete = await findVisible(drawer, [
    { role: "button", name: /delete namespace|namespace delete/i },
    { role: "link", name: /delete namespace|namespace delete/i }
  ], "a separate namespace deletion", { soft: true });
  assert.equal(namespaceDelete, null, "the console offers a separate namespace deletion");
  const remove = await findVisible(drawer, [
    { role: "button", name: /delete profile/i }
  ], "the destructive Profile action");
  const before = capture.plans.length + capture.operations.length;
  await remove.click();
  const deletion = page.locator("#profile-delete-drawer");
  await deletion.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length + capture.operations.length, before, "the destructive action was submitted without a confirmation");
  const warning = await visibleText(deletion.locator(".danger-note"));
  assert.match(warning, /namespace|irreversible|storage|delet/i, "the destructive action shows no warning before it is confirmed");
  const confirmField = deletion.locator("#profile-delete-confirm");
  const reason = deletion.locator("#profile-delete-reason");
  assert.ok(await confirmField.count(), "the destructive action does not ask for an exact name confirmation");
  assert.ok(await reason.count(), "the destructive action does not ask for a reason");
  assert.equal(await deletion.locator("#profile-delete-name").inputValue(), PROFILE_ALPHA.name, "the destructive action does not name its target");
  // A wrong confirmation is refused before any request is made.
  await confirmField.fill(PROFILE_ALPHA.name + "-wrong");
  await reason.fill("Synthetic browser acceptance reason");
  await deletion.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "a wrong confirmation name still produced a plan request");
  assert.match(await profileMessage(deletion), /does not match|confirm/i, "the console does not report the wrong confirmation name");
  // The exact confirmation is accepted and produces one typed request.
  await confirmField.fill(PROFILE_ALPHA.name);
  await reason.fill("Synthetic browser acceptance reason");
  await deletion.locator('button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length, before + 1, "the confirmed destructive action did not produce exactly one plan request");
  const plan = capture.plans[capture.plans.length - 1].body;
  assert.equal(plan.kind, "profile.delete", "the destructive plan request does not use the typed profile.delete kind");
  assert.equal(plan.name, PROFILE_ALPHA.name, "the destructive plan request does not name the Profile");
  assert.equal(plan.confirmName, PROFILE_ALPHA.name, "the destructive plan request does not carry the exact confirmation name");
  assert.equal(plan.reason, "Synthetic browser acceptance reason", "the destructive plan request does not carry the reason as typed");
  assert.equal(plan.namespace, undefined, "the destructive plan request carries a namespace");
  assert.equal(plan.owner, undefined, "the destructive plan request carries an owner");
  const preview = await dialog.innerText();
  assert.match(preview, /irreversible|storage|namespace|warning|destructive|delete/i, "the destructive plan preview shows no warning");
  assert.equal(capture.operations.length, 0, "the destructive action was submitted before the plan was reviewed");
}

// The namespace summary links every observed Notebook, claim and Pod to the
// scoped page that lists it. The generated link carries the kind, the name
// and the namespace scope, so a resource is never addressed by its name
// alone.
async function checkNamespaceScopedLinks(context) {
  const { page } = context;
  await gotoTable(page, context.base, "/namespaces", "namespaces");
  const row = await rowFor(page, "namespaces", NAMESPACE);
  const control = await findVisible(row, [
    { role: "button", name: NAMESPACE, exact: true },
    `button:has-text("${NAMESPACE}")`
  ], `the ${NAMESPACE} namespace control`);
  await control.click();
  const drawer = page.locator("#namespace-summary-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  for (const resource of [NOTEBOOK, CLAIM, POD]) {
    const link = await findVisible(drawer, [
      { role: "link", name: new RegExp(`${resource.kind} ${resource.name}`) },
      `a[href*="resource=${resource.name}"]`
    ], `the ${resource.kind} ${resource.name} link`);
    const href = await link.getAttribute("href");
    assert.ok(href, `the ${resource.kind} ${resource.name} link has no target`);
    const values = [...new URL(href, "http://console.invalid").searchParams.values()].map(value => value.toLowerCase());
    assert.ok(values.includes(NAMESPACE.toLowerCase()), `the ${resource.kind} ${resource.name} link is not scoped to the namespace (href=${href})`);
    assert.ok(values.includes(resource.name.toLowerCase()), `the ${resource.kind} ${resource.name} link does not carry the resource name (href=${href})`);
    assert.ok(values.includes(resource.kind.toLowerCase()), `the ${resource.kind} ${resource.name} link does not carry the resource kind (href=${href})`);
  }
  // Following the link opens the scoped page with the resource focused.
  const notebookLink = await findVisible(drawer, [
    { role: "link", name: new RegExp(`${NOTEBOOK.kind} ${NOTEBOOK.name}`) }
  ], `the ${NOTEBOOK.kind} ${NOTEBOOK.name} link`);
  await notebookLink.click();
  await page.waitForTimeout(400);
  assert.ok(page.url().includes("/notebooks"), `the namespace summary link did not open the notebooks page (url=${page.url()})`);
  assert.ok(page.url().includes(NAMESPACE), "the scoped page lost the namespace scope");
  await page.locator('[data-table="notebooks"] tbody tr').first().waitFor({ timeout: 10000 });
  const names = await page.locator('[data-table="notebooks"] tbody tr .resource-name > span').allTextContents();
  const focused = names.map(text => text.trim()).filter(Boolean);
  assert.ok(focused.length > 0, "the scoped page shows no rows");
  for (const shown of focused) {
    assert.equal(shown, NOTEBOOK.name, `the scoped page shows an unrelated resource (${shown})`);
  }
  const search = await page.locator('[data-table="notebooks"] [data-search]').inputValue();
  assert.equal(search, NOTEBOOK.name, "the scoped page does not show the resource focus in the visible filter");
}

// Resources of different kinds can share a name, and the same name can exist
// in two namespaces. A row is only identified by its kind and its name, never
// by its name alone.
async function checkCollidingResourceRows(context) {
  const { page } = context;
  await gotoTable(page, context.base, "/storage", "storage");
  const rows = page.locator('[data-table="storage"] tbody tr');
  const nameOf = async row => (await row.locator('.resource-name > span, .resource-name > button.link-button').first().innerText()).trim();
  const claimCell = rows.getByRole("cell", { name: "PersistentVolumeClaim", exact: true }).first();
  const volumeCell = rows.getByRole("cell", { name: "PersistentVolume", exact: true }).first();
  assert.ok(await volumeCell.count(), "the storage table does not show the PersistentVolume row");
  assert.ok(await claimCell.count(), "the storage table does not show the PersistentVolumeClaim row");
  const claimRow = claimCell.locator("xpath=ancestor::tr");
  const volumeRow = volumeCell.locator("xpath=ancestor::tr");
  assert.equal(await nameOf(claimRow), "data", "the colliding claim row does not show the name data");
  assert.equal(await nameOf(volumeRow), "data", "the colliding volume row does not show the name data");
  const label = await claimRow.locator("button.link-button").first().getAttribute("aria-label");
  if (label) assert.match(label, /PersistentVolumeClaim/, "the colliding claim name does not name its kind");
  // The same Pod name in two namespaces is two rows, not one.
  await gotoTable(page, context.base, "/pods", "pods");
  const podRows = page.locator('[data-table="pods"] tbody tr');
  const podNames = await podRows.locator('.resource-name > span, .resource-name > button.link-button').allTextContents();
  const podMeta = await podRows.locator('.resource-name small').allTextContents();
  const shared = [];
  for (let index = 0; index < podNames.length; index++) {
    if (podNames[index].trim() !== "shared-worker") continue;
    shared.push(podMeta[index].split("·")[1].trim());
  }
  assert.equal(shared.length, 2, `the pods table does not show both Pods named shared-worker (${shared.join(", ")})`);
  assert.deepEqual(shared.sort(), ["team-alpha", "team-beta"], "the Pods named shared-worker are not distinguished by their namespace");
}

// The observed details of a Pod, Node, Notebook and volume open the right
// hand drawer instead of an inline disclosure.
async function checkResourceDetailsDrawer(context) {
  const { page } = context;
  const cases = [
    { route: "/pods", table: "pods", resource: POD },
    { route: "/nodes", table: "nodes", resource: NODE },
    { route: "/notebooks", table: "notebooks", resource: NOTEBOOK },
    { route: "/storage", table: "storage", resource: CLAIM }
  ];
  for (const testCase of cases) {
    await gotoTable(page, context.base, testCase.route, testCase.table);
    const row = await rowFor(page, testCase.table, testCase.resource.name);
    const details = row.locator(`button[aria-label="Details for ${testCase.resource.kind} ${testCase.resource.name}"]`);
    const control = (await details.count()) ? details.first() : await findVisible(row, [
      { role: "button", name: /^details/i }
    ], `the details control of the ${testCase.resource.name} row`);
    await control.click();
    const drawer = page.locator("#resource-details-drawer");
    await drawer.waitFor({ state: "visible", timeout: 10000 });
    const title = (await drawer.locator("#resource-details-title").innerText()).trim();
    assert.equal(title, testCase.resource.name, "the details drawer does not name the observed resource");
    const text = await visibleText(drawer);
    assert.ok(text.includes(testCase.resource.detail), `the details drawer does not show the projected details of ${testCase.resource.name}`);
    await assertRightAligned(page, drawer, "the resource details drawer");
    await page.keyboard.press("Escape");
    await drawer.waitFor({ state: "hidden", timeout: 5000 });
  }
  // A storage name is the visible handle of a claim or a volume.
  await gotoTable(page, context.base, "/storage", "storage");
  const claimRow = await rowFor(page, "storage", CLAIM.name);
  const nameControl = await findVisible(claimRow, [
    { role: "button", name: CLAIM.name, exact: true },
    `button[aria-label="Details for ${CLAIM.kind} ${CLAIM.name}"]`
  ], "the storage name control");
  await nameControl.click();
  const drawer = page.locator("#resource-details-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  assert.ok((await visibleText(drawer)).includes(CLAIM.detail), "the storage name does not open the observed details drawer");
  await assertRightAligned(page, drawer, "the resource details drawer");
}

// The configuration precondition of a Profile is an internal request guard:
// it is never rendered and never searchable.
async function checkDigestNotExposed(context) {
  const { page } = context;
  const observed = await observedProfile(context.base, PROFILE_ALPHA.name);
  const digest = (observed.details || {})["configDigest"];
  assert.ok(digest, "the observed Profile carries no configuration digest");
  await gotoTable(page, context.base, "/profiles", "profiles");
  // The precondition is never rendered with the observed Profile.
  const table = page.locator('[data-table="profiles"]');
  assert.ok(!(await visibleText(table)).includes(digest), "the configuration precondition is rendered");
  // The observed details drawer hides it as well.
  const row = await rowFor(page, "profiles", PROFILE_ALPHA.name);
  const details = row.locator('button[aria-label="Details for Profile team-alpha"]');
  const control = (await details.count()) ? details.first() : await findVisible(row, [
    { role: "button", name: /^details/i }
  ], "the details control of the Profile row");
  await control.click();
  const drawer = page.locator("#resource-details-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  assert.ok(!(await visibleText(drawer)).includes(digest), "the configuration precondition is rendered in the details drawer");
  await page.keyboard.press("Escape");
  await drawer.waitFor({ state: "hidden", timeout: 5000 });
  // It is not part of the searchable observation either.
  const search = table.locator("[data-search]");
  await search.fill(digest);
  await page.waitForTimeout(300);
  const rows = await page.locator('[data-table="profiles"] tbody tr').count();
  assert.equal(rows, 0, "the configuration precondition is searchable");
}

// Every table offers a clear control that resets the search text and the
// namespace scope.
async function checkClearFilters(context) {
  const { page } = context;
  await gotoTable(page, context.base, "/pods", "pods");
  const table = page.locator('[data-table="pods"]');
  const search = table.locator("[data-search]");
  const scope = table.locator("[data-namespace]");
  assert.ok(await scope.count(), "the pods table has no namespace scope control");
  await search.fill("team");
  await scope.selectOption(NAMESPACE);
  await page.waitForTimeout(300);
  const clear = await findVisible(table, [
    { text: "Clear filters", tag: "button" },
    '[data-clear-filters]'
  ], "the clear filters control");
  assert.equal(await clear.isDisabled().catch(() => false), false, "the clear control stays disabled while a filter is applied");
  await clear.click();
  await page.waitForTimeout(400);
  assert.equal(await search.inputValue(), "", "clearing the filters does not empty the search box");
  assert.equal(await scope.inputValue(), "", "clearing the filters does not reset the namespace scope");
  assert.ok(!page.url().includes("namespace="), "clearing the filters keeps the namespace scope in the URL");
}

// The sidebar keeps its copyright line.
async function checkCopyright(context) {
  const { page } = context;
  await page.goto(context.base + "/", { waitUntil: "domcontentloaded" });
  const sidebar = page.locator(".sidebar");
  const text = await visibleText(sidebar);
  assert.match(text, /©\s*2026\s+Murat Shagirov/, "the sidebar lost its copyright line");
  const link = sidebar.locator('a[href*="github.com"]');
  assert.ok(await link.count(), "the sidebar lost its source link");
}

// Every page renders without a horizontal overflow and without an
// uncaught JavaScript error, on a phone and on a desktop viewport.
async function checkResponsive(context) {
  const { page, errors } = context;
  const routes = ["/", "/profiles", "/namespaces", "/storage", "/notebooks", "/pods", "/nodes"];
  const results = [];
  for (const viewport of [MOBILE, DESKTOP]) {
    await page.setViewportSize(viewport);
    for (const route of routes) {
      await page.goto(context.base + route, { waitUntil: "domcontentloaded" });
      await page.locator("#page-title").waitFor({ timeout: 10000 });
      await page.waitForTimeout(300);
      const overflow = await page.evaluate(() => ({
        document: document.documentElement.scrollWidth - document.documentElement.clientWidth,
        body: document.body.scrollWidth - document.documentElement.clientWidth
      }));
      results.push(`${viewport.width}px ${route}: overflow ${overflow.document}/${overflow.body}px`);
      assert.ok(overflow.document <= 1, `${route} overflows the ${viewport.width}px viewport by ${overflow.document}px`);
      assert.ok(overflow.body <= 1, `${route} overflows the ${viewport.width}px viewport by ${overflow.body}px`);
    }
  }
  console.log("      measured overflow: " + results.join(", "));
  assert.deepEqual(errors, [], "the console raised JavaScript errors: " + errors.join(" | "));
}

// --------------------------------------------------------------------- runner

(async () => {
  const playwright = loadPlaywright();
  if (!playwright) {
    console.log("SKIP profile_browser_optional: no Playwright module is installed");
    return;
  }
  const chrome = process.env.CHROME_BIN || DEFAULT_CHROME;

  let base = process.env.DASHBOARD_BASE_URL;
  let gatedBase = base;
  const fixtures = [];
  let artifactDir = "";
  if (!base) {
    // A fixture that cannot be built or started is a failure. A missing
    // asset is never masked by a skip.
    let artifact = "";
    try {
      const built = await buildFixture();
      artifact = built.artifact;
      artifactDir = built.artifactDir;
      const fixture = await startFixture({ artifact, writes: true, deletion: true, port: FIXTURE_PORT });
      fixtures.push(fixture);
      base = fixture.base;
      const gated = await startFixture({ artifact, writes: true, deletion: false, port: GATED_FIXTURE_PORT });
      fixtures.push(gated);
      gatedBase = gated.base;
    } catch (error) {
      console.error("FAIL profile_browser_optional: the synthetic fixture could not be built or started (" + error.message + ")");
      for (const fixture of fixtures) fixture.stop();
      if (artifactDir) fs.rmSync(artifactDir, { recursive: true, force: true });
      process.exitCode = 1;
      return;
    }
  }

  let browser;
  try { browser = await playwright.chromium.launch({ executablePath: chrome, headless: true }); }
  catch (error) {
    console.log("SKIP profile_browser_optional: Chrome could not be launched (" + error.message + ")");
    for (const fixture of fixtures) fixture.stop();
    if (artifactDir) fs.rmSync(artifactDir, { recursive: true, force: true });
    return;
  }

  const failures = [];
  const passed = [];
  // A rejected promise that nobody awaits is a failure too. It is collected
  // instead of terminating the runner, so the report stays complete.
  const unhandled = [];
  process.on("unhandledRejection", reason => {
    unhandled.push(String((reason && reason.message) || reason));
  });
  const run = async (title, fn, options = {}) => {
    console.log("RUN  profile_browser_optional: " + title);
    const context = await browser.newContext({ viewport: options.viewport || DESKTOP });
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    const errors = [];
    page.on("pageerror", error => errors.push(String((error && error.message) || error)));
    const capture = attachCapture(page);
    try {
      await fn({ page, capture, errors, base: options.base || base, gatedBase });
      passed.push(title);
    } catch (error) {
      failures.push(title + ": " + (error && error.message ? error.message : String(error)));
    } finally {
      await context.close().catch(() => {});
    }
  };

  try {
    await run("rendered HTML uses the Config projection and the identity", checkRenderedProjection);
    await run("invalid Profile names are refused with a visible reason", checkInvalidProfileNames);
    await run("a dotted or underscored name offers a hyphenated suggestion", checkProfileNameSuggestion);
    await run("an existing Profile name is refused", checkDuplicateProfileRefused);
    await run("an existing namespace name is reserved", checkReservedNamespaceRefused);
    await run("a valid Profile name produces one typed plan and operation", checkValidProfileCreation);
    await run("the Profile summary shows the apiVersion, owner and quotas", checkProfileSummaryContents);
    await run("the Profile update form is prefilled with the owner and quotas", checkProfileUpdatePrefill);
    await run("an update carries the precondition and the unchanged owner", checkProfileUpdateRoundTrip);
    await run("a plan that has expired is never submitted", checkExpiredPlanRefused);
    await run("an observed quota of zero never blocks an update", checkZeroQuotaUpdate);
    await run("the destructive Profile action is gated by the deletion capability", checkDeletionCapabilityGated, { base: gatedBase });
    await run("the destructive Profile action needs a confirmation and shows a warning", checkDestructiveConfirmation);
    await run("the namespace summary links are scoped and filtered by name", checkNamespaceScopedLinks);
    await run("resources of different kinds can share a name", checkCollidingResourceRows);
    await run("the observed details open the right hand drawer", checkResourceDetailsDrawer);
    await run("the configuration precondition is never rendered or searchable", checkDigestNotExposed);
    await run("clear filters resets the search and the namespace scope", checkClearFilters);
    await run("the sidebar keeps its copyright", checkCopyright);
    await run("every page renders without overflow and without JavaScript errors", checkResponsive, { viewport: MOBILE });
  } finally {
    await browser.close();
    for (const fixture of fixtures) fixture.stop();
    if (artifactDir) fs.rmSync(artifactDir, { recursive: true, force: true });
  }

  for (const title of passed) console.log("PASS profile_browser_optional: " + title);
  for (const failure of failures) console.log("FAIL profile_browser_optional: " + failure);
  for (const reason of unhandled) console.log("FAIL profile_browser_optional: unhandled rejection: " + reason);
  const total = failures.length + unhandled.length;
  if (total) {
    console.error(`FAIL profile_browser_optional: ${total} of ${passed.length + total} checks failed`);
    process.exitCode = 1;
    return;
  }
  console.log(`PASS profile_browser_optional: ${passed.length} profile workflow checks passed`);
})().catch(error => { console.error(error); process.exitCode = 1; });
