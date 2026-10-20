"use strict";
// Optional real-browser regression runner. It is not part of the normal Go
// test path and adds no dependency to the repository. Provide the supervisor's
// synthetic fixture URL and a Playwright module path to run it:
//
//   DASHBOARD_BASE_URL=http://127.0.0.1:18767 \
//   PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
//   CHROME_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
//   node internal/web/testdata/browser_optional.cjs
//
// Without those inputs it exits successfully after printing SKIP.
const assert = require("node:assert/strict");

const base = process.env.DASHBOARD_BASE_URL || "http://127.0.0.1:18767";
const chrome = process.env.CHROME_BIN || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";

function loadPlaywright() {
  const candidates = [process.env.PLAYWRIGHT_MODULE, "playwright"].filter(Boolean);
  for (const candidate of candidates) {
    try { return require(candidate); } catch { /* try next */ }
  }
  return null;
}

(async () => {
  const playwright = loadPlaywright();
  if (!playwright) { console.log("SKIP browser_optional: Playwright module not provided"); return; }
  try {
    const health = await fetch(base + "/api/v1/inventory");
    if (!health.ok) throw new Error("fixture not ready");
  } catch {
    console.log("SKIP browser_optional: synthetic fixture is not reachable at " + base);
    return;
  }
  let browser;
  try { browser = await playwright.chromium.launch({ executablePath: chrome, headless: true }); }
  catch { console.log("SKIP browser_optional: Chrome/Playwright could not launch"); return; }
  try {
    // --- Independent History load during a slow inventory read, headers, counts,
    // lazy stage details, pager focus and local freshness under server skew.
    const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
    page.setDefaultTimeout(8000);
    const pageErrors = [];
    page.on("pageerror", error => pageErrors.push(error.message));
    let inventoryRequests = 0, inventoryPending = 0;
    let releaseInventory;
    const inventoryGate = new Promise(resolve => { releaseInventory = resolve; });
    for (const tab of ["operations", "audit"]) {
      await page.route(`**/api/v1/${tab}`, async route => {
        const response = await route.fetch();
        await route.fulfill({
          response,
          headers: {
            ...response.headers(),
            "x-history-limit": "500",
            "x-history-window": tab === "audit" ? "recent-audit-events" : "recent-plus-unresolved",
            "x-history-retention-days": "60"
          }
        });
      });
    }
    await page.route("**/api/v1/inventory", async route => {
      inventoryRequests++; inventoryPending++;
      await inventoryGate;
      const response = await route.fetch();
      const body = await response.json();
      body.observedAt = "2000-01-01T00:00:00Z"; // server clock skew must not matter
      await route.fulfill({ response, json: body });
      inventoryPending--;
    });
    await page.goto(base + "/history?tab=operations");
    await page.locator('[data-table="historyOperations"] tbody tr').first().waitFor();
    assert.equal(inventoryPending, 1, "History renders while inventory is still pending");
    assert((await page.locator('[data-table="historyOperations"] [data-namespace] option').allTextContents()).includes("team-a"), "History namespaces populate without inventory");
    releaseInventory();
    await page.locator("#observed-at").filter({ hasText: "Observed" }).waitFor();
    assert((await page.locator('[data-table="historyOperations"] .table-note').innerText()).includes("up to 500 recent operations plus unresolved operations"));
    await page.locator('[data-history-tab="audit"]').click();
    await page.locator('[data-table="historyAudit"] tbody tr').first().waitFor();
    const auditTable = page.locator('[data-table="historyAudit"]');
    assert((await auditTable.locator(".result-count").innerText()).includes("operation group"));
    assert((await auditTable.locator(".result-count").innerText()).includes("loaded audit event"));
    assert((await auditTable.locator(".table-note").innerText()).includes("at most 60 days"));
    assert.equal(await auditTable.locator("details.stage-details .stage-table").count(), 0, "stage details start unbuilt");
    await auditTable.locator("details.stage-details summary").first().click();
    await auditTable.locator("details.stage-details .stage-table").first().waitFor();
    assert.equal(await auditTable.locator("details.stage-details .stage-table").count(), 1, "stage details build on toggle");
    const firstAuditText = await auditTable.locator("tbody tr").first().innerText();
    assert.equal(firstAuditText.split("Synthetic administrator reason").length - 1, 1, "reason is shown once per group");
    await page.locator('[data-history-tab="operations"]').click();
    const next = page.locator('[data-table="historyOperations"] [data-pager="next"]');
    await next.click();
    assert.equal(await page.evaluate(() => document.activeElement?.dataset?.pager), "next", "pager preserves keyboard focus");
    for (const route of ["profiles", "storage", "notebooks", "pods", "overview"]) {
      await page.locator(`nav a[data-page="${route}"]`).click();
      await page.locator(`[data-table="${route}"] tbody tr`).first().waitFor();
    }
    assert.equal(inventoryRequests, 1, "server observedAt skew never forces a re-scan on navigation");
    assert.deepEqual(pageErrors, []);
    await page.close();

    // --- Drawer-scoped plan failure/retry/dismiss and unsupported owner gating.
    const errorPage = await browser.newPage({ viewport: { width: 1280, height: 844 } });
    errorPage.setDefaultTimeout(8000);
    let planFailures = 2;
    await errorPage.route("**/api/v1/plans", async route => {
      if (planFailures-- > 0) {
        await route.fulfill({ status: 422, contentType: "application/json", body: JSON.stringify({ error: { code: "validation_failed", message: "Synthetic plan rejected for test.", retryable: false } }) });
      } else await route.continue();
    });
    await errorPage.route("**/api/v1/inventory", async route => {
      const response = await route.fetch();
      const body = await response.json();
      const pod = body.resources.find(resource => resource.kind === "Pod" && resource.name === "work-000-0");
      if (pod) { pod.owner = "Deployment/legacy"; pod.details = { ...(pod.details || {}), owner: "Deployment/legacy" }; }
      await route.fulfill({ response, json: body });
    });
    await errorPage.goto(base + "/notebooks");
    await errorPage.locator('[data-table="notebooks"] tbody tr').first().waitFor();
    await errorPage.locator('[data-open-drawer="notebook"]').click();
    const drawer = errorPage.locator("#notebook-drawer");
    assert(await drawer.isVisible());
    assert.equal(await errorPage.evaluate(() => document.querySelector("main").inert), true, "drawer inerts the background");
    await drawer.locator('[name="namespace"]').selectOption("team-a");
    await drawer.locator('[name="name"]').selectOption("work-000");
    await drawer.locator('[name="reason"]').fill("Synthetic retry reason");
    await drawer.getByRole("button", { name: "Prepare plan" }).click();
    const formError = drawer.locator(".form-error");
    await formError.waitFor();
    assert((await formError.innerText()).includes("Synthetic plan rejected"));
    const storageBefore = await errorPage.evaluate(() => sessionStorage.getItem("kf-admin:notifications:v1:synthetic-session-one"));
    await errorPage.evaluate(() => {
      const notice = [...document.querySelectorAll("#notifications article")].find(item => item.innerText.includes("Synthetic plan rejected"));
      notice?.querySelector("button")?.click();
    });
    const storageAfter = await errorPage.evaluate(() => sessionStorage.getItem("kf-admin:notifications:v1:synthetic-session-one"));
    assert.equal(storageAfter, storageBefore, "action-error dismissal is not remembered");
    await drawer.getByRole("button", { name: "Prepare plan" }).click();
    await formError.waitFor();
    assert((await formError.innerText()).includes("Synthetic plan rejected"), "retry shows the form error again");
    assert.equal(await errorPage.locator("#notifications article", { hasText: "Synthetic plan rejected" }).count(), 1, "retry re-notifies after dismissal");
    await errorPage.keyboard.press("Escape");
    assert(await drawer.isHidden());
    assert.equal(await errorPage.evaluate(() => document.querySelector("main").inert), false, "background inert is released");
    assert(await formError.isHidden(), "closing the drawer clears the form error");
    await errorPage.locator('nav a[data-page="pods"]').click();
    await errorPage.locator('[data-table="pods"] tbody tr').first().waitFor();
    const legacyRow = errorPage.locator('[data-table="pods"] tbody tr', { hasText: "work-000-0" });
    assert(await legacyRow.getByRole("button", { name: /^Restart Pod/ }).isDisabled(), "observed non-StatefulSet owner disables restart");
    assert((await legacyRow.getByRole("button", { name: /^Restart Pod/ }).getAttribute("title")).includes("not a StatefulSet"));
    await errorPage.close();

    console.log("PASS browser_optional: history race, typed headers/counts, lazy stages, pager focus, local freshness, drawer error retry, unsupported-owner gating");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
