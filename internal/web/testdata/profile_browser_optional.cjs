"use strict";
// Optional real-browser acceptance runner for the Profile and storage
// workflows.
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
//   PROFILE_READONLY_FIXTURE_PORT  loopback port of the fixture without writes
//   PROFILE_NOCONTRACT_FIXTURE_PORT loopback port of the fixture without the
//                           confirmed Notebook controller contract
//   PROFILE_STORAGE_GATED_FIXTURE_PORT loopback port of the fixture with both
//                           storage opt-ins disabled
//
// The fixture also accepts KF_ADMIN_STORAGE_EDITING and
// KF_ADMIN_STORAGE_DELETION; the runner starts the storage-gated instance with
// both set to 0.
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
const READONLY_FIXTURE_PORT = process.env.PROFILE_READONLY_FIXTURE_PORT || "18770";
const NOCONTRACT_FIXTURE_PORT = process.env.PROFILE_NOCONTRACT_FIXTURE_PORT || "18771";
const STORAGE_GATED_FIXTURE_PORT = process.env.PROFILE_STORAGE_GATED_FIXTURE_PORT || "18772";

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
// A Profile whose quota declares the Kubernetes aliases and a device key of
// its own. The observed keys are edited under their own names.
const PROFILE_LEGACY = {
  name: "team-legacy",
  owner: "legacy.owner@example.org",
  apiVersion: "kubeflow.org/v1",
  ownerKind: "User",
  quotas: [
    { key: "cpu", value: "128" },
    { key: "memory", value: "256Gi" },
    { key: "nvidia.com/gpu", value: "4" },
    { key: "persistentvolumeclaims", value: "20" },
    { key: "requests.storage", value: "500Gi" }
  ],
  unmanaged: { key: "requests.ephemeral-storage", value: "5Gi" },
  counted: { key: "count/jobs.batch", value: "5" }
};
const RESERVED_NAMESPACE = "team-delta";
const NEW_PROFILE = "team-gamma";
const EXPIRED_PLAN_PROFILE = "team-epsilon";
const NEW_PROFILE_OWNER = "Gamma.Owner@Example.org";
const NAMESPACE = "team-alpha";
const NOTEBOOK = { kind: "Notebook", name: "team-alpha-notebook-0", namespace: NAMESPACE, detail: "node: node-a" };
const NOTEBOOK_WORK = { kind: "Notebook", name: "work", namespace: NAMESPACE, detail: "image.notebook: kubeflownotebookswg/jupyter:1.9.0" };
const STATEFULSET_WORK = { kind: "StatefulSet", name: "work", namespace: NAMESPACE, detail: "replicas: 1" };
const POD_WORK = { kind: "Pod", name: "work", namespace: NAMESPACE, detail: "node: node-a" };
const BETA_WORK = { namespace: "team-beta" };
const CLAIM = { kind: "PersistentVolumeClaim", name: "team-alpha-claim", namespace: NAMESPACE, detail: "storageClass: standard" };
// The same name in two kinds is a different resource. The fixture exposes a
// Bound claim and volume named data, plus an unclaimed Available volume.
const STORAGE_CLAIM = { kind: "PersistentVolumeClaim", name: "data", namespace: NAMESPACE, uid: "uid-pvc-team-alpha-data" };
const STORAGE_VOLUME = { kind: "PersistentVolume", name: "data", namespace: "", uid: "uid-pv-data" };
const STORAGE_AVAILABLE = { kind: "PersistentVolume", name: "spare-volume", namespace: "", uid: "uid-pv-spare-volume" };
const POD = { kind: "Pod", name: "team-alpha-notebook-0-0", namespace: NAMESPACE, detail: "node: node-a" };
const NODE = { kind: "Node", name: "node-a", detail: "version: v1.31.4" };
// The synthetic fixture observes three Running Pods on node-a, across two
// namespaces: work, team-alpha-notebook-0-0 and shared-worker.
const NODE_RUNNING_PODS = 3;
// The typed synthetic Notebook detail model. The management drawer has to
// prefill from this observed model, not from a fresh guess.
const NOTEBOOK_MODEL = {
  kind: "Notebook",
  name: "team-alpha-notebook-0",
  namespace: NAMESPACE,
  container: "notebook",
  image: "kubeflownotebookswg/jupyter:1.9.0",
  cpu: "4",
  memory: "8Gi",
  claim: "team-alpha-claim",
  storageClass: "standard"
};

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
async function startFixture({ artifact, writes = true, deletion = true, contract = true, storageEditing = true, storageDeletion = true, port } = {}) {
  const launch = async requestedPort => {
    const child = spawn(artifact, [], {
      env: {
        ...process.env,
        KF_ADMIN_REPO_ROOT: process.env.KF_ADMIN_REPO_ROOT || REPO_ROOT,
        KF_ADMIN_WRITES: writes ? "1" : "0",
        KF_ADMIN_PROFILE_DELETION: deletion ? "1" : "0",
        KF_ADMIN_NOTEBOOK_CONTRACT: contract ? "1" : "0",
        KF_ADMIN_STORAGE_EDITING: storageEditing ? "1" : "0",
        KF_ADMIN_STORAGE_DELETION: storageDeletion ? "1" : "0",
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
  // A Profile name opens exactly one Profile summary. It must not be a
  // generic RESOURCE DETAILS drawer.
  assert.equal(await page.locator("#resource-details-drawer").isVisible().catch(() => false), false,
    "the Profile name opened the generic RESOURCE DETAILS drawer instead of the Profile summary");
  assert.equal(await drawer.locator("#profile-summary-title").innerText(), name, "the Profile summary does not name the selected Profile");
  return drawer;
}

// openProfileEdit opens the summary of one Profile and follows its Edit
// action. The summary is the only Profile entry point; there is no generic
// per-row details control.
async function openProfileEdit(page, base, name) {
  const summary = await openProfileSummary(page, base, name);
  const edit = await findVisible(summary, [
    { role: "button", name: /^edit profile$/i },
    { role: "button", name: /^edit/i },
    { role: "button", name: /edit/i }
  ], "the Profile edit action in the summary");
  await edit.click();
  const form = page.locator("#profile-drawer");
  await form.waitFor({ state: "visible", timeout: 10000 });
  await page.waitForTimeout(200);
  return form;
}

// assertNoDetailsControls proves that a table has no separate Details button
// and no Details column. A resource name is the visible details handle.
async function assertNoDetailsControls(table, description) {
  const buttons = await table.locator("button").evaluateAll(nodes =>
    nodes.filter(node => (node.textContent || "").trim() === "Details").map(node => node.outerHTML));
  assert.deepEqual(buttons, [], description + " still offers a separate Details button: " + buttons.join(" | "));
  const headers = await table.locator("thead th").allTextContents();
  assert.ok(!headers.some(header => header.trim().toLowerCase() === "details"),
    description + " still offers a separate Details column (" + headers.join(", ") + ")");
}

// resourceNameText reads the visible name of a row, whether the name is plain
// text or the button that opens the details drawer.
async function resourceNameText(row) {
  return (await row.locator(".resource-name > span, .resource-name > button.link-button").first().innerText()).trim();
}

// rowKind reads the observed kind a row shows under its name.
async function rowKind(row) {
  const meta = (await row.locator(".resource-name small").first().innerText().catch(() => "")).trim();
  return meta.split("·")[0].trim();
}

// clickResourceName opens the details drawer through the visible resource
// name. A separate Details button is never required and is never used.
async function clickResourceName(page, tableKey, name) {
  const row = await rowFor(page, tableKey, name);
  const control = await findVisible(row, [
    { role: "button", name, exact: true },
    `.resource-name > button.link-button`,
    `button[aria-label*="${name}"]`
  ], "the " + name + " resource name control");
  await control.click();
  const drawer = page.locator("#resource-details-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  return drawer;
}

// openNotebookDetails opens the Notebook details drawer and returns both the
// drawer and the typed API model the console fetched.
async function openNotebookDetails(page, base, notebook = NOTEBOOK) {
  const route = notebook.namespace ? "/notebooks?namespace=" + encodeURIComponent(notebook.namespace) : "/notebooks";
  await gotoTable(page, base, route, "notebooks");
  const drawer = await clickResourceName(page, "notebooks", notebook.name);
  assert.equal((await drawer.locator("#resource-details-title").innerText()).trim(), notebook.name,
    "the Notebook details drawer does not name the observed Notebook");
  return drawer;
}

// openNotebookManagement opens one exact Notebook's details and follows its
// Manage Notebook action into the management drawer.
async function openNotebookManagement(page, base, notebook = NOTEBOOK) {
  const details = await openNotebookDetails(page, base, notebook);
  const manage = await notebookManageControl(details);
  await manage.click();
  const form = await findVisible(page, [
    "#notebook-drawer",
    "#notebook-management-drawer",
    '[data-drawer="notebook-management"]'
  ], "the Notebook management drawer");
  await form.locator("form").first().waitFor({ timeout: 10000 });
  return { details, manage, form };
}

// notebookManageControl resolves the Manage Notebook action of the open
// details drawer. A gated console may not render it at all.
async function notebookManageControl(drawer, options = {}) {
  return findVisible(drawer, [
    "#manage-notebook",
    "[data-manage-notebook]",
    { role: "button", name: /^manage notebook$/i },
    { role: "button", name: /manage.*notebook/i }
  ], "the Manage Notebook action", options);
}

// assertNotebookManageFocus proves that closing the management drawer returns
// the focus to the visible control of the very same Notebook. When the console
// hands the details drawer over to management, the control inside the closed
// details drawer may never take the focus back.
async function assertNotebookManageFocus(page, details, manage, key) {
  const focus = await page.evaluate(expected => {
    const active = document.activeElement;
    const owner = active && active.closest ? active.closest("[data-resource-key]") : null;
    return {
      id: active?.id || "",
      text: String(active?.textContent || "").trim().slice(0, 80),
      key: (active?.dataset && active.dataset.resourceKey) || (owner && owner.dataset && owner.dataset.resourceKey) || "",
      hidden: !!(active && active.closest && active.closest("[hidden]")),
      visible: !!(active && typeof active.getClientRects === "function" && active.getClientRects().length > 0),
      expected
    };
  }, key);
  assert.equal(focus.hidden, false, "the focus went to a control inside a closed drawer");
  assert.equal(focus.visible, true, "the focus did not return to a visible control");
  if (await details.isVisible().catch(() => false)) {
    assert.equal(await manage.evaluate(node => node === document.activeElement), true,
      "closing Notebook management did not return the focus to the Manage Notebook control");
    return;
  }
  assert.equal(focus.key, key,
    "closing Notebook management did not return the focus to the visible control of the exact Notebook (" + JSON.stringify(focus) + ")");
  assert.notEqual(focus.text, "Manage Notebook",
    "closing Notebook management focused the Manage control inside the closed details drawer");
}

// openStorageDetails opens the observed details of one exact storage resource.
// A claim, a volume and a storage class can share a name, so the row is
// resolved by its kind, namespace and name rather than by its name alone.
async function openStorageDetails(page, base, resource) {
  const route = resource.namespace ? "/storage?namespace=" + encodeURIComponent(resource.namespace) : "/storage";
  await gotoTable(page, base, route, "storage");
  const rows = page.locator('[data-table="storage"] tbody tr');
  let target = null;
  for (let index = 0; index < await rows.count(); index++) {
    const row = rows.nth(index);
    if ((await rowKind(row)) !== resource.kind) continue;
    if ((await resourceNameText(row)) !== resource.name) continue;
    target = row;
    break;
  }
  assert.ok(target, `the storage table does not show ${resource.kind} ${resource.name}`);
  const control = target.locator("button.link-button").first();
  await control.click();
  const drawer = page.locator("#resource-details-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  return drawer;
}

// openStorageEdit follows the visible Edit action and waits until the current
// restricted document has settled into the editor.
async function openStorageEdit(page, base, resource) {
  const details = await openStorageDetails(page, base, resource);
  const edit = details.locator("button").filter({ hasText: /^Edit$/ }).first();
  await edit.click();
  const drawer = page.locator("#storage-edit-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  await page.waitForFunction(() => {
    const textarea = document.querySelector("#storage-edit-yaml");
    return !!(textarea && !textarea.disabled);
  }, null, { timeout: 10000 });
  return { details, edit, drawer };
}

// openStorageDelete follows the visible Delete action and waits until the
// current document has bound the target UID and resourceVersion.
async function openStorageDelete(page, base, resource) {
  const details = await openStorageDetails(page, base, resource);
  const remove = details.locator("button").filter({ hasText: /^Delete$/ }).first();
  await remove.click();
  const drawer = page.locator("#storage-delete-drawer");
  await drawer.waitFor({ state: "visible", timeout: 10000 });
  await page.waitForFunction(() => {
    const uid = document.querySelector("#storage-delete-target-uid");
    const rv = document.querySelector("#storage-delete-target-rv");
    return !!(uid && uid.value && rv && rv.value);
  }, null, { timeout: 10000 });
  return { details, remove, drawer };
}


async function notebookField(form, candidates) {
  for (const selector of candidates) {
    const field = form.locator(selector);
    if (await field.count()) return field.first();
  }
  throw new Error("the Notebook management drawer has none of: " + candidates.join(", "));
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
  for (const name of ["notebook-writes", "static-storage", "pvc-expansion", "pod-restarts", "node-cordon", "profile-deletion", "storage-editing", "storage-deletion"]) {
    const value = await page.locator(`meta[name="${name}"]`).getAttribute("content");
    assert.ok(value !== null, `the ${name} capability is not projected`);
  }
  const writes = await page.locator("body").getAttribute("data-writes");
  if (state) {
    assert.equal(writes, String(state.features.writesEnabled), "the write capability does not match the configuration");
    assert.equal(await page.locator('meta[name="profile-deletion"]').getAttribute("content"), String(state.features.profileDeletion),
      "the deletion capability does not match the configuration");
    assert.equal(await page.locator('meta[name="notebook-writes"]').getAttribute("content"), String(state.features.notebookContract),
      "the Notebook controller contract capability does not match the configuration");
    assert.equal(await page.locator('meta[name="storage-editing"]').getAttribute("content"), String(state.features.storageEditing),
      "the storage editing capability does not match the configuration");
    assert.equal(await page.locator('meta[name="storage-deletion"]').getAttribute("content"), String(state.features.storageDeletion),
      "the storage deletion capability does not match the configuration");
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
  assert.deepEqual(names, ["team-alpha", "team-beta", "team-legacy"], "the fixture inventory changed after a plan and operation");
}

// The Profile name opens one Profile summary. The summary carries the whole
// observed Profile contract: apiVersion, owner, every hard quota key, state,
// creation time and safe metadata, plus the Edit and Delete actions. The
// Profiles table has no generic Details button or column.
async function checkProfileSummaryContents(context) {
  const { page } = context;
  await gotoTable(page, context.base, "/profiles", "profiles");
  const table = page.locator('[data-table="profiles"]');
  await assertNoDetailsControls(table, "the Profiles table");
  const drawer = await openProfileSummary(page, context.base, PROFILE_ALPHA.name);
  const text = await visibleText(drawer);
  assert.ok(text.includes(PROFILE_ALPHA.apiVersion), `the Profile summary does not show the apiVersion ${PROFILE_ALPHA.apiVersion}`);
  assert.ok(text.includes(PROFILE_ALPHA.owner), `the Profile summary does not show the owner ${PROFILE_ALPHA.owner}`);
  assert.ok(text.includes(PROFILE_ALPHA.ownerKind), "the Profile summary does not show the observed owner kind");
  assert.ok(text.includes("Active"), "the Profile summary does not show the observed state");
  assert.match(text, /created/i, "the Profile summary does not show when the Profile was created");
  assert.match(text, /resource version/i, "the Profile summary does not show the safe observed metadata heading");
  assert.ok(text.includes("4242"), "the Profile summary does not show the observed safe resource metadata");
  for (const quota of PROFILE_ALPHA.quotas) {
    const pair = await quotaPair(drawer, quota.key, quota.value);
    assert.ok(pair.ok, `the Profile summary does not show ${quota.key} = ${quota.value} (${pair.how})`);
  }
  const unmanaged = await quotaPair(drawer, PROFILE_ALPHA.unmanaged.key, PROFILE_ALPHA.unmanaged.value);
  assert.ok(unmanaged.ok, `the Profile summary does not show ${PROFILE_ALPHA.unmanaged.key} = ${PROFILE_ALPHA.unmanaged.value} (${unmanaged.how})`);
  // The management actions live in the one summary.
  await findVisible(drawer, [
    { role: "button", name: /^edit profile$/i },
    { role: "button", name: /edit/i }
  ], "the Profile edit action");
  await findVisible(drawer, [
    { role: "button", name: /delete profile/i },
    { role: "button", name: /^delete/i }
  ], "the Profile delete action");
  await page.keyboard.press("Escape");
  await drawer.waitFor({ state: "hidden", timeout: 5000 });

  // A legacy Profile keeps its declared aliases and its raw device key. The
  // raw key is visibly explained: standard Kubernetes does not enforce a bare
  // device key as a device quota. No key is renamed to a canonical requests.*
  // counterpart.
  const legacy = await openProfileSummary(page, context.base, PROFILE_LEGACY.name);
  const legacyText = await visibleText(legacy);
  for (const quota of PROFILE_LEGACY.quotas) {
    const pair = await quotaPair(legacy, quota.key, quota.value);
    assert.ok(pair.ok, `the legacy Profile summary does not show ${quota.key} = ${quota.value} (${pair.how})`);
  }
  for (const extra of [PROFILE_LEGACY.unmanaged, PROFILE_LEGACY.counted]) {
    const pair = await quotaPair(legacy, extra.key, extra.value);
    assert.ok(pair.ok, `the legacy Profile summary does not show the unmanaged ${extra.key} = ${extra.value} (${pair.how})`);
  }
  assert.match(legacyText, /bare|not enforced|does not enforce|not a device quota|scheduling resource/i,
    "the legacy Profile summary does not warn that a bare device key is not enforced as a device quota by standard Kubernetes");
  // An enforced object-count quota is not a raw device key: the warning names
  // only the bare GPU key and never all unmanaged keys.
  const warning = (await visibleText(legacy.locator('p.warning[role="note"]'))).trim();
  assert.ok(warning.includes("nvidia.com/gpu"), "the bare device warning does not name the observed GPU key");
  assert.ok(!warning.includes(PROFILE_LEGACY.counted.key), "an object-count quota is reported as a raw device key");
  for (const canonical of ["requests.nvidia.com/gpu", "requests.cpu", "requests.memory"]) {
    assert.ok(!legacyText.includes(canonical), "the legacy Profile summary silently shows or migrates to the canonical key " + canonical);
  }
}

// The Profile update form prefills the observed owner as a read-only value,
// the observed quota and the configuration precondition.
async function checkProfileUpdatePrefill(context) {
  const { page } = context;
  const observed = await observedProfile(context.base, PROFILE_ALPHA.name);
  assert.ok(observed, "the observed Profile is missing from the inventory");
  const digest = (observed.details || {})["configDigest"];
  assert.ok(digest, "the observed Profile carries no configuration digest");
  const form = await openProfileEdit(page, context.base, PROFILE_ALPHA.name);

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

// An existing Profile may declare quota keys this console has no static field
// for: the cpu and memory aliases and a device key of its own. They are edited
// under their own names. The declared key set never changes and no canonical
// requests.* key is added beside them.
async function checkLegacyQuotaKeyUpdate(context) {
  const { page, capture } = context;
  const observed = await observedProfile(context.base, PROFILE_LEGACY.name);
  assert.ok(observed, "the legacy Profile is missing from the inventory");
  const digest = (observed.details || {})["configDigest"];
  assert.ok(digest, "the observed legacy Profile carries no configuration digest");
  const drawer = await openProfileEdit(page, context.base, PROFILE_LEGACY.name);
  assert.equal(await drawer.locator("#profile-kind").inputValue(), "profile.update",
    "the Profile summary Edit action did not open the update form");
  // The raw device warning stays visible before the plan is prepared.
  assert.match(await visibleText(drawer), /bare|normally not enforced|requests\.<device>/i,
    "the update form does not visibly warn that a bare device key is not enforced as a device quota");

  // Every observed key is editable, prefilled and carries its own name.
  for (const quota of PROFILE_LEGACY.quotas) {
    const field = quotaInput(drawer, quota.key);
    assert.ok(await field.count(), `the update form has no field for the observed quota ${quota.key}`);
    assert.equal(await field.evaluate(node => node.readOnly === true), false, `the observed quota ${quota.key} is read-only in the update form`);
    assert.equal(await field.inputValue(), quota.value, `the update form is not prefilled with the observed ${quota.key}`);
  }
  // Keys the console does not manage are shown, but never editable. An
  // object-count quota is preserved exactly like a storage-class quota.
  for (const observedExtra of [PROFILE_LEGACY.unmanaged, PROFILE_LEGACY.counted]) {
    const extra = extraQuotaInput(drawer, observedExtra.key);
    assert.ok(await extra.count(), `the update form does not show the unmanage quota key ${observedExtra.key}`);
    const extraState = await extra.evaluate(node => ({
      readOnly: node.readOnly === true,
      disabled: node.disabled === true,
      type: node.type,
      value: node.value
    }));
    assert.ok(extraState.readOnly || extraState.disabled || extraState.type === "hidden",
      "an unmanage quota key is editable in the update form");
    assert.equal(extraState.value, observedExtra.value, "the unmanage quota key does not keep its observed value");
  }

  // The visible warning names the bare device key only. An object-count quota
  // is unmanaged, not a device, and never raises the warning.
  const editWarning = (await visibleText(drawer.locator("#profile-observed-quota-fields .warning"))).trim();
  assert.ok(editWarning.includes("nvidia.com/gpu"), "the update form does not warn about the observed bare GPU key");
  assert.ok(!editWarning.includes(PROFILE_LEGACY.counted.key), "the update form warns about an object-count quota as if it were a device");

  // A device count is a whole number of devices: a fraction never reaches the
  // plan API.
  const before = capture.plans.length;
  await quotaInput(drawer, "nvidia.com/gpu").fill("0.5");
  await drawer.locator("#profile-reason").fill("Synthetic browser acceptance reason");
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(400);
  assert.equal(capture.plans.length, before, "a fractional device count reached the plan API");

  // Only the CPU alias changes. The request carries exactly the observed key
  // set: the aliases and the device key keep their own names.
  await quotaInput(drawer, "nvidia.com/gpu").fill("4");
  await quotaInput(drawer, "cpu").fill("96");
  await drawer.locator('button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length, before + 1, "the update did not produce exactly one plan request");
  const plan = capture.plans[capture.plans.length - 1].body;
  assert.ok(plan && typeof plan === "object", "the update plan request is not a JSON object");
  assert.equal(plan.kind, "profile.update", "the update request does not use the typed update kind");
  assert.equal(plan.name, PROFILE_LEGACY.name, "the update request does not name the Profile");
  assert.equal(plan.owner, PROFILE_LEGACY.owner, "the update request does not submit the unchanged owner");
  assert.equal(plan.baseConfigDigest, digest, "the update request does not carry the base configuration digest of the observed Profile");
  assert.ok(plan.quota && typeof plan.quota === "object", "the update request does not carry the quota values");
  assert.equal(plan.quota["cpu"], "96", "the changed alias is submitted under its own name");
  assert.equal(plan.quota["memory"], "256Gi", "the unchanged alias is submitted");
  assert.equal(plan.quota["nvidia.com/gpu"], "4", "the device key is submitted under its own name");
  assert.equal(plan.quota["persistentvolumeclaims"], "20", "the claim count is submitted");
  assert.equal(plan.quota["requests.storage"], "500Gi", "the storage request is submitted");
  assert.equal(plan.quota[PROFILE_LEGACY.unmanaged.key], PROFILE_LEGACY.unmanaged.value,
    "the unmanage quota key is returned unchanged");
  assert.equal(plan.quota[PROFILE_LEGACY.counted.key], PROFILE_LEGACY.counted.value,
    "the unmanaged object-count quota is returned unchanged");
  assert.deepEqual(Object.keys(plan.quota).sort(),
    ["cpu", "memory", "nvidia.com/gpu", "persistentvolumeclaims", "requests.ephemeral-storage", "requests.storage", "count/jobs.batch"].sort(),
    "the update carries exactly the observed key set");
  for (const key of ["requests.cpu", "requests.memory", "requests.nvidia.com/gpu", "limits.cpu", "limits.memory", "pods"]) {
    assert.equal(plan.quota[key], undefined, "the undeclared key " + key + " is never added");
  }
  // The validated difference names the changed key under its own name.
  const changesText = await visibleText(page.locator("#plan-changes"));
  assert.ok(changesText.includes("quota.cpu"), "the validated difference does not name the changed quota key");
  assert.ok(changesText.includes("96"), "the validated difference does not show the new quota value");
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
  const focusedRows = page.locator('[data-table="notebooks"] tbody tr');
  const focused = [];
  for (let index = 0; index < await focusedRows.count(); index++) {
    focused.push(await resourceNameText(focusedRows.nth(index)));
  }
  assert.ok(focused.length > 0, "the scoped page shows no rows");
  for (const shown of focused) {
    assert.equal(shown, NOTEBOOK.name, `the scoped page shows an unrelated resource (${shown})`);
  }
  const search = await page.locator('[data-table="notebooks"] [data-search]').inputValue();
  assert.equal(search, NOTEBOOK.name, "the scoped page does not show the resource focus in the visible filter");

  // A StatefulSet with the same name as an observed Notebook is its own
  // resource. Its generated link may stay on the Notebooks page or move to the
  // Overview, but it always carries kind, namespace and name. The runner
  // follows the link the console really generated.
  await gotoTable(page, context.base, "/namespaces", "namespaces");
  const statefulRow = await rowFor(page, "namespaces", NAMESPACE);
  const statefulOpener = await findVisible(statefulRow, [
    { role: "button", name: NAMESPACE, exact: true },
    `button:has-text("${NAMESPACE}")`
  ], "the " + NAMESPACE + " namespace control");
  await statefulOpener.click();
  const statefulDrawer = page.locator("#namespace-summary-drawer");
  await statefulDrawer.waitFor({ state: "visible", timeout: 10000 });
  const statefulLink = await findVisible(statefulDrawer, [
    { role: "link", name: new RegExp(`StatefulSet\\s+${STATEFULSET_WORK.name}`) },
    `a[href*="kind=StatefulSet"]`
  ], "the StatefulSet " + STATEFULSET_WORK.name + " link");
  const statefulHref = await statefulLink.getAttribute("href");
  const statefulUrl = new URL(statefulHref, "http://console.invalid");
  const statefulValues = [...statefulUrl.searchParams.values()].map(value => value.toLowerCase());
  assert.ok(statefulValues.includes("statefulset"), "the StatefulSet link does not carry its kind (href=" + statefulHref + ")");
  assert.ok(statefulValues.includes(NAMESPACE), "the StatefulSet link is not namespace scoped (href=" + statefulHref + ")");
  assert.ok(statefulValues.includes(STATEFULSET_WORK.name), "the StatefulSet link does not carry the resource name (href=" + statefulHref + ")");
  await statefulLink.click();
  await page.locator("#page-title").waitFor({ timeout: 10000 });
  const statefulPath = new URL(page.url()).pathname;
  if (statefulPath === "/notebooks") {
    // The Notebooks page only lists Notebook rows: a StatefulSet focus may
    // never bring a StatefulSet row back into it.
    const rows = page.locator('[data-table="notebooks"] tbody tr');
    for (let index = 0; index < await rows.count(); index++) {
      assert.equal(await rowKind(rows.nth(index)), "Notebook",
        "a StatefulSet link brought a non-Notebook row back into the Notebooks page");
    }
  } else {
    assert.ok(["/", "/overview"].includes(statefulPath),
      "the StatefulSet link opened an unrelated page (" + page.url() + ")");
  }

  // The Notebook link of the same collision stays a Notebook focus: kind,
  // namespace and name select exactly one Notebook row, never the StatefulSet
  // or the Pod that share the name.
  await gotoTable(page, context.base, "/namespaces", "namespaces");
  const notebookWorkRow = await rowFor(page, "namespaces", NAMESPACE);
  const notebookWorkOpener = await findVisible(notebookWorkRow, [
    { role: "button", name: NAMESPACE, exact: true },
    `button:has-text("${NAMESPACE}")`
  ], "the " + NAMESPACE + " namespace control");
  await notebookWorkOpener.click();
  const notebookWorkDrawer = page.locator("#namespace-summary-drawer");
  await notebookWorkDrawer.waitFor({ state: "visible", timeout: 10000 });
  const notebookWorkLink = await findVisible(notebookWorkDrawer, [
    { role: "link", name: new RegExp(`Notebook\\s+${NOTEBOOK_WORK.name}`) },
    `a[href*="kind=Notebook"]`
  ], "the Notebook " + NOTEBOOK_WORK.name + " link");
  const notebookWorkHref = await notebookWorkLink.getAttribute("href");
  const notebookWorkUrl = new URL(notebookWorkHref, "http://console.invalid");
  const notebookWorkValues = [...notebookWorkUrl.searchParams.values()].map(value => value.toLowerCase());
  assert.ok(notebookWorkValues.includes("notebook"), "the colliding Notebook link does not carry its kind (href=" + notebookWorkHref + ")");
  assert.ok(notebookWorkValues.includes(NAMESPACE), "the colliding Notebook link is not namespace scoped (href=" + notebookWorkHref + ")");
  assert.ok(notebookWorkValues.includes(NOTEBOOK_WORK.name), "the colliding Notebook link does not carry the resource name (href=" + notebookWorkHref + ")");
  await notebookWorkLink.click();
  await page.locator("#page-title").waitFor({ timeout: 10000 });
  assert.ok(new URL(page.url()).pathname === "/notebooks", "the Notebook link did not open the Notebooks page (" + page.url() + ")");
  const workRows = page.locator('[data-table="notebooks"] tbody tr');
  await workRows.first().waitFor({ timeout: 10000 });
  assert.equal(await workRows.count(), 1, "the colliding Notebook link did not select exactly one row");
  assert.equal(await resourceNameText(workRows.first()), NOTEBOOK_WORK.name, "the colliding Notebook link selected another resource");
  assert.equal(await rowKind(workRows.first()), "Notebook", "the colliding Notebook link selected a non-Notebook row");
  assert.equal(await page.locator('[data-table="notebooks"] [data-search]').inputValue(), NOTEBOOK_WORK.name,
    "the colliding Notebook link does not show its exact resource focus");
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

  // A Notebook, a StatefulSet and a Pod share the name "work" in each
  // namespace. The Notebooks page shows only the Notebook row, and a row is
  // only matched by its exact kind, namespace and name.
  for (const namespace of [NAMESPACE, BETA_WORK.namespace]) {
    await page.goto(context.base + "/notebooks?namespace=" + encodeURIComponent(namespace), { waitUntil: "domcontentloaded" });
    const table = page.locator('[data-table="notebooks"]');
    await table.locator("tbody tr").first().waitFor({ timeout: 10000 });
    await assertNoDetailsControls(table, "the Notebooks table");
    const scopedRows = table.locator("tbody tr");
    const work = [];
    for (let index = 0; index < await scopedRows.count(); index++) {
      const row = scopedRows.nth(index);
      assert.equal(await rowKind(row), "Notebook",
        "the Notebooks page lists a non-Notebook row (kind " + await rowKind(row) + ")");
      const shown = await resourceNameText(row);
      if (shown === NOTEBOOK_WORK.name) work.push(row);
    }
    assert.equal(work.length, 1,
      "the Notebooks page does not show exactly one Notebook row named " + NOTEBOOK_WORK.name + " in " + namespace);
    const meta = (await work[0].locator(".resource-name small").innerText()).trim();
    assert.ok(meta.includes(namespace), "the Notebook row named work does not carry its namespace " + namespace + " (" + meta + ")");
  }
  // A search without a namespace scope finds two Notebooks named "work", not
  // the six resources of three kinds that share the name.
  await gotoTable(page, context.base, "/notebooks", "notebooks");
  await page.locator('[data-table="notebooks"] [data-search]').fill(NOTEBOOK_WORK.name);
  await page.waitForTimeout(400);
  const workRows = page.locator('[data-table="notebooks"] tbody tr');
  const workNamespaces = [];
  for (let index = 0; index < await workRows.count(); index++) {
    const row = workRows.nth(index);
    assert.equal(await rowKind(row), "Notebook", "a Pod or StatefulSet named work is listed on the Notebooks page");
    if (await resourceNameText(row) !== NOTEBOOK_WORK.name) continue;
    const meta = (await row.locator(".resource-name small").innerText()).trim();
    workNamespaces.push(meta.split("·")[1].trim());
  }
  assert.equal(workNamespaces.length, 2, "the Notebooks page does not show both Notebooks named work (" + workNamespaces.join(", ") + ")");
  assert.deepEqual(workNamespaces.sort(), ["team-alpha", "team-beta"],
    "the Notebooks named work are not distinguished by their namespace");
  // The Pods and StatefulSet still exist on their own pages; only the
  // Notebooks page filters them out.
  await gotoTable(page, context.base, "/pods", "pods");
  const podWork = [];
  const podRowsWork = page.locator('[data-table="pods"] tbody tr');
  for (let index = 0; index < await podRowsWork.count(); index++) {
    if (await resourceNameText(podRowsWork.nth(index)) === POD_WORK.name) podWork.push(podRowsWork.nth(index));
  }
  assert.equal(podWork.length, 2, "the Pods page lost the Pods named work");
  // The visible Notebook name is its details handle.
  await page.goto(context.base + "/notebooks?namespace=" + encodeURIComponent(NAMESPACE), { waitUntil: "domcontentloaded" });
  await page.locator('[data-table="notebooks"] tbody tr').first().waitFor({ timeout: 10000 });
  const details = await clickResourceName(page, "notebooks", NOTEBOOK_WORK.name);
  assert.ok((await visibleText(details)).includes("image.notebook"), "the colliding Notebook name did not open its own Notebook details");
  await page.keyboard.press("Escape");
  await details.waitFor({ state: "hidden", timeout: 5000 });
}

// The observed details of a Pod, Node and Notebook open from the visible
// resource name into the right hand RESOURCE DETAILS drawer. Those tables have
// no separate Details button or column. Storage keeps its name handle as well.
async function checkResourceDetailsDrawer(context) {
  const { page } = context;
  const cases = [
    { route: "/pods", table: "pods", resource: POD },
    { route: "/nodes", table: "nodes", resource: NODE },
    { route: "/notebooks", table: "notebooks", resource: NOTEBOOK }
  ];
  for (const testCase of cases) {
    await gotoTable(page, context.base, testCase.route, testCase.table);
    await assertNoDetailsControls(page.locator(`[data-table="${testCase.table}"]`), `the ${testCase.table} table`);
    const drawer = await clickResourceName(page, testCase.table, testCase.resource.name);
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

// A Node details drawer shows a scoped Running Pod count above its raw
// observed details. The link opens the Pods page with an exact, URL-backed node
// and Running filter across all namespaces. Clearing filters removes it and
// browser history restores it without any hidden table state.
async function checkNodeScopedPods(context) {
  const { page } = context;
  await gotoTable(page, context.base, "/nodes", "nodes");
  const drawer = await clickResourceName(page, "nodes", NODE.name);
  const body = drawer.locator("#resource-details-body");
  const headings = (await body.locator("h3").allTextContents()).map(heading => heading.trim());
  const scopedAt = headings.indexOf("Scoped view");
  const observedAt = headings.indexOf("Observed details");
  assert.ok(scopedAt >= 0, "the Node details drawer has no Scoped view section");
  assert.ok(observedAt > scopedAt, "the Node Scoped view is not above Observed details");
  const link = body.locator("[data-pod-node-link]").first();
  await link.waitFor({ state: "visible", timeout: 5000 });
  assert.equal(await link.getAttribute("href"), "/pods?node=node-a&phase=Running",
    "the Node scoped link does not carry exactly the node and Running filters");
  assert.match(await visibleText(link), new RegExp(String(NODE_RUNNING_PODS) + "\\s+Running Pods"),
    "the Node scoped view does not show the observed Running Pod count");
  await link.click();
  await page.waitForURL(/\/pods\?node=node-a&phase=Running$/, { timeout: 10000 });
  const filterUrl = new URL(page.url());
  assert.equal(filterUrl.searchParams.get("node"), "node-a", "the destination URL lost the exact node");
  assert.equal(filterUrl.searchParams.get("phase"), "Running", "the destination URL lost the Running filter");
  assert.equal(filterUrl.searchParams.has("namespace"), false, "the node-scoped view is not restricted to one namespace");
  assert.equal(await drawer.isVisible().catch(() => false), false, "the Node details drawer stayed open after the scoped handoff");
  assert.equal(await page.evaluate(() => document.activeElement?.id || ""), "page-title",
    "the scoped handoff did not focus the visible Pods page heading");
  const table = page.locator('[data-table="pods"]');
  const rows = table.locator("tbody tr");
  await rows.first().waitFor({ timeout: 10000 });
  assert.equal(await rows.count(), NODE_RUNNING_PODS, "the scoped Pods view does not show exactly the Running Pods on node-a");
  const states = (await table.locator("tbody tr td:nth-child(8)").allTextContents()).map(value => value.trim());
  const nodes = (await table.locator("tbody tr td:nth-child(9)").allTextContents()).map(value => value.trim());
  assert.ok(states.every(state => state === "Running"), "the scoped Pods view includes a non-Running Pod");
  assert.ok(nodes.every(node => node === "node-a"), "the scoped Pods view includes a Pod from another node");
  const chip = table.locator("[data-pod-filters]");
  await chip.waitFor({ state: "visible", timeout: 5000 });
  const chipText = await visibleText(chip);
  assert.ok(/Node/i.test(chipText) && chipText.includes("node-a") && /Running/.test(chipText),
    "the destination does not show a visible node and Running filter chip");

  // Back and forward rehydrate the URL-backed filters and their visible chip.
  await page.goBack();
  await page.waitForURL(/\/nodes$/, { timeout: 10000 });
  await page.goForward();
  await page.waitForURL(/\/pods\?node=node-a&phase=Running$/, { timeout: 10000 });
  await rows.first().waitFor({ timeout: 10000 });
  assert.equal(await rows.count(), NODE_RUNNING_PODS, "browser forward did not restore the node-scoped Pods");
  await chip.waitFor({ state: "visible", timeout: 5000 });

  // Clear filters rewrites the URL and removes the chip; every Pod returns.
  await table.locator("[data-clear-filters]").click();
  await page.waitForFunction(() => location.pathname === "/pods" && location.search === "");
  assert.equal(await chip.isHidden(), true, "Clear filters left the node chip visible");
  assert.ok(await rows.count() > NODE_RUNNING_PODS, "Clear filters did not restore the unfiltered Pods view");
}

// Only the observed PersistentVolume and PersistentVolumeClaim details offer
// Edit and Delete. A StorageClass keeps its details and grows no actions.
async function checkStorageDetailsActions(context) {
  const { page } = context;
  await gotoTable(page, context.base, "/storage", "storage");
  const tableActions = await page.locator('[data-table="storage"] button').evaluateAll(nodes =>
    nodes.filter(node => ["Edit", "Delete"].includes((node.textContent || "").trim())).length);
  assert.equal(tableActions, 0, "the storage table exposes table-level Edit/Delete actions");
  for (const resource of [STORAGE_CLAIM, STORAGE_VOLUME]) {
    const details = await openStorageDetails(page, context.base, resource);
    const actions = await details.locator("button").evaluateAll(nodes =>
      nodes.filter(node => ["Edit", "Delete"].includes((node.textContent || "").trim())).map(node => ({
        text: (node.textContent || "").trim(), disabled: node.disabled, title: node.title || ""
      })));
    assert.deepEqual(actions.map(action => action.text).sort(), ["Delete", "Edit"],
      `the ${resource.kind} details do not offer exactly Edit and Delete`);
    assert.ok(actions.every(action => !action.disabled), `the ${resource.kind} actions are disabled while both opt-ins are on`);
    await page.keyboard.press("Escape");
    await details.waitFor({ state: "hidden", timeout: 5000 });
  }
  const storageClass = await openStorageDetails(page, context.base, { kind: "StorageClass", name: "data" });
  const classActions = await storageClass.locator("button").evaluateAll(nodes =>
    nodes.filter(node => ["Edit", "Delete"].includes((node.textContent || "").trim())).length);
  assert.equal(classActions, 0, "StorageClass details expose an Edit or Delete action");
  await page.keyboard.press("Escape");
  await storageClass.waitFor({ state: "hidden", timeout: 5000 });
}

// The YAML editor fetches the current restricted projection, binds the exact
// observed UID and resourceVersion and submits the typed projection through
// the existing plan workflow.
async function checkStorageEditPayload(context) {
  const { page, capture } = context;
  const documentReads = [];
  page.on("request", request => {
    try { if (new URL(request.url()).pathname === "/api/v1/storage-document") documentReads.push(request.url()); } catch { /* ignore */ }
  });
  const { drawer } = await openStorageEdit(page, context.base, STORAGE_CLAIM);
  const target = (await drawer.locator("#storage-edit-target").innerText()).trim();
  assert.ok(target.includes("PersistentVolumeClaim") && target.includes("team-alpha/data"), "the editor does not name the exact selected claim");
  assert.equal(await drawer.locator("#storage-edit-target-uid").inputValue(), STORAGE_CLAIM.uid, "the editor does not bind the observed claim UID");
  const resourceVersion = await drawer.locator("#storage-edit-target-rv").inputValue();
  assert.ok(resourceVersion.trim(), "the editor does not bind the current resourceVersion");
  const textarea = drawer.locator("#storage-edit-yaml");
  const original = await textarea.inputValue();
  for (const text of ["kind: PersistentVolumeClaim", "namespace: team-alpha", "uid: uid-pvc-team-alpha-data", "resourceVersion:", "storage: 10Gi"]) {
    assert.ok(original.includes(text), "the restricted claim projection is missing " + text);
  }
  for (const hidden of ["status:", "finalizers:", "annotations:", "managedFields:", "ownerReferences:", "claimRef:", "volumeName:", "synthetic-storage-backend-source"]) {
    assert.ok(!original.includes(hidden), "the editor exposes a hidden backend field: " + hidden);
  }
  assert.match(await drawer.locator("#storage-edit-preserve-hint").innerText(), /not the full manifest/i,
    "the editor does not state that the projection is partial");
  const warnings = drawer.locator("#storage-edit-warnings");
  assert.equal(await warnings.isVisible().catch(() => false), true, "the server-provided projection warnings are not visible");
  assert.match(await warnings.innerText(), /metadata\.labels follows the server projection/i,
    "the editor does not delegate label semantics to the server warnings");
  assert.match(await drawer.locator("#storage-edit-reason-hint").innerText(), /API treats an edit reason as optional/i,
    "the editor does not distinguish its audit reason from the optional API field");
  await textarea.fill(original.replace("storage: 10Gi", "storage: 20Gi"));
  await drawer.locator("#storage-edit-reason").fill("Grow the synthetic claim for browser acceptance");
  const before = capture.plans.length;
  await drawer.locator('button[type="submit"]').click();
  await assertLivePlan(context, capture, before + 1);
  const plan = capture.plans[before];
  assert.equal(plan.body.kind, "pvc.edit", "the storage edit did not plan pvc.edit");
  assert.equal(plan.body.namespace, "team-alpha", "the storage edit lost the claim namespace");
  assert.equal(plan.body.name, "data", "the storage edit lost the claim name");
  assert.equal(plan.body.targetUID, STORAGE_CLAIM.uid, "the plan does not carry the observed target UID");
  assert.equal(plan.body.targetResourceVersion, resourceVersion, "the plan does not carry the observed resourceVersion");
  assert.ok(String(plan.body.storageYAML).includes("storage: 20Gi"), "the plan does not carry the typed projection");
  assert.equal(plan.body.confirmName, undefined, "a storage edit carries a deletion confirmation");
  assert.equal(plan.body.force, undefined, "a storage edit carries a force flag");
  const dialog = page.locator("#plan-dialog");
  await page.locator("#cancel-plan").click();
  await dialog.waitFor({ state: "hidden", timeout: 5000 });

  // Every fresh open performs a full document read; a closed drawer's answer
  // is never reused as the next form.
  const beforeReopen = documentReads.length;
  const reopened = await openStorageEdit(page, context.base, STORAGE_CLAIM);
  assert.ok(documentReads.length > beforeReopen, "reopening the editor reused an old document instead of fetching the current one");
  await page.keyboard.press("Escape");
  await reopened.drawer.waitFor({ state: "hidden", timeout: 5000 });

  // A cluster-scoped PV edit carries no namespace. The synthetic fixture uses
  // the Available spare volume for a plan, so the acceptance check never
  // implies that a Bound volume is eligible.
  const pv = await openStorageEdit(page, context.base, STORAGE_AVAILABLE);
  const pvYaml = await pv.drawer.locator("#storage-edit-yaml").inputValue();
  assert.ok(pvYaml.includes("kind: PersistentVolume") && pvYaml.includes("persistentVolumeReclaimPolicy: Retain"),
    "the PV projection does not show the restricted reclaim policy");
  await pv.drawer.locator("#storage-edit-reason").fill("Review the synthetic volume policy");
  const pvBefore = capture.plans.length;
  await pv.drawer.locator('button[type="submit"]').click();
  await assertLivePlan(context, capture, pvBefore + 1);
  const pvPlan = capture.plans[pvBefore];
  assert.equal(pvPlan.body.kind, "pv.edit");
  assert.equal(pvPlan.body.namespace, undefined, "a PV edit carries a namespace");
  assert.equal(pvPlan.body.name, "spare-volume");
  assert.equal(pvPlan.body.targetUID, STORAGE_AVAILABLE.uid);
  await page.locator("#cancel-plan").click();
  await dialog.waitFor({ state: "hidden", timeout: 5000 });

  // The projection is never persisted in browser storage.
  const stored = await page.evaluate(() => ({
    local: JSON.stringify(window.localStorage || {}),
    session: JSON.stringify(window.sessionStorage || {})
  }));
  for (const value of [stored.local, stored.session]) {
    assert.ok(!value.includes("PersistentVolume") && !value.includes("storageYAML") && !value.includes("apiVersion"),
      "the restricted YAML projection was persisted in browser storage");
  }
}

// Deletion requires the current document, a reason, the exact typed name and a
// separate browser-only acknowledgement. The fixture records a pending plan
// and operation and never claims a live controller execution.
async function checkStorageDeletePayload(context) {
  const { page, capture } = context;
  const { drawer } = await openStorageDelete(page, context.base, STORAGE_CLAIM);
  const warning = (await drawer.locator("#storage-delete-warning").innerText()).trim();
  for (const text of ["not a filesystem wipe", "never removes a finalizer", "needs-review", "do not retry"]) {
    assert.ok(warning.includes(text), "the deletion warning is missing: " + text);
  }
  assert.equal(await drawer.locator("#storage-delete-target-uid").inputValue(), STORAGE_CLAIM.uid);
  const resourceVersion = await drawer.locator("#storage-delete-target-rv").inputValue();
  await drawer.locator("#storage-delete-reason").fill("Remove the synthetic unused claim");
  await drawer.locator("#storage-delete-confirm").fill("data");
  let before = capture.plans.length;
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "the deletion proceeded without the data-loss acknowledgement");
  assert.match(await drawer.locator("#storage-delete-form .form-error").innerText(), /acknowledge/i,
    "the missing acknowledgement is not explained");
  await drawer.locator("#storage-delete-ack").check();
  await drawer.locator("#storage-delete-confirm").fill(" data ");
  await drawer.locator('button[type="submit"]').click();
  await page.waitForTimeout(300);
  assert.equal(capture.plans.length, before, "a padded confirmation reached the plan API");
  assert.match(await drawer.locator("#storage-delete-form .form-error").innerText(), /does not match/i,
    "the exact confirmation mismatch is not explained");
  await drawer.locator("#storage-delete-confirm").fill("data");
  await drawer.locator('button[type="submit"]').click();
  await assertLivePlan(context, capture, before + 1);
  const plan = capture.plans[before];
  assert.equal(plan.body.kind, "pvc.delete");
  assert.equal(plan.body.namespace, "team-alpha");
  assert.equal(plan.body.name, "data");
  assert.equal(plan.body.targetUID, STORAGE_CLAIM.uid);
  assert.equal(plan.body.targetResourceVersion, resourceVersion);
  assert.equal(plan.body.confirmName, "data");
  assert.ok(String(plan.body.reason).includes("Remove the synthetic unused claim"));
  assert.equal(plan.body.force, undefined, "the deletion carries a force flag");
  assert.equal(plan.body.storageYAML, undefined, "the deletion carries a YAML projection");
  assert.equal(plan.body.dataLossAck, undefined, "the browser acknowledgement reached the plan API");
  // The fixture only records a pending operation.
  const operationsBefore = capture.operations.length;
  await page.locator("#confirm-plan").check();
  await page.locator("#submit-plan").click();
  await page.waitForFunction(() => {
    const notices = document.querySelector("#notifications");
    return !!(notices && notices.textContent.includes("accepted (Pending)"));
  }, null, { timeout: 10000 });
  // Once the drawer and confirmation dialog have closed, the accumulated
  // validation errors lead the compact stack, ahead of the acceptance notice.
  const noticeStack = page.locator("#notifications");
  assert.equal(await noticeStack.locator(".notification:visible").count(), 1);
  assert.equal(await noticeStack.locator(".notification:visible").getAttribute("data-level"), "error");
  const stackToggle = noticeStack.locator(".notification-toggle");
  await stackToggle.click();
  assert.equal(await stackToggle.getAttribute("aria-expanded"), "true");
  const noticeOrder = await noticeStack.locator(".notification:visible").evaluateAll(nodes => nodes.map(node => ({
    error: node.dataset.level === "error", recency: Number(node.dataset.recency)
  })));
  assert.ok(noticeOrder.length > 1, "expanding shows the accumulated notifications");
  for (let index = 1; index < noticeOrder.length; index++) {
    const beforeNotice = noticeOrder[index - 1], afterNotice = noticeOrder[index];
    assert.ok(!afterNotice.error || beforeNotice.error, "errors precede non-errors");
    if (beforeNotice.error === afterNotice.error) assert.ok(beforeNotice.recency > afterNotice.recency, "each group is newest-first");
  }
  await stackToggle.click();
  assert.equal(await noticeStack.locator(".notification:visible").count(), 1);
  const notificationToggle = page.locator("#notifications .notification-toggle");
  if (await notificationToggle.isVisible() && await notificationToggle.getAttribute("aria-expanded") === "false") {
    await notificationToggle.click();
  }
  const operationsBeforeAfter = capture.operations.length;
  assert.ok(operationsBeforeAfter > operationsBefore, "the confirmed deletion was not submitted as an operation");
  const notice = (await page.locator("#notifications").innerText()).trim();
  assert.ok(notice.includes("accepted (Pending)"), "the fixture pending operation is not reported honestly: " + notice);
  assert.ok(!/succeeded|deleted/i.test(notice), "the console claimed a completed deletion that the fixture never executed");

  const pv = await openStorageDelete(page, context.base, STORAGE_AVAILABLE);
  const pvWarning = (await pv.drawer.locator("#storage-delete-warning").innerText()).trim();
  assert.ok(pvWarning.includes("PersistentVolume spare-volume") && pvWarning.includes("Retain"),
    "the PV deletion warning does not name the observed reclaim policy");
  await pv.drawer.locator("#storage-delete-reason").fill("Remove the synthetic spare volume");
  await pv.drawer.locator("#storage-delete-confirm").fill("spare-volume");
  await pv.drawer.locator("#storage-delete-ack").check();
  const pvBefore = capture.plans.length;
  await pv.drawer.locator('button[type="submit"]').click();
  await assertLivePlan(context, capture, pvBefore + 1);
  const pvPlan = capture.plans[pvBefore];
  assert.equal(pvPlan.body.kind, "pv.delete");
  assert.equal(pvPlan.body.namespace, undefined, "a PV deletion carries a namespace");
  assert.equal(pvPlan.body.confirmName, "spare-volume");
  await page.locator("#cancel-plan").click();
}

// The storage actions stay visible but disabled when either opt-in or writes
// is off, and the exact gate is stated in visible text.
async function checkStorageGated(context, label) {
  const { page, capture } = context;
  const state = await fetchJson(context.base + "/__fixture").catch(() => null);
  if (!state) {
    console.log("NOTE  profile_browser_optional: the external fixture does not expose its capabilities; storage gating is " + label);
    return;
  }
  const details = await openStorageDetails(page, context.base, STORAGE_CLAIM);
  const actions = await details.locator("button").evaluateAll(nodes =>
    nodes.filter(node => ["Edit", "Delete"].includes((node.textContent || "").trim())).map(node => ({
      text: (node.textContent || "").trim(), disabled: node.disabled
    })));
  assert.equal(actions.length, 2, "gated storage actions are hidden instead of visible and disabled (" + label + ")");
  assert.ok(actions.every(action => action.disabled), "a gated storage action is enabled (" + label + ")");
  const text = (await details.innerText()).trim();
  if (state.features.writesEnabled === false) {
    assert.ok(text.includes("features.writesEnabled is false"), "the read-only policy is not explained (" + label + ")");
  } else {
    assert.ok(text.includes("features.storageEditing is false"), "the editing gate is not explained (" + label + ")");
    assert.ok(text.includes("features.storageDeletion is false"), "the deletion gate is not explained (" + label + ")");
  }
  assert.equal(capture.plans.length, 0, "a gated storage action planned a change (" + label + ")");
  await page.keyboard.press("Escape");
}


async function checkNotebookManagementTarget(context) {
  const { page, capture } = context;
  const details = await openNotebookDetails(page, context.base);
  const modelText = await visibleText(details);
  assert.ok(modelText.includes(NOTEBOOK_MODEL.claim), "the Notebook details drawer does not show the observed storage claim");
  assert.ok(modelText.includes(NOTEBOOK_MODEL.container), "the Notebook details drawer does not show the observed container");
  assert.ok(modelText.includes(NOTEBOOK_MODEL.storageClass), "the Notebook details drawer does not show the observed storage class");
  assert.ok(modelText.includes(NOTEBOOK_MODEL.image), "the Notebook details drawer does not show the observed container image");
  assert.ok(modelText.includes("4 CPU / 8Gi"), "the Notebook details drawer does not show the observed requested resources");
  const manage = await notebookManageControl(details);
  assert.equal(await manage.isDisabled().catch(() => false), false,
    "Manage Notebook is disabled while writes and the controller contract are enabled");
  assert.equal(await page.locator("#notebook-page-manage").isDisabled().catch(() => false), false,
    "the page-level Manage notebook control is disabled while both gates are on");
  assert.equal(await page.locator("#notebook-page-policy").isVisible().catch(() => false), false,
    "a policy hint is visible while both Notebook gates are on");

  await manage.click();
  const form = await findVisible(page, [
    "#notebook-drawer",
    "#notebook-management-drawer",
    '[data-drawer="notebook-management"]'
  ], "the Notebook management drawer");
  await form.locator("form").first().waitFor({ timeout: 10000 });
  // The target is preselected from the opened Notebook, not reset or guessed.
  const namespaceField = await notebookField(form, [
    '#notebook-form [name="namespace"]',
    '[name="targetNamespace"]',
    '#notebook-target-namespace'
  ]);
  assert.equal(await namespaceField.inputValue(), NOTEBOOK_MODEL.namespace,
    "Notebook management is not preselected with the observed namespace");
  const notebookSelector = await notebookField(form, [
    "#notebook-selector",
    '[name="notebookName"]',
    '#notebook-form select[name="name"]'
  ]);
  assert.equal(await notebookSelector.inputValue(), NOTEBOOK_MODEL.name,
    "Notebook management is not preselected with the observed Notebook");
  // The container and its declared resources come from the typed detail
  // document.
  const containerSelector = await notebookField(form, ["#container-selector", '[name="container"]']);
  const optionValues = await containerSelector.locator("option").evaluateAll(nodes => nodes.map(node => node.value || node.textContent));
  assert.ok(optionValues.includes(NOTEBOOK_MODEL.container),
    "the Notebook management form does not offer the observed container (" + optionValues.join(", ") + ")");
  // Opening the exact Notebook never silently switches the action to Edit.
  const action = await notebookField(form, ["#notebook-kind", '[name="kind"]']);
  assert.equal(await action.inputValue(), "notebook.start", "opening Notebook management silently changed the selected action");
  // Nothing is planned just by opening the management drawer.
  assert.equal(capture.plans.length, 0, "opening Notebook management planned a change");
  assert.equal(capture.operations.length, 0, "opening Notebook management submitted an operation");

  // Close and Escape both return the focus to a visible control of the exact
  // Notebook. They never focus the Manage control inside a closed drawer.
  const key = [NOTEBOOK_MODEL.kind, NOTEBOOK_MODEL.namespace, NOTEBOOK_MODEL.name].join("|");
  await form.locator("[data-close-drawer]").first().click();
  await form.waitFor({ state: "hidden", timeout: 5000 });
  await assertNotebookManageFocus(page, details, manage, key);

  // Escape follows the same path. If the console handed the details drawer
  // over to management, the visible Notebook row control is used again.
  let reopened = details;
  let reopenedManage = manage;
  if (!(await details.isVisible().catch(() => false))) {
    reopened = await clickResourceName(page, "notebooks", NOTEBOOK_MODEL.name);
    reopenedManage = await notebookManageControl(reopened);
  }
  await reopenedManage.click();
  await form.waitFor({ state: "visible", timeout: 10000 });
  await page.keyboard.press("Escape");
  await form.waitFor({ state: "hidden", timeout: 5000 });
  await assertNotebookManageFocus(page, reopened, reopenedManage, key);
}

// The production observation has no Notebook details API and no resource or
// mount prefill. Those fields intentionally start blank on Edit: a blank field
// keeps the installed configuration. The runner types a CPU request and one PVC
// mount itself and proves the typed plan carries exactly those values.
async function checkNotebookEditBlankAndPayload(context) {
  const { page, capture } = context;
  const { form } = await openNotebookManagement(page, context.base, NOTEBOOK_WORK);
  const action = await notebookField(form, ["#notebook-kind", '[name="kind"]']);
  assert.equal(await action.inputValue(), "notebook.start", "opening Notebook management silently changed the selected action");
  await action.selectOption("notebook.edit");
  await page.waitForTimeout(150);

  // The installed configuration is not projected into these fields. Blank is
  // the visible "keep current" state.
  for (const name of ["cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "gpuCount"]) {
    const field = await notebookField(form, [`[name="${name}"]`]);
    assert.equal(await field.inputValue(), "", "the Edit form invents a " + name + " instead of keeping the installed value");
  }
  for (const name of ["image", "gpuPreset"]) {
    const field = await notebookField(form, [`[name="${name}"]`]);
    assert.equal(await field.inputValue(), "", "the Edit form invents a " + name + " instead of keeping the installed value");
  }
  assert.equal(await form.locator("#replace-mounts").isChecked(), false, "the Edit form replaces mounts by default");
  assert.equal(await form.locator(".mount-row").count(), 0, "the Edit form invents a PVC mount instead of keeping the installed one");
  assert.equal(await form.locator('[name="restart"]').isChecked(), false, "the Edit form restarts the Notebook by default");
  assert.equal(await form.locator("#notebook-selector").inputValue(), NOTEBOOK_WORK.name, "the Edit form lost the selected Notebook");
  assert.equal(await (await notebookField(form, ["#container-selector", '[name="container"]'])).inputValue(), NOTEBOOK_MODEL.container,
    "the Edit form did not select the one observed container");
  // A visible hint explains that a blank field keeps the installed value.
  const preserveHint = form.locator("#notebook-preserve-hint");
  assert.equal(await preserveHint.isVisible().catch(() => false), true,
    "the Edit form does not show the blank-preserve hint");
  const preserveText = (await visibleText(preserveHint)).trim();
  assert.match(preserveText, /blank.*preserve|preserve.*existing|mounts are kept|keep the existing/i,
    "the blank-preserve hint does not explain what a blank field keeps (" + JSON.stringify(preserveText) + ")");

  // Type only the intended change.
  await (await notebookField(form, ['[name="cpuRequest"]'])).fill("2");
  await form.locator("#add-mount").click();
  const row = form.locator(".mount-row").last();
  await row.locator('[data-field="name"]').fill("team-alpha-claim");
  await row.locator('[data-field="claim"]').selectOption("team-alpha-claim");
  await row.locator('[data-field="path"]').fill("/home/jovyan/data");
  await row.locator('[data-field="readOnly"]').check();
  await form.locator('[name="reason"]').fill("Synthetic browser acceptance reason");

  const before = capture.plans.length;
  await form.locator('#notebook-form button[type="submit"]').click();
  const dialog = page.locator("#plan-dialog");
  await dialog.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(capture.plans.length, before + 1, "the Notebook edit did not produce exactly one plan request");
  const plan = capture.plans[capture.plans.length - 1].body;
  assert.ok(plan && typeof plan === "object", "the Notebook edit plan is not a JSON object");
  assert.equal(plan.kind, "notebook.edit", "the Notebook edit request does not use the typed edit kind");
  assert.equal(plan.namespace, NOTEBOOK_WORK.namespace, "the Notebook edit request does not carry the observed namespace");
  assert.equal(plan.name, NOTEBOOK_WORK.name, "the Notebook edit request does not carry the observed Notebook");
  assert.equal(plan.container, NOTEBOOK_MODEL.container, "the Notebook edit request does not carry the observed container");
  assert.equal(plan.cpuRequest, "2", "the Notebook edit request does not carry the typed CPU request");
  assert.equal(plan.cpuLimit, undefined, "a blank CPU limit was invented in the Notebook edit request");
  assert.equal(plan.memoryRequest, undefined, "a blank memory request was invented in the Notebook edit request");
  assert.equal(plan.memoryLimit, undefined, "a blank memory limit was invented in the Notebook edit request");
  assert.equal(plan.image, undefined, "a blank image was invented in the Notebook edit request");
  assert.equal(plan.gpuPreset, undefined, "a blank GPU preset was invented in the Notebook edit request");
  assert.equal(plan.gpuCount, undefined, "a blank GPU count was invented in the Notebook edit request");
  assert.equal(plan.restart, false, "the Notebook edit request restarts the workload by default");
  assert.deepEqual(plan.mounts, [
    { name: "team-alpha-claim", claim: "team-alpha-claim", path: "/home/jovyan/data", readOnly: true }
  ], "the Notebook edit request does not carry exactly the typed PVC mount");
  assert.equal(capture.operations.length, 0, "the Notebook edit was submitted before the plan was reviewed");
}

// Switching to another Notebook or container is a new target. No typed value,
// mount row, restart flag or old validation message may travel with it.
async function checkNotebookSwitchClearsTypedState(context) {
  const { page } = context;
  const { form } = await openNotebookManagement(page, context.base, NOTEBOOK_WORK);
  await (await notebookField(form, ["#notebook-kind", '[name="kind"]'])).selectOption("notebook.edit");
  await page.waitForTimeout(150);
  for (const [name, value] of [["cpuRequest", "2"], ["cpuLimit", "3"], ["memoryRequest", "4Gi"], ["memoryLimit", "5Gi"], ["gpuCount", "1"]]) {
    await (await notebookField(form, [`[name="${name}"]`])).fill(value);
  }
  await (await notebookField(form, ['[name="image"]'])).selectOption(NOTEBOOK_MODEL.image);
  await (await notebookField(form, ['[name="gpuPreset"]'])).selectOption("L4");
  await form.locator('[name="restart"]').check();
  await form.locator("#add-mount").click();
  const row = form.locator(".mount-row").last();
  await row.locator('[data-field="name"]').fill("old-mount");
  await row.locator('[data-field="claim"]').selectOption("team-alpha-claim");
  await row.locator('[data-field="path"]').fill("/home/jovyan/old");
  await form.locator('[name="reason"]').fill("Reason typed for the first Notebook");
  // A visible target error from the previous selection must not survive the
  // switch. It is placed in the drawer exactly as a refused submission leaves it.
  await form.evaluate(node => {
    const region = node.querySelector(".form-error");
    if (!region) return;
    region.textContent = "The previous Notebook target changed. Reopen it.";
    region.hidden = false;
  });
  assert.equal(await form.locator(".form-error").isVisible().catch(() => false), true,
    "the test could not place a visible stale form error");

  // Select another observed Notebook. It is a new exact target.
  await (await notebookField(form, ["#notebook-selector", '[name="notebookName"]'])).selectOption(NOTEBOOK.name);
  await page.waitForTimeout(250);
  assert.equal(await (await notebookField(form, ['#notebook-form [name="namespace"]'])).inputValue(), NOTEBOOK.namespace,
    "switching Notebook lost the namespace target");
  assert.equal(await (await notebookField(form, ["#notebook-selector", '[name="notebookName"]'])).inputValue(), NOTEBOOK.name,
    "switching Notebook did not bind the new observed Notebook");
  for (const name of ["cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "gpuCount"]) {
    assert.equal(await (await notebookField(form, [`[name="${name}"]`])).inputValue(), "",
      "switching Notebook carried the old " + name + " value");
  }
  for (const name of ["image", "gpuPreset"]) {
    assert.equal(await (await notebookField(form, [`[name="${name}"]`])).inputValue(), "",
      "switching Notebook carried the old " + name + " value");
  }
  assert.equal(await form.locator("#replace-mounts").isChecked(), false, "switching Notebook carried the old mount replacement flag");
  assert.equal(await form.locator(".mount-row").count(), 0, "switching Notebook carried the old PVC mount row");
  assert.equal(await form.locator('[name="restart"]').isChecked(), false, "switching Notebook carried the old restart flag");
  assert.equal(await form.locator(".form-error").isVisible().catch(() => false), false,
    "switching Notebook carried the old validation message");
  assert.equal((await form.locator(".form-error").innerText().catch(() => "")).trim(), "",
    "switching Notebook left the old validation text in the drawer");
  assert.equal(await form.locator("#notebook-target-warning").isVisible().catch(() => false), false,
    "a freshly selected observed Notebook shows a target warning");
  // The selected action is not silently switched on a target change.
  assert.equal(await (await notebookField(form, ["#notebook-kind", '[name="kind"]'])).inputValue(), "notebook.edit",
    "switching Notebook silently changed the selected action");

  // A fresh Manage Notebook opening is a new operation context: the reason
  // typed for the previous Notebook is not reused.
  const reason = form.locator('#notebook-form [name="reason"]');
  assert.equal(await reason.inputValue(), "Reason typed for the first Notebook",
    "the test did not leave the first reason in the form");
  await form.locator("[data-close-drawer]").first().click();
  await form.waitFor({ state: "hidden", timeout: 5000 });
  const reopenedDetails = await clickResourceName(page, "notebooks", NOTEBOOK_WORK.name);
  const reopenedManage = await notebookManageControl(reopenedDetails);
  await reopenedManage.click();
  await form.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(await reason.inputValue(), "", "a fresh Manage Notebook opening reused the previous reason");
  assert.equal(await (await notebookField(form, ['[name="cpuRequest"]'])).inputValue(), "",
    "a fresh Manage Notebook opening reused the previous typed resource");

  // The page-level opener is a new operation context as well: it does not
  // reuse the reason of the Notebook that was just managed.
  await reason.fill("Reason typed for the details opener");
  await form.locator("[data-close-drawer]").first().click();
  await form.waitFor({ state: "hidden", timeout: 5000 });
  const pageOpener = await findVisible(page, [
    '[data-open-drawer="notebook"]',
    { role: "button", name: /manage notebook/i }
  ], "the page-level Notebook management control");
  await pageOpener.click();
  await form.waitFor({ state: "visible", timeout: 10000 });
  assert.equal(await reason.inputValue(), "", "the page-level Manage Notebook opener reused the previous reason");
  assert.equal(await (await notebookField(form, ['[name="cpuRequest"]'])).inputValue(), "",
    "the page-level Manage Notebook opener reused the previous typed resource");
}

// Notebook writes depend on two independent capabilities: writes and a
// confirmed Notebook controller contract. When either is absent, the details
// drawer never offers an enabled management action.
async function checkNotebookManagementGated(context, label) {
  const { page, capture } = context;
  const state = await fetchJson(context.base + "/__fixture").catch(() => null);
  const details = await openNotebookDetails(page, context.base);
  const manage = await notebookManageControl(details, { soft: true });
  if (!state) {
    console.log("NOTE  profile_browser_optional: the external fixture does not expose its capabilities; Manage Notebook is " + (manage ? "offered" : "not offered") + " (" + label + ")");
    return;
  }
  if (state.features.notebookWrites !== false) {
    // An external console was supplied instead of a gated synthetic fixture.
    console.log("NOTE  profile_browser_optional: the supplied console still projects Notebook writes; " + label + " gating is not probed");
    return;
  }
  if (manage) {
    assert.equal(await manage.isDisabled().catch(() => false), true,
      "Manage Notebook is enabled without " + label);
  }
  const manageText = manage ? (await manage.evaluate(node => node.closest("#resource-details-body")?.innerText || "")).trim() : "";
  const pageButton = page.locator("#notebook-page-manage");
  assert.equal(await pageButton.isDisabled().catch(() => false), true,
    "the page-level Manage notebook control is enabled without " + label);
  const pageHint = page.locator("#notebook-page-policy");
  assert.equal(await pageHint.isVisible().catch(() => false), true,
    "the page-level Manage notebook control has no visible policy hint (" + label + ")");
  const hintText = (await pageHint.innerText()).trim();
  if (state.features.writesEnabled === false) {
    assert.match(hintText, /features\.writesEnabled is false/, "the page hint does not name the write gate (" + label + ")");
  }
  if (state.features.notebookWrites === false) {
    assert.match(hintText, /notebooks\.controllerContractConfirmed is false/, "the page hint does not name the controller contract gate (" + label + ")");
  }
  if (manage) {
    assert.match(manageText, /Manage Notebook is unavailable:/, "the details drawer does not show a visible reason (" + label + ")");
    assert.match(manageText, /features\.writesEnabled is false|notebooks\.controllerContractConfirmed is false/,
      "the details reason does not name a gate (" + label + ")");
  }
  assert.equal(capture.plans.length, 0, "a gated Notebook management action planned a change (" + label + ")");
  assert.equal(capture.operations.length, 0, "a gated Notebook management action submitted an operation (" + label + ")");
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
  await assertNoDetailsControls(table, "the Profiles table");
  assert.ok(!(await visibleText(table)).includes(digest), "the configuration precondition is rendered");
  // The one Profile summary hides it as well, and there is no generic
  // resource details drawer behind the Profile name.
  const drawer = await openProfileSummary(page, context.base, PROFILE_ALPHA.name);
  assert.ok(!(await visibleText(drawer)).includes(digest), "the configuration precondition is rendered in the Profile summary");
  assert.equal(await page.locator("#resource-details-drawer").isVisible().catch(() => false), false,
    "a generic RESOURCE DETAILS drawer was opened for a Profile");
  await page.keyboard.press("Escape");
  await drawer.waitFor({ state: "hidden", timeout: 5000 });
  // The summary digest is never searchable either.
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
      if (route === "/pods" || route === "/nodes") {
        const key = route.slice(1);
        const table = page.locator(`[data-table="${key}"]`);
        const headers = await table.locator("th").allTextContents();
        const expected = key === "pods"
          ? ["Pod", "Ready", "Restarts", "State", "Node", "Owner", "Restart"]
          : ["Name", "Status", "Roles", "Age", "Version", "Internal-IP", "External-IP", "Cordon / Uncordon"];
        assert.deepEqual(headers.map(text => text.replace(/[↕▲▼]/g, "").trim()), expected, `${key} has only the simplified columns`);
        if (key === "nodes" && viewport.width > 1200) {
          const widths = await table.locator(".table-scroll").evaluate(node => ({ available: node.clientWidth, content: node.scrollWidth }));
          assert.ok(widths.content <= widths.available + 1, "Nodes table fits its wide-screen container without horizontal scrolling");
        }
        const rowLengths = await table.locator("tbody tr").evaluateAll(rows => rows.map(row => row.children.length));
        assert.ok(rowLengths.every(length => length === expected.length), `${key} cells match their headers`);
        const details = await clickResourceName(page, key, key === "pods" ? "team-alpha-notebook-0-0" : "node-a");
        const detailText = await details.innerText();
        const preserved = key === "pods" ? ["cpuRequest", "memoryRequest", "cpuLimit", "memoryLimit"]
          : ["osImage", "kernelVersion", "containerRuntime"];
        for (const field of preserved) assert.ok(detailText.includes(field), `${key} Resource Details preserves ${field}`);
        await page.keyboard.press("Escape");
      }
      if (route === "/") {
        assert.equal(await page.locator("#page-eyebrow.eyebrow").textContent(), "Cluster at a Glance", "Overview retains the existing eyebrow class");
        assert.equal(await page.locator("#page-subtitle").count(), 0, "Overview has no subtitle paragraph");
        assert.equal(await page.locator("#metrics").count(), 0, "Overview has no legacy metrics row");
        const cards = await page.locator("#workspace-health-summary .health-card").evaluateAll(nodes => nodes.map(node => {
          const rect = node.getBoundingClientRect();
          return { label: node.querySelector("h3").textContent, x: rect.x, y: rect.y, width: rect.width, height: rect.height, bottom: rect.bottom };
        }));
        assert.deepEqual(cards.map(card => card.label), ["Nodes", "Profiles", "Notebooks", "Pods", "PVCs", "PVs", "Events"]);
        if (viewport.width > 1200) {
          const [nodes, profiles, notebooks, pods, pvcs, pvs, events] = cards;
          assert.ok(nodes.width > profiles.width && nodes.height > profiles.height, "Nodes is larger than the other health cards");
          assert.ok(nodes.x < profiles.x && profiles.x < notebooks.x && notebooks.x < pods.x, "Nodes sits beside the top row of three cards");
          assert.ok(Math.abs(profiles.y - notebooks.y) < 1 && Math.abs(profiles.y - pods.y) < 1, "top row is aligned");
          assert.ok(Math.abs(pvcs.y - pvs.y) < 1 && Math.abs(pvcs.y - events.y) < 1 && pvcs.y > profiles.y, "bottom row is aligned below the top row");
          assert.ok(Math.abs(profiles.x - pvcs.x) < 1 && Math.abs(notebooks.x - pvs.x) < 1 && Math.abs(pods.x - events.x) < 1, "six cards form three columns");
          assert.ok(Math.abs(nodes.bottom - events.bottom) < 1, "Nodes spans both rows");
        }
      }
    }
  }
  // Exercise client-side navigation, not just direct page loads: subtitles
  // must be recreated once outside Overview and removed again on return.
  await page.locator('nav a[data-page="overview"]').click();
  assert.equal(await page.locator("#page-subtitle").count(), 0);
  await page.locator('nav a[data-page="profiles"]').click();
  assert.equal(await page.locator("#page-subtitle").count(), 1);
  assert.equal(await page.locator("#page-subtitle").textContent(), "Manage ownership, namespaces and resource quotas.");
  assert.equal(await page.locator("#page-eyebrow").textContent(), "CLUSTER ADMINISTRATION");
  await page.locator('nav a[data-page="overview"]').click();
  assert.equal(await page.locator("#page-subtitle").count(), 0);
  await page.goBack();
  assert.equal(await page.locator("#page-subtitle").count(), 1);
  await page.goForward();
  assert.equal(await page.locator("#page-subtitle").count(), 0);
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
  let readonlyBase = base;
  let noContractBase = base;
  let storageGatedBase = base;
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
      const readonly = await startFixture({ artifact, writes: false, deletion: false, port: READONLY_FIXTURE_PORT });
      fixtures.push(readonly);
      readonlyBase = readonly.base;
      const noContract = await startFixture({ artifact, writes: true, deletion: false, contract: false, port: NOCONTRACT_FIXTURE_PORT });
      fixtures.push(noContract);
      noContractBase = noContract.base;
      const storageGated = await startFixture({ artifact, writes: true, deletion: false, storageEditing: false, storageDeletion: false, port: STORAGE_GATED_FIXTURE_PORT });
      fixtures.push(storageGated);
      storageGatedBase = storageGated.base;
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
      await fn({ page, capture, errors, base: options.base || base, gatedBase, readonlyBase, noContractBase, storageGatedBase });
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
    await run("an existing alias and device key are edited under their own names", checkLegacyQuotaKeyUpdate);
    await run("a plan that has expired is never submitted", checkExpiredPlanRefused);
    await run("an observed quota of zero never blocks an update", checkZeroQuotaUpdate);
    await run("the destructive Profile action is gated by the deletion capability", checkDeletionCapabilityGated, { base: gatedBase });
    await run("the destructive Profile action needs a confirmation and shows a warning", checkDestructiveConfirmation);
    await run("the namespace summary links are scoped and filtered by name", checkNamespaceScopedLinks);
    await run("resources of different kinds can share a name", checkCollidingResourceRows);
    await run("the observed details open the right hand drawer", checkResourceDetailsDrawer);
    await run("a Node opens its exact Running Pods view", checkNodeScopedPods);
    await run("only PV and PVC details offer Edit and Delete", checkStorageDetailsActions);
    await run("the storage YAML editor binds the exact target and submits one plan", checkStorageEditPayload);
    await run("storage deletion needs an exact confirmation and records a pending plan", checkStorageDeletePayload);
    await run("gated storage actions stay visible with the exact policy reason", checkStorageGated, { base: storageGatedBase });
    await run("read-only storage actions stay visible with the write policy reason", context => checkStorageGated(context, "writes"), { base: readonlyBase });
    await run("Manage Notebook binds the exact observed target and returns focus", checkNotebookManagementTarget);
    await run("Notebook Edit keeps blank fields and sends exactly the typed change", checkNotebookEditBlankAndPayload);
    await run("switching Notebook clears typed resources, mounts and errors", checkNotebookSwitchClearsTypedState);
    await run("Manage Notebook is gated when writes are disabled", context => checkNotebookManagementGated(context, "writes"), { base: readonlyBase });
    await run("Manage Notebook is gated without the controller contract", context => checkNotebookManagementGated(context, "controller contract"), { base: noContractBase });
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
