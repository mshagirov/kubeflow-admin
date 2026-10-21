"use strict";
// Focused navigation/presentation contract tests. Pure navigation, inventory
// refresh and table state live in logic.js; the static shell is checked so a
// refactor cannot silently drop required controls, meta fields or CSP rules.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const logic = require("../assets/logic.js");

const root = path.join(__dirname, "..", "assets");
const html = fs.readFileSync(path.join(root, "index.html"), "utf8");
const app = fs.readFileSync(path.join(root, "app.js"), "utf8");
const style = fs.readFileSync(path.join(root, "style.css"), "utf8");

// Both browser scripts must parse before any test relies on them.
new vm.Script(app, { filename: "app.js" });
new vm.Script(fs.readFileSync(path.join(root, "logic.js"), "utf8"), { filename: "logic.js" });

// Sidebar order and legacy History entry points.
const nav = html.match(/<nav aria-label="Main navigation">([\s\S]*?)<\/nav>/);
assert.ok(nav, "main navigation is missing");
const links = [...nav[1].matchAll(/data-page="([^"]+)"[^>]*>([^<]+)</g)].map(match => [match[1], match[2]]);
assert.deepEqual(links, [["overview", "Overview"], ["profiles", "Profiles"], ["namespaces", "Namespaces"], ["storage", "Storage"], ["notebooks", "Notebooks"], ["pods", "Pods"], ["nodes", "Nodes"], ["history", "History"]]);
assert.ok(html.includes('href="/history" data-page="history"'));
assert.ok(html.includes('class="source-link"') && html.includes('href="https://github.com/mshagirov/kubeflow-admin.git"') && html.includes('class="gh-icon"') && html.includes('rel="noopener noreferrer"'), "sidebar links to the public source repository");
assert.ok(style.includes(".source-link") && style.includes(".gh-icon"));
assert.ok(html.includes('</a><small>© 2026 Murat Shagirov</small></div></aside>'), "sidebar copyright follows the repository link");
assert.equal(html.includes("Administrative control plane"), false, "sidebar omits the administrative control plane label");
assert.equal(html.includes('<span class="dot"></span>'), false, "sidebar omits the status dot");
assert.equal(/data-page="(operations|audit)"/.test(html), false, "History replaces separate sidebar entries");
assert.equal(html.includes('<section id="operations"'), false);
assert.equal(html.includes('<section id="audit"'), false);

// The Overview no longer carries the static explanation panels: the console
// explains nothing the operator did not ask for.
assert.equal(html.includes("Review before you apply"), false, "the Overview no longer renders the review panel");
assert.equal(html.includes("Observed, not assumed"), false, "the Overview no longer renders the observed-state panel");
assert.equal(html.includes("info-grid"), false, "the Overview info grid is removed");
assert.equal(style.includes(".info-grid"), false, "the removed info grid leaves no styling behind");
assert.ok(style.includes(".info{"), "the shared info panel styling stays for the other pages");

// The footer is deployment configuration, not a shipped sentence. An empty
// configuration renders no footer element at all.
assert.equal(html.includes("Administrative changes are audited"), false, "the built-in footer sentence is removed");
assert.equal(html.includes("Inventory reflects its observation timestamp"), false, "the built-in footer sentence is removed");
assert.equal(html.includes("History is a bounded recent window, not a full export"), false, "the built-in footer sentence is removed");
assert.ok(html.includes("{{if .Config.UI.FooterText}}") && html.includes("<footer>{{.Config.UI.FooterText}}</footer>"), "the footer text is projected from the deployment configuration");
assert.ok(/<footer>\{\{\.Config\.UI\.FooterText\}\}<\/footer>/.test(html), "the footer carries no hardcoded text");

// The plan confirmation workflow is unchanged: the browser still confirms a
// server-owned plan before anything is submitted.
assert.ok(html.includes('id="plan-dialog"') && html.includes('id="plan-preview"') && html.includes('id="confirm-plan"') && html.includes('id="cancel-plan"') && html.includes('id="submit-plan"'), "the plan confirmation workflow is unchanged");
assert.deepEqual(logic.routeFor("/operations", ""), { page: "history", tab: "operations" });
assert.deepEqual(logic.routeFor("/audit", ""), { page: "history", tab: "audit" });
assert.deepEqual(logic.routeFor("/history", "?tab=audit"), { page: "history", tab: "audit" });
assert.deepEqual(logic.routeFor("/namespaces", ""), { page: "namespaces", tab: null });
assert.deepEqual(logic.routeFor("/nodes", ""), { page: "nodes", tab: null });

// Required page containers and shared viewers.
for (const table of ["overview", "profiles", "namespaces", "storage", "notebooks", "pods", "nodes", "historyOperations", "historyAudit"]) {
  assert.ok(html.includes(`data-table="${table}"`), `missing table ${table}`);
}
for (const id of ["overview", "profiles", "namespaces", "storage", "notebooks", "pods", "nodes", "history"]) {
  assert.ok(new RegExp(`<section id="${id}" class="page"`).test(html), `missing page ${id}`);
}
assert.ok(html.includes('role="tablist"') && html.includes('data-history-tab="operations"') && html.includes('data-history-tab="audit"'));

// Supervisors inject these; the browser must not make an extra /me round trip.
assert.ok(html.includes('<meta name="notification-scope" content="{{.Identity.NotificationScope}}">'));
assert.ok(html.includes('<meta name="pod-restarts" content="{{.Config.Features.PodRestarts}}">'));
assert.ok(html.includes('<meta name="node-cordon" content="{{.Config.Features.NodeCordon}}">'));
assert.ok(app.includes("nodeCordonFeature") && app.includes('meta("node-cordon")'));
assert.ok(html.includes('<meta name="csrf-token" content="{{.Identity.CSRF}}">'));
assert.ok(html.includes('<meta name="profile-deletion" content="{{.Config.Features.ProfileDeletion}}">'), "the profile deletion capability is projected to the browser as a feature meta");
assert.ok(html.includes("{{if .Identity.DisplayName}}"));
assert.ok(html.includes('action="/auth/logout" method="post"'));
assert.ok(html.includes('name="csrf"'));
assert.ok(app.includes("notification-scope") && app.includes("pod-restarts"));
assert.ok(app.includes("podRestartFeature") && app.includes("&& notebookWrites"), "pod restart requires confirmed notebook controller contract");
assert.ok(app.includes("podRestartSupport") && app.includes("not a StatefulSet"), "unsupported observed owners explain disabled restart");
assert.ok(app.includes("setBackgroundInert") && app.includes("main.inert") && app.includes("sidebar.inert"));
assert.ok(app.includes("node.isConnected") || app.includes("target.isConnected"));
assert.ok(app.includes("showFormError") && app.includes("clearFormErrors"), "drawer form errors are scoped and cleared on close");
assert.ok(app.includes("persistableCode") && app.includes('code === "inventory"') && app.includes("/^policy:/"), "only stable observation/policy notices remember dismissals");
assert.equal(app.includes("logic.actorID"), false, "raw actor IDs are never rendered or searched in the UI");
assert.equal(logic.actorID, undefined, "the Actor ID helper is removed from shared frontend logic");
assert.equal(logic.profileResources([{kind:"Profile",name:"team"},{kind:"Namespace",name:"team"}], "other").length, 1, "Profiles remain cluster-wide");
assert.ok(app.includes("actorLabel(record)") && logic.actorLabel({actor:"issuer|subject"}) === "Legacy administrator", "legacy actors use one readable label");
assert.ok(app.includes("operationStateCell") && app.includes("state-error-slot") && app.includes("error-tooltip") && app.includes('"Failure details: "'), "failure indicators occupy a fixed slot with an accessible tooltip");
assert.ok(app.includes("aria-describedby") && app.includes("aria-label") && !app.includes("button.title = failure"), "failure tooltip is linked accessibly without a native title duplicate");
assert.ok(app.includes('id: "toggle-overview-table"') || html.includes('id="toggle-overview-table"'), "the detailed overview table has a toggle");
assert.ok(html.includes('id="overview-table" data-table="overview" hidden'), "the detailed overview table starts hidden");
assert.ok(app.includes("workspaceHealthSummary") && html.includes('id="workspace-health-summary"') && html.includes('id="overview-namespace"'), "workspace health summary and its namespace selector are wired");
assert.ok(app.includes("openNamespaceSummary") && html.includes('id="namespace-summary-drawer"'), "namespace names open a right-side resource summary");
assert.ok(app.includes("openNodeCordon") && html.includes('id="node-cordon-drawer"') && html.includes('name="force"'), "nodes expose a cordon/uncordon drawer with force");
assert.ok(app.includes("logic.namespaceResources") && app.includes("logic.formatAge"), "namespaces and age use shared pure helpers");
assert.ok(app.includes("historyInflight") && app.includes("ensureActiveHistory"), "per-tab in-flight history loading is independent of inventory refresh");
assert.ok(app.includes("historyLimit") && app.includes("historyWindow") && app.includes("X-History-Limit") && app.includes("X-History-Window"));
assert.ok(app.includes("submitInflight") && app.includes("cancelPlanForNavigation"), "navigation preserves an in-flight idempotent submit");
assert.ok(app.includes("invalidateHistoryCaches"), "accepted mutations invalidate history caches");
assert.ok(app.includes("monotonicNow") && app.includes("visibilitychange"), "freshness uses local receipt time and visibility refresh");
assert.ok(app.includes("auditStageTable") && app.includes('addEventListener("toggle"'), "audit stage details are built lazily");
assert.ok(app.includes("operation group") && app.includes("loaded audit event"), "audit counts label groups and loaded events");
assert.ok(app.includes("historyNote") && app.includes("Loaded window only"));
assert.ok(app.includes('value === null || value === undefined || value === "" ? "—"'), "empty preview values render as an em dash");
assert.ok(app.includes("notebookCondition(resource)") && app.includes('key: "condition", label: "Capacity / condition", type: "text"'), "notebook condition sorts the displayed text, not a quantity");
assert.ok(app.includes('type: "mixed"'), "desired/requested columns use mixed numeric/text sorting");
assert.ok(app.includes("historyCache[tab].rows") && app.includes("logic.parseTarget(row.target)"), "history namespaces are available without inventory");

// Accessible management drawers for Profile, Storage, Notebook and Pod restart,
// including the scoped Profile summary modal.
for (const drawer of ["profile-drawer", "storage-drawer", "notebook-drawer", "profile-summary-drawer", "pod-restart-drawer", "namespace-summary-drawer", "node-cordon-drawer", "resource-details-drawer"]) {
  assert.ok(html.includes(`id="${drawer}"`), `missing ${drawer}`);
}
assert.equal((html.match(/aria-modal="true"/g) || []).length >= 8, true);
assert.ok(html.includes('id="resource-details-body"') && html.includes('aria-labelledby="resource-details-title"'), "the observed resource details drawer is a labelled modal dialog");
assert.ok(app.includes("openResourceDetails") && app.includes("detailEntries"), "observed resource details open the right-side drawer");
assert.ok(app.includes('(resource?.kind || "") === "Profile"') && app.includes('openProfileSummary(resource.name || "", opener)'), "generic Profile details always hand over to the consolidated summary");
assert.ok(app.includes("logic.notebookResources(scoped)") && app.includes("nameDetailsKinds"), "Notebook rows are Notebook CRs only and names open resource details");
assert.equal(app.includes("detailsNode"), false, "resource details never use an inline disclosure");
assert.ok(app.includes('el("details", undefined, "stage-details")'), "audit stage details keep their lazy disclosure");
assert.ok(app.includes("data-clear-filters") && app.includes("clearFilters"), "every table offers a clear-filters control");
assert.ok(app.includes("syncClearFilters") && app.includes("aria-disabled"), "the clear-filters control reports whether a filter is applied");
assert.ok(app.includes("applyResourceScope") && app.includes("resourceScope") && app.includes("dropResourceScope"), "a deep-linked resource focus is applied and dropped coherently");
assert.ok(app.includes("resourceScopeCell") && app.includes("resourceScopeTargets"), "namespace summary resources link to their scoped page");
// The Profile management contract: an owned new name, an editable create
// owner, a read-only update owner and a dedicated deletion drawer.
assert.ok(html.includes('id="profile-create-name"') && html.includes('maxlength="63"') && html.includes('pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"'), "the new Profile name is a DNS-1123 label input");
assert.equal(html.includes('list="profile-names"'), false, "the new Profile name never offers the existing profiles");
assert.equal(html.includes('<datalist id="profile-names">'), false, "the Profile name datalist is removed");
assert.ok(html.includes("Email (registered with identity provider, e.g. user email from LDAP)"), "the create owner asks for the registered email address");
assert.ok(html.includes('name="profileCreateOwner" type="email"'), "the create owner is an email field");
assert.ok(html.includes('name="profileOwner" type="text" readonly'), "the update owner is read-only and still submitted");
assert.equal(html.includes('name="profileOwner" type="text" disabled'), false, "a disabled owner would not be submitted");
assert.ok(html.includes('name="profileBaseConfigDigest"') && html.includes('type="hidden"'), "the configuration precondition is a hidden field");
assert.ok(html.includes("Owner transfer is unsupported by the installed Profile controller"), "the read-only owner explains the unsupported transfer");
assert.ok(html.includes("every existing quota key must be supplied with its current value"), "the quota hint states the no-removal contract");
assert.ok(html.includes('id="profile-delete-drawer"') && html.includes('id="profile-delete-form"'), "the deletion has its own drawer and form");
assert.ok(html.includes('name="kind" value="profile.delete"'), "the deletion form posts the typed deletion action");
assert.ok(html.includes('name="confirmName"') && html.includes('name="reason" required maxlength="500"'), "the deletion form requires a reason and an exact confirmation");
assert.ok(app.includes("profileDeleteValidation") && app.includes("preSubmitError"), "the deletion uses the shared validation helper before the request");
assert.ok(app.includes("profileNameValidation") && app.includes("profileFormValues") && app.includes("profileQuotaValues"), "the Profile form uses the shared pure helpers");
assert.ok(app.includes("profile-use-suggestion") && app.includes("aria-describedby"), "the name suggestion is explicit and accessible");
for (const kind of ["profile", "storage", "notebook"]) assert.ok(html.includes(`data-open-drawer="${kind}"`));
assert.ok(html.includes('id="profile-form"') && html.includes('id="storage-form"') && html.includes('id="notebook-form"') && html.includes('id="pod-restart-form"') && html.includes('id="node-cordon-form"'));
assert.ok(html.includes("notebook-preserve-hint") && html.includes("Blank resource and image fields preserve") && html.includes("PVC mounts are kept unless Replace is selected") && html.includes("validated plan’s before/after changes"), "Notebook management states that blanks preserve current configuration and mounts stay unless replaced");
assert.ok(html.includes('name="kind" value="pod.restart"'));
assert.ok(html.includes('name="kind" value="node.cordon"'));
assert.ok(html.includes("spec.unschedulable=true") || app.includes("spec.unschedulable=true"), "cordon description explains the exact effect");
assert.ok(html.includes("Only Notebook StatefulSet-managed Pods are supported"));
assert.ok(app.includes("closeDrawers(true)") && app.includes("showModal()"), "drawer closes before the plan dialog");
assert.ok(app.includes('event.key === "Escape"') && app.includes("focusable(drawer)"));
assert.ok(app.includes("no implicit") || app.includes("implicitly submit"));

// No CSP-hostile constructs: no inline handlers/styles and no innerHTML.
assert.equal(/\son(click|change|submit|load|error)\s*=/i.test(html), false, "inline handlers are forbidden");
assert.equal(/\sstyle\s*=/i.test(html), false, "inline styles are forbidden");
assert.equal(html.includes("innerHTML"), false);
assert.equal(app.includes("innerHTML"), false);
assert.equal(app.includes("document.write"), false);
assert.equal(app.includes("eval("), false);
assert.ok(style.includes("position:sticky") && style.includes("max-height:62vh"), "bounded sticky table viewport is required");
assert.ok(style.includes(".drawer{position:fixed"), "right-side drawer styling is required");
assert.ok(style.includes("@media(max-width:800px)"), "small-screen layout is required");
assert.ok(style.includes(".notification.warning") && style.includes(".notification.error"));
assert.ok(style.includes(".notifications{position:fixed;bottom:18px") && style.includes("max-height:50vh"), "notices sit bottom-right with a bounded stack");
assert.ok(style.includes("body.signed-out main{margin-left:0"), "public signed-out page does not inherit sidebar margin");
assert.ok(style.includes(".form-error") && style.includes(".policy-status"));
assert.ok(style.includes(".state-error-slot") && style.includes(".error-dot") && style.includes(".error-tooltip"), "failure indicator styling is fixed-size and does not shift rows");
assert.ok(style.includes("right:calc(100% + 8px)") && style.includes("transform:translateY(-50%)") && style.includes(".error-dot.open+.error-tooltip"), "failure tooltips open left of the dot and can be pinned by click");
assert.ok(style.includes(".drawer .table-scroll>table{min-width:0;width:100%}"), "drawer summary tables fit the drawer");
assert.ok(style.includes('[data-table="historyOperations"] .table-scroll>table{min-width:780px}') && style.includes('[data-table="namespaces"] .table-scroll>table{min-width:460px}'));
assert.ok(style.includes(".health-summary") && style.includes(".health-count"), "workspace health dashboard cards are styled");
assert.ok(style.includes('[data-table="nodes"]') && style.includes('[data-table="namespaces"]'));

// Credentials, action schema and plan workflow boundaries.
assert.ok(app.includes('crypto.randomUUID()'));
assert.ok(app.includes("plan.prepared.changes.map"), "change preview wiring must remain");
assert.equal(app.includes("plan.prepared.data"), false);
assert.ok(app.includes('credentials: "same-origin"'));
assert.ok(app.includes('"X-CSRF-Token"'));
assert.ok(app.includes("AbortSignal.timeout"));
assert.ok(app.includes("perPage = 50"));
assert.ok(app.includes("sessionStorage") && app.includes("logic.dismissalStorageKey(notificationScope)"));
assert.ok(app.includes("JSON.stringify([...dismissed]"), "only dismissal identifiers are stored");
assert.ok(app.includes("setTimeout(() => removeNotification(id), 10000)"), "success/info notices expire after 10 seconds");
assert.equal(/localStorage/.test(app), false, "no persistent browser inventory storage");

// Sorting, namespace context, scoped links and the "/" shortcut wiring.
assert.ok(app.includes("logic.sortRows"));
assert.ok(app.includes("logic.nextSort"));
assert.ok(app.includes("namespaceContext"));
assert.ok(app.includes("logic.queryNamespace(location.search)"));
assert.ok(app.includes("url.searchParams.set(\"namespace\""));
assert.ok(app.includes("data-scoped-link"));
assert.ok(app.includes("event.key === \"/\""));
assert.ok(app.includes('dialog[open]'));
assert.ok(app.includes("data-search"));
assert.ok(app.includes('requested.cpu') && app.includes('limit.memory'), "actual Pod resource detail projection is consumed");
assert.ok(app.includes("podReady(resource)") && app.includes("podRestartCount(resource)"));

// Bounded history wording and profile integration honesty.
assert.ok(app.includes("historyHref") && app.includes("historyTab"));
assert.ok(app.includes("Retention is at most ${days} days"));
assert.ok(app.includes("not a complete historical export"));
assert.ok(app.includes("Kubeflow pipeline runs are not integrated"));
assert.ok(app.includes("Zero counts are omitted rather than inferred"));
assert.ok(app.includes("Requested and limit values are configuration, not live usage") || html.includes("not live usage"));
assert.ok(html.includes("METRICS NOT INTEGRATED"));

// Inventory reuse policy: no whole-cluster request on every sidebar click.
const now = Date.parse("2026-01-01T12:00:00Z");
assert.equal(logic.needsRefresh("2026-01-01T11:59:45Z", now), false);
assert.equal(logic.needsRefresh("2026-01-01T11:59:00Z", now), true);
assert.equal(logic.needsRefresh("", now), true);
assert.equal(logic.routeFor("/pods", "").page, "pods");
assert.equal(logic.historyHref("operations", "team"), "/history?tab=operations&namespace=team");

// Table state is per table and survives page navigation by identity in app.js;
// the pure helper must preserve search/sort when the page changes.
const operations = logic.defaultTableState("time", "desc");
operations.search = "alice";
const audit = logic.defaultTableState("time", "desc");
assert.equal(operations.search, "alice");
assert.equal(audit.search, "");
assert.equal(operations.sortDir, "desc");
assert.equal(logic.nextSort(operations, "time").sortDir, "asc");

// Minimal browser harness: boot the real script with a stub DOM, navigate every
// sidebar entry and confirm snapshots stay in memory instead of rescanning the
// cluster. This is deliberately small; the pure logic is tested above.
async function bootHarness(options = {}) {
  class Element {
    constructor(tag = "div", selector = "") {
      this.tagName = String(tag).toUpperCase(); this.children = []; this.dataset = {}; this.value = "";
      this.listeners = {}; this.className = ""; this.hidden = false; this.disabled = false; this.open = false;
      this.attributes = {}; this._selector = selector; this.options = []; this.selectedOptions = []; this.elements = {};
      this.required = false; this.isConnected = true; this.parent = null;
      this.classList = { set: new Set(), add: (...names) => names.forEach(name => this.classList.set.add(name)), remove: (...names) => names.forEach(name => this.classList.set.delete(name)), toggle: (name, on) => { const value = on === undefined ? !this.classList.set.has(name) : on; value ? this.classList.set.add(name) : this.classList.set.delete(name); }, contains: name => this.classList.set.has(name) };
    }
    append(...nodes) { nodes.forEach(node => { if (node) node.parent = this; this.children.push(node); }); }
    replaceChildren(...nodes) { this.children.forEach(node => { if (node) { node.parent = null; disconnect(node); } }); nodes.forEach(node => { if (node) node.parent = this; }); this.children = nodes; }
    prepend(...nodes) { nodes.forEach(node => { if (node) node.parent = this; }); this.children.unshift(...nodes); }
    addEventListener(name, handler) { (this.listeners[name] || (this.listeners[name] = [])).push(handler); }
    querySelector(selector) {
      if (selector === "dialog[open]") return null;
      return descendants(this, selector)[0] || null;
    }
    querySelectorAll(selector) { return descendants(this, selector); }
    // A hidden ancestor hides its control as well, exactly as in a browser.
    closest(selector) {
      if (selector !== "[hidden]") return this;
      let node = this;
      while (node) { if (node.hidden) return node; node = node.parent; }
      return null;
    }
    get parentElement() { return new Element(); }
    before() {}
    remove() {
      if (this.parent) { this.parent.children = this.parent.children.filter(child => child !== this); this.parent = null; }
      disconnect(this);
    }
    focus() { document.activeElement = this; }
    click() { for (const handler of this.listeners.click || []) handler({ button: 0, defaultPrevented: false, preventDefault() {}, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, currentTarget: this, target: this }); }
    select() {}
    showModal() { this.open = true; }
    close() { this.open = false; }
    getClientRects() { return this.hidden ? [] : [{}]; }
    contains(node) { return node === this || this.children.some(child => child && typeof child.contains === "function" && child.contains(node)); }
    setAttribute(name, value) { this.attributes[name] = value; }
    hasAttribute(name) { return Object.hasOwn(this.attributes, name); }
    removeAttribute(name) { delete this.attributes[name]; }
    get textContent() { return this._text || ""; }
    set textContent(value) { this._text = String(value); }
  }
  class Option extends Element { constructor(text, value) { super("option"); this.textContent = text; this.value = value; } }
  // A removed element is no longer connected to the document. The application
  // relies on that to find the control that may take the focus back.
  function disconnect(node) {
    if (!node) return;
    node.isConnected = false;
    for (const child of node.children || []) disconnect(child);
  }
  class FakeFormData {
    constructor(form) { this.form = form; }
    *[Symbol.iterator]() {
      // A real FormData carries every enabled, named control of the form.
      for (const control of this.form?.controls || []) {
        if (!control || control.disabled) continue;
        const name = String(control.name || "");
        if (!name) continue;
        if (control.type === "checkbox") { if (control.checked) yield [name, control.value || "on"]; continue; }
        yield [name, control.value === undefined ? "" : control.value];
      }
    }
  }
  function matchesSelector(node, selector) {
    const cleaned = String(selector || "").trim().replace(/:not\([^)]*\)/g, "");
    if (!cleaned) return false;
    if (cleaned.includes(",")) return cleaned.split(",").some(part => matchesSelector(node, part));
    let token = cleaned.split(/\s+/).pop();
    const tag = /^([a-zA-Z][\w-]*)/.exec(token);
    if (tag) { if (node.tagName !== tag[1].toUpperCase()) return false; token = token.slice(tag[1].length); }
    while (token) {
      if (token.startsWith(".")) {
        const match = /^\.([\w-]+)/.exec(token);
        if (!match || !String(node.className).split(/\s+/).includes(match[1])) return false;
        token = token.slice(match[0].length);
        continue;
      }
      if (token.startsWith("[")) {
        const match = /^\[([\w-]+)(?:="([^"]*)")?\]/.exec(token);
        if (!match) return false;
        const name = match[1], expected = match[2];
        let actual;
        if (name.startsWith("data-")) actual = node.dataset?.[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())];
        else if (Object.hasOwn(node, name)) actual = node[name];
        else actual = node.attributes?.[name];
        if (expected === undefined ? actual === undefined : String(actual) !== expected) return false;
        token = token.slice(match[0].length);
        continue;
      }
      return false;
    }
    return true;
  }
  function descendants(node, selector, out = []) {
    for (const child of node.children || []) {
      if (!child || !child.tagName) continue;
      if (matchesSelector(child, selector)) out.push(child);
      descendants(child, selector, out);
    }
    return out;
  }
  const nodes = new Map();
  function get(selector) {
    if (!nodes.has(selector)) nodes.set(selector, new Element("div", selector));
    return nodes.get(selector);
  }
  // Controls created at runtime are found by walking the registered stubs.
  function registryMatches(selector) {
    const found = [];
    for (const node of nodes.values()) {
      if (matchesSelector(node, selector)) found.push(node);
      descendants(node, selector, found);
    }
    return found;
  }
  const sidebar = [["overview", "/"], ["profiles", "/profiles"], ["namespaces", "/namespaces"], ["storage", "/storage"], ["notebooks", "/notebooks"], ["pods", "/pods"], ["nodes", "/nodes"], ["history", "/history"]].map(([page, href]) => {
    const link = new Element("a"); link.dataset.page = page; link.href = "https://admin.example.invalid" + href; return link;
  });
  const sections = ["overview", "profiles", "namespaces", "storage", "notebooks", "pods", "nodes", "history"].map(id => { const section = new Element("section"); section.id = id; return section; });
  const tabs = ["operations", "audit"].map(tab => { const button = new Element("button"); button.dataset.historyTab = tab; return button; });
  const drawers = ["profile", "storage", "notebook", "profile-summary", "pod-restart", "namespace-summary", "node-cordon", "resource-details", "profile-delete"].map(kind => { const drawer = get("#" + kind + "-drawer"); drawer.id = kind + "-drawer"; drawer.hidden = true; return drawer; });
  // The body of a summary drawer lives inside its drawer, exactly as in the
  // document. A closed drawer therefore hides its controls with it.
  for (const kind of ["profile-summary", "namespace-summary", "resource-details"]) get("#" + kind + "-drawer").append(get("#" + kind + "-body"));
  const sharedError = new Element("p"); sharedError.className = "form-error"; sharedError.hidden = true;
  const sharedSubmit = new Element("button"); sharedSubmit.type = "submit"; sharedSubmit.textContent = "Prepare plan";
  const forms = ["profile-form", "storage-form", "notebook-form", "pod-restart-form", "profile-delete-form"].map(id => {
    const form = new Element("form"); form.id = id; form.append(sharedError, sharedSubmit); return form;
  });
  // The forms carry the controls a real browser would submit, so a payload can
  // be asserted end to end.
  const control = (name, value = "", type = "text") => { const input = new Element(type === "select" ? "select" : "input"); input.name = name; input.value = value; input.type = type; return input; };
  const profileQuotaKeys = ["requests.cpu", "requests.memory", "limits.cpu", "limits.memory", "requests.storage", "persistentvolumeclaims", "pods"];
  const profileQuotaInputs = profileQuotaKeys.map(key => { const input = control("quota." + key); input.dataset.profileQuota = key; return input; });
  const profileQuotaFieldset = get("#profile-quota-fields");
  profileQuotaInputs.forEach(input => profileQuotaFieldset.append(input));
  const profileControls = {
    kind: control("kind", "profile.create", "select"),
    profileCreateName: control("profileCreateName"),
    profileTarget: control("profileTarget", "", "select"),
    profileCreateOwner: control("profileCreateOwner", "", "email"),
    profileOwner: control("profileOwner", "", "text"),
    profileBaseConfigDigest: control("profileBaseConfigDigest"),
    reason: control("reason")
  };
  // The stubs are the same nodes the application resolves by id.
  for (const [id, name] of [["#profile-kind", "kind"], ["#profile-create-name", "profileCreateName"], ["#profile-update-name", "profileTarget"],
    ["#profile-create-owner", "profileCreateOwner"], ["#profile-update-owner", "profileOwner"], ["#profile-update-digest", "profileBaseConfigDigest"], ["#profile-reason", "reason"]]) {
    const stub = get(id);
    stub.name = profileControls[name].name;
    stub.value = profileControls[name].value;
    profileControls[name] = stub;
  }
  const staticProfileControls = [profileControls.kind, profileControls.profileCreateName, profileControls.profileTarget, profileControls.profileCreateOwner,
    profileControls.profileOwner, profileControls.profileBaseConfigDigest, profileControls.reason, ...profileQuotaInputs];
  // The unmanaged quota keys are round-tripped through hidden controls the
  // application creates at runtime, exactly as a browser would submit them.
  // The editable fields the application creates for an observed key are part
  // of the form as well, so they are submitted the same way.
  Object.defineProperty(forms[0], "controls", { get() {
    const runtime = registryMatches("[data-profile-quota]").filter(input => !staticProfileControls.includes(input));
    return [...staticProfileControls, ...registryMatches("[data-profile-quota-extra]"), ...runtime];
  } });
  const deleteControls = { kind: control("kind", "profile.delete"), name: control("name"), reason: control("reason"), confirmName: control("confirmName") };
  for (const [id, name] of [["#profile-delete-name", "name"], ["#profile-delete-reason", "reason"], ["#profile-delete-confirm", "confirmName"]]) {
    const stub = get(id);
    stub.name = deleteControls[name].name;
    deleteControls[name] = stub;
  }
  forms[4].controls = [deleteControls.kind, deleteControls.name, deleteControls.reason, deleteControls.confirmName];
  // The Notebook form controls are the same nodes the application resolves:
  // changing a target can be observed in the payload exactly as in a browser.
  const notebookEdit = get("#notebook-edit");
  const notebookControls = {
    kind: get("#notebook-kind"), namespace: get('#notebook-form [name="namespace"]'),
    name: get("#notebook-selector"), container: get("#container-selector"),
    image: get('#notebook-form [name="image"]'), cpuRequest: get('#notebook-form [name="cpuRequest"]'),
    cpuLimit: get('#notebook-form [name="cpuLimit"]'), memoryRequest: get('#notebook-form [name="memoryRequest"]'),
    memoryLimit: get('#notebook-form [name="memoryLimit"]'), gpuPreset: get('#notebook-form [name="gpuPreset"]'),
    gpuCount: get('#notebook-form [name="gpuCount"]'), restart: get('#notebook-form [name="restart"]'),
    reason: get('#notebook-form [name="reason"]')
  };
  for (const [name, controlNode] of Object.entries(notebookControls)) {
    controlNode.name = name;
    controlNode.tagName = ["kind", "namespace", "name", "container", "image", "gpuPreset"].includes(name) ? "SELECT" : "INPUT";
    if (name === "restart") { controlNode.type = "checkbox"; controlNode.checked = false; }
    if (["image", "gpuPreset", "kind"].includes(name)) controlNode.type = "select";
    if (name === "gpuCount") controlNode.type = "number";
    if (!controlNode.value) controlNode.value = "";
  }
  forms[2].append(notebookControls.kind, notebookControls.namespace, notebookControls.name, notebookEdit);
  notebookEdit.append(notebookControls.container, notebookControls.image, notebookControls.cpuRequest, notebookControls.cpuLimit,
    notebookControls.memoryRequest, notebookControls.memoryLimit, notebookControls.gpuPreset, notebookControls.gpuCount,
    notebookControls.restart, get("#replace-mounts"), get("#mount-rows"));
  Object.defineProperty(forms[2], "controls", { get() {
    return [notebookControls.kind, notebookControls.namespace, notebookControls.name, notebookControls.container,
      notebookControls.image, notebookControls.cpuRequest, notebookControls.cpuLimit, notebookControls.memoryRequest,
      notebookControls.memoryLimit, notebookControls.gpuPreset, notebookControls.gpuCount, notebookControls.restart,
      notebookControls.reason];
  } });
  forms[1].controls = [control("kind", "pvc.create"), control("namespace", "team-a"), control("name", "data")];
  forms[3].controls = [control("kind", "pod.restart"), control("namespace", "team-a"), control("name", "work-0"), control("reason", "Synthetic retry reason")];
  const nodeCordonForm = get("#node-cordon-form");
  nodeCordonForm.elements = { kind: new Element("input"), name: new Element("input"), reason: new Element("input"), force: new Element("input") };
  const openers = ["profile", "storage", "notebook"].map(kind => { const button = new Element("button"); button.dataset.openDrawer = kind; return button; });
  const closers = [new Element("button")];
  const labels = ["pvc.create pvc.expand pv.nfs.create pv.reserve", "pvc.create pv.nfs.create pv.reserve", "pv.reserve", "pv.nfs.create", "pv.nfs.create"].map(value => { const label = new Element("label"); label.dataset.storage = value; return label; });
  const handlers = {}, intervals = [], writes = [], calls = [], bodies = [];
  const document = {
    hidden: !!options.hidden, activeElement: new Element("body"),
    body: { dataset: { writes: options.writes === false ? "false" : "true" }, classList: new Element().classList },
    querySelector(selector) {
      if (selector === "dialog[open]") return null;
      const tab = /^\[data-history-tab="(operations|audit)"\]$/.exec(selector);
      if (tab) return tabs.find(item => item.dataset.historyTab === tab[1]);
      return get(selector);
    },
    querySelectorAll(selector) {
      if (selector === "nav a") return sidebar;
      if (selector === ".page") return sections;
      if (selector === "[data-history-tab]") return tabs;
      if (selector === ".drawer") return drawers;
      if (selector === ".drawer form" || selector === "[data-action-form]") return forms;
      if (selector === "[data-open-drawer]") return openers;
      const opener = /^\[data-open-drawer="([^"]+)"\]$/.exec(selector);
      if (opener) return openers.filter(item => item.dataset.openDrawer === opener[1]);
      if (selector === "[data-close-drawer]") return closers;
      if (selector === "[data-namespaces]") return [new Element("select"), new Element("select")];
      if (selector === "[data-gpu-quota]") return [];
      if (selector === "[data-storage]") return labels;
      if (selector === ".form-error") return [sharedError];
      if (selector === ".error-dot.open") {
        const openDots = [];
        const visit = node => { for (const child of node.children || []) { if (!child || !child.tagName) continue; if (String(child.className).split(/\s+/).includes("error-dot") && child.classList.contains("open")) openDots.push(child); visit(child); } };
        for (const node of nodes.values()) visit(node);
        return openDots;
      }
      if (selector === "[data-action-form] button[type=\"submit\"]") return [sharedSubmit];
      if (selector === "[data-profile-quota]" || selector === "[data-profile-quota-extra]") return registryMatches(selector);
      if (selector === "[data-resource-key]") return registryMatches(selector);
      return [];
    },
    createElement(tag) { return new Element(tag); },
    createTextNode(text) { const node = new Element("#text"); node._text = text; return node; },
    addEventListener(name, handler) { (handlers[name] || (handlers[name] = [])).push(handler); }
  };
  const notebookWrites = options.notebookWrites !== false, podRestarts = options.podRestarts !== false, nodeCordon = options.nodeCordon !== false;
  const profileDeletion = options.profileDeletion !== false;
  for (const name of ["csrf-token", "notebook-writes", "static-storage", "pvc-expansion", "pod-restarts", "node-cordon", "profile-deletion", "notification-scope"]) {
    get(`meta[name="${name}"]`).content = name === "csrf-token" ? "test-csrf"
      : name === "notebook-writes" ? String(notebookWrites)
      : name === "pod-restarts" ? String(podRestarts)
      : name === "node-cordon" ? String(nodeCordon)
      : name === "profile-deletion" ? String(profileDeletion)
      : name === "notification-scope" ? "random-test-scope" : "false";
  }
  get("#storage-kind").value = "pvc.create"; get("#notebook-kind").value = "notebook.start"; get("#profile-kind").value = "profile.create";
  get("#overview-table").hidden = true;
  // The detailed Overview table is a hidden section of the page: its rows
  // exist in the document but can never take the focus.
  get('[data-table="overview"]').hidden = true;
  get("#storage-template").selectedOptions = []; get("#pv-selector").selectedOptions = []; get("#replace-mounts").checked = false;
  const mountHint = new Element("p"); mountHint.className = "hint"; get("#replace-mounts").append(mountHint);
  const location = new URL("https://admin.example.invalid/" + (options.path || ""));
  const windowHandlers = {};
  const window = {
    history: {
      pushState(_state, _title, url) { const next = new URL(url, "https://admin.example.invalid"); location.pathname = next.pathname; location.search = next.search; },
      replaceState(_state, _title, url) { const next = new URL(url, "https://admin.example.invalid"); location.pathname = next.pathname; location.search = next.search; }
    },
    addEventListener(name, handler) { (windowHandlers[name] || (windowHandlers[name] = [])).push(handler); }
  };
  const inventory = {
    // Deliberately skewed server time proves the browser uses local receipt age.
    observedAt: options.observedAt || "2000-01-01T00:00:00Z",
    resources: options.resources || [
      { kind: "Profile", name: "team", uid: "uid-profile-team", state: "Active", owner: "alice@example.test",
        details: { apiVersion: "kubeflow.org/v1", ownerKind: "User", createdAt: "2026-01-01T00:00:00Z", resourceVersion: "7", configDigest: "sha256:teamdigest", "quota.requests.cpu": "4", "quota.requests.memory": "8Gi", "quota.example.com/gpu": "1" } },
      { kind: "Profile", name: "guarded", uid: "uid-profile-guarded", state: "Protected", owner: "bob@example.test",
        details: { apiVersion: "kubeflow.org/v1", ownerKind: "User", configDigest: "sha256:guardeddigest" } },
      { kind: "Namespace", name: "team", uid: "uid-namespace-team", state: "Active", details: { createdAt: "2026-01-01T00:00:00Z" } },
      { kind: "Namespace", name: "other", uid: "uid-namespace-other", state: "Active", details: { createdAt: "2026-01-02T00:00:00Z" } },
      { kind: "Namespace", name: "reserved-ns", uid: "uid-namespace-reserved", state: "Active", details: { createdAt: "2026-01-03T00:00:00Z" } },
      { kind: "Notebook", name: "work", namespace: "team", uid: "uid-notebook-work", state: "Running / Ready", details: { "image.main": "image" } },
      // A StatefulSet with the same name as the Notebook: only the kind tells
      // the two observed resources apart.
      { kind: "StatefulSet", name: "work", namespace: "team", uid: "uid-statefulset-work", state: "1 ready", details: { "image.main": "image" } },
      { kind: "Pod", name: "work-0", namespace: "team", uid: "uid-pod-team-work-0", state: "Running", owner: "StatefulSet/work", details: { node: "node-1", ready: "true", containers: "1/1 ready", restarts: "2", "requested.cpu": "500m", "requested.memory": "1Gi", "limit.cpu": "2", "limit.memory": "4Gi" } },
      { kind: "Pod", name: "legacy-0", namespace: "team", uid: "uid-pod-team-legacy-0", state: "Running", owner: "Deployment/legacy", details: { node: "node-1", ready: "true", containers: "1/1 ready", restarts: "0" } },
      // The same Pod name in another namespace is a different Pod.
      { kind: "Pod", name: "work-0", namespace: "other", uid: "uid-pod-other-work-0", state: "Running", owner: "StatefulSet/work", details: { node: "node-2", ready: "true", containers: "1/1 ready", restarts: "0" } },
      { kind: "PersistentVolumeClaim", name: "data", namespace: "team", uid: "uid-pvc-team-data", state: "Bound", details: { requested: "10Gi", capacity: "10Gi", storageClass: "team-class" } },
      // A volume and a storage class that share the name of the claim.
      { kind: "PersistentVolume", name: "data", uid: "uid-pv-data", state: "Bound", details: { requested: "10Gi", capacity: "10Gi", storageClass: "team-class", claim: "team/data" } },
      { kind: "StorageClass", name: "data", uid: "uid-sc-data", details: { provisioner: "example.test/nfs" } },
      { kind: "StorageClass", name: "team-class", uid: "uid-sc-team-class" },
      { kind: "Event", name: "scheduled", namespace: "team", uid: "uid-event-scheduled", state: "Warning" },
      // A resource quota that shares the name of the event.
      { kind: "ResourceQuota", name: "scheduled", namespace: "team", uid: "uid-quota-scheduled", details: { "requests.cpu": "8" } },
      { kind: "Node", name: "node-1", uid: "uid-node-1", state: "Ready,SchedulingDisabled", details: { roles: "control-plane", createdAt: "2026-01-01T00:00:00Z", version: "v1.31.1", internalIP: "10.0.0.1", externalIP: "", osImage: "Flatcar", kernelVersion: "6.6.0", containerRuntime: "containerd://2.0", unschedulable: "true", controlPlane: "true" } },
      { kind: "Node", name: "node-2", uid: "uid-node-2", state: "Ready", details: { roles: "control-plane", createdAt: "2026-01-01T00:00:00Z", version: "v1.31.1", internalIP: "10.0.0.2", externalIP: "", osImage: "Flatcar", kernelVersion: "6.6.0", containerRuntime: "containerd://2.0", unschedulable: "false", controlPlane: "true" } },
      { kind: "Node", name: "node-3", uid: "uid-node-3", state: "NotReady", details: { roles: "worker", createdAt: "2026-01-01T00:00:00Z", version: "v1.31.1", internalIP: "10.0.0.3", externalIP: "", osImage: "Flatcar", kernelVersion: "6.6.0", containerRuntime: "containerd://2.0", unschedulable: "false", controlPlane: "false" } }
    ],
    warnings: ["Filesystem usage is not available."]
  };
  let plansFailures = options.plansFailures || 0;
  let resolveInventory = null;
  const operationsRows = options.operations || [{ id: "operation-race", actor: "https://issuer.example|subject", actorDisplay: "Test Admin", target: "notebook:team/work", action: "notebook.stop", state: "succeeded", step: "observed", createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }];
  const auditRows = options.audit || [
    { id: "audit-q", operationID: "operation-race", actor: "https://issuer.example|subject", actorDisplay: "Test Admin", target: "notebook:team/work", action: "notebook.stop", result: "queued", reason: "Synthetic reason text", time: "2026-01-01T00:00:00Z" },
    { id: "audit-s", operationID: "operation-race", actor: "https://issuer.example|subject", actorDisplay: "Test Admin", target: "notebook:team/work", action: "notebook.stop", result: "succeeded", reason: "Synthetic reason text", time: "2026-01-01T00:00:00Z" }
  ];
  const headers = options.historyHeaders || {
    operations: { "X-History-Limit": "1000", "X-History-Window": "recent-plus-unresolved", "X-History-Retention-Days": "30" },
    audit: { "X-History-Limit": "500", "X-History-Window": "recent-audit-events", "X-History-Retention-Days": "60" }
  };
  const response = (data, ok = true, status = 200, headerMap = {}) => ({
    ok, status,
    headers: { get(name) { return Object.hasOwn(headerMap, name) ? headerMap[name] : null; } },
    json: async () => data
  });
  const fetch = async (url, init = {}) => {
    calls.push(url);
    if (init.body) { try { bodies.push(JSON.parse(init.body)); } catch { bodies.push(init.body); } }
    if (url.endsWith("/inventory")) {
      if (options.deferInventory) await new Promise(resolve => { resolveInventory = resolve; });
      return response(inventory);
    }
    if (url.endsWith("/plans")) {
      if (plansFailures > 0) {
        plansFailures--;
        return response({ error: { code: "validation_failed", message: "Synthetic plan rejected for test.", retryable: false } }, false, 422);
      }
      return response({ id: "synthetic-plan", actor: "https://issuer.example|subject", actorDisplay: "Test Admin", createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 300000).toISOString(), prepared: { action: { kind: "notebook.stop" }, summary: "Synthetic preview", warnings: ["Synthetic interruption warning"], targetUID: "uid", resourceVersion: "1", changes: [{ field: "requests.cpu", before: "4", after: "8" }] } }, true, 201);
    }
    if (url.endsWith("/operations") && init.method === "POST") return response({ id: "operation-test", state: "queued" });
    if (url.endsWith("/operations")) return response(operationsRows, true, 200, headers.operations);
    if (url.endsWith("/audit")) return response(auditRows, true, 200, headers.audit);
    return response([]);
  };
  const context = vm.createContext({
    document, window, location, URL, Option, Node: Element, Date, AbortSignal, console, fetch,
    DashboardLogic: logic, FormData: FakeFormData, crypto: { randomUUID: () => "test-idempotency-key" },
    setInterval(fn) { intervals.push(fn); }, setTimeout() { return 0; }, clearTimeout() {},
    sessionStorage: { getItem() { return null; }, setItem(key) { writes.push(key); } }
  });
  vm.runInContext(app, context);
  const settle = async () => { for (let index = 0; index < 16; index++) await new Promise(setImmediate); };
  await settle();
  function walk(node, predicate, found = []) {
    if (!node) return found;
    if (predicate(node)) found.push(node);
    for (const child of node.children || []) walk(child, predicate, found);
    return found;
  }
  function textOf(node) {
    if (!node) return "";
    return [node._text || "", ...(node.children || []).map(textOf)].join(" ");
  }
  return {
    get, calls, writes, bodies, drawers, openers, closers, tabs, forms, error: sharedError, location, handlers, textOf, walk,
    profileQuotaInputs, profileControls, windowHandlers,
    get activeElement() { return document.activeElement; },
    // The observation is replaced the way a refreshed inventory replaces it.
    setResources(resources) { inventory.resources = resources; return inventory; },
    resources() { return inventory.resources; },
    async popstate(path) {
      if (path !== undefined) {
        const next = new URL(path, "https://admin.example.invalid");
        location.pathname = next.pathname; location.search = next.search;
      }
      for (const handler of windowHandlers.popstate || []) handler();
      await settle();
    },
    async click(page) {
      const link = sidebar.find(item => item.dataset.page === page);
      for (const handler of link.listeners.click || []) handler({ button: 0, defaultPrevented: false, preventDefault() {}, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false });
      await settle();
    },
    async clickTab(tab) {
      const button = tabs.find(item => item.dataset.historyTab === tab);
      for (const handler of button.listeners.click || []) handler({ preventDefault() {} });
      await settle();
    },
    async submitForm(index) {
      for (const handler of forms[index].listeners.submit || []) await handler({ preventDefault() {} });
      await settle();
    },
    resolveInventory() { if (resolveInventory) { const resolve = resolveInventory; resolveInventory = null; resolve(); } },
    setHidden(hidden) {
      document.hidden = hidden;
      if (!hidden) for (const handler of handlers.visibilitychange || []) handler();
    },
    settle
  };
}

(async () => {
  const appTest = await bootHarness({ plansFailures: 2 });
  assert.equal(appTest.calls.filter(url => url.endsWith("inventory")).length, 1, "initial boot reads inventory once");
  assert.ok(appTest.get("#notifications").children.length >= 1, "inventory warnings become notifications");
  assert.deepEqual(appTest.writes, [], "notification dismissal is not persisted without a dismissal");
  for (const page of ["profiles", "storage", "notebooks", "pods", "history", "overview", "pods"]) await appTest.click(page);
  assert.equal(appTest.calls.filter(url => url.endsWith("inventory")).length, 1, "skewed observedAt must not force a scan on every sidebar click");
  assert.equal(appTest.calls.filter(url => url.endsWith("operations")).length, 1, "history loads independently of inventory");
  assert.equal(appTest.calls.some(url => url.includes("/api/v1/me")), false, "the shell must not make an extra identity round trip");

  // Workspace Health dashboard cards respect scope and the detailed table is opt-in.
  await appTest.click("overview");
  const healthText = appTest.textOf(appTest.get("#workspace-health-summary"));
  assert.ok(healthText.includes("Pods") && healthText.includes("Notebooks") && healthText.includes("PVCs") && healthText.includes("PVs") && healthText.includes("Events"), "dashboard cards cover each resource category");
  assert.ok(healthText.includes("Running") && healthText.includes("Warning") && healthText.includes("Errors"));
  const overviewTable = appTest.get("#overview-table");
  assert.equal(overviewTable.hidden, true, "detailed workspace table starts collapsed");
  appTest.get("#toggle-overview-table").listeners.click[0]();
  assert.equal(overviewTable.hidden, false, "toggle reveals the detailed table");
  assert.ok(appTest.textOf(appTest.get("#toggle-overview-table")).includes("Hide detailed resource table"));
  appTest.get("#toggle-overview-table").listeners.click[0]();
  assert.equal(overviewTable.hidden, true, "toggle collapses the detailed table again");

  // Audit window headers, grouped counts, terminal result and lazily built details.
  await appTest.click("history");
  const operationHeaders = appTest.walk(appTest.get('[data-table="historyOperations"]'), node => node.tagName === "TH")
    .map(node => appTest.textOf(node).replace(/[↕▲▼]/g, "").trim()).filter(Boolean);
  assert.deepEqual(operationHeaders, ["Time", "Actor", "Kind", "Namespace", "Name", "Action", "State"], "operation history keeps exactly the seven readable columns");
  for (const absent of ["Step", "Detail", "IDs"]) assert.equal(operationHeaders.includes(absent), false, absent + " column is removed");
  appTest.tabs[0].listeners.keydown[0]({ key: "ArrowRight", preventDefault() {} });
  assert.equal(appTest.activeElement, appTest.tabs[1], "ArrowRight moves keyboard focus to the selected History tab");
  await appTest.settle();
  await appTest.clickTab("audit");
  const auditContainer = appTest.get('[data-table="historyAudit"]');
  const auditText = appTest.textOf(auditContainer);
  assert.ok(auditText.includes("1 operation group"), "audit count labels operation groups");
  assert.ok(auditText.includes("2 loaded audit events"), "audit count labels loaded events");
  assert.ok(auditText.includes("up to 500 recent audit events"), "X-History-Limit and X-History-Window label the window");
  assert.ok(auditText.includes("at most 60 days"), "X-History-Retention-Days labels retention");
  assert.ok(auditText.includes("succeeded") && !auditText.includes("queued"), "equal-timestamp terminal stage wins before details open");
  assert.ok(auditText.includes("Operation: operation-race") && !auditText.includes("Audit group"), "operation ID replaces the synthetic audit-group label");
  assert.equal(auditText.split("Synthetic reason text").length - 1, 1, "group reason is shown once, not per stage");
  const details = appTest.walk(auditContainer, node => String(node.className).includes("stage-details"))[0];
  assert.ok(details, "audit stage details disclosure exists");
  details.open = true;
  for (const handler of details.listeners.toggle || []) handler();
  assert.ok(appTest.walk(details, node => String(node.className).includes("stage-table")).length === 1, "stage table builds on first toggle");
  assert.equal(appTest.walk(details, node => node.tagName === "TH").map(node => node.textContent).includes("Reason"), false, "identical group reasons are not repeated in stage details");

  // Unsupported observed owners are disabled and explained.
  await appTest.click("pods");
  const podsContainer = appTest.get('[data-table="pods"]');
  const podHeaders = appTest.walk(podsContainer, node => node.tagName === "TH")
    .map(node => appTest.textOf(node).replace(/[↕▲▼]/g, "").trim()).filter(Boolean);
  assert.ok(podHeaders.indexOf("Node") > podHeaders.indexOf("State"), "Node column follows State on the Pods table");
  assert.ok(appTest.textOf(podsContainer).includes("node-1"), "Pod rows show the observed node");
  const restartButtons = appTest.walk(podsContainer, node => node.tagName === "BUTTON" && node.textContent === "Restart");
  assert.ok(restartButtons.length >= 2, "Pod rows expose restart controls");
  const unsupported = restartButtons.find(button => String(button.title).includes("not a StatefulSet"));
  assert.ok(unsupported && unsupported.disabled, "observed non-StatefulSet owner disables restart");
  assert.ok(restartButtons.some(button => !button.disabled), "supported observed StatefulSet Pod stays enabled when policy allows");

  const failureTest = await bootHarness({ operations: [
    { id: "operation-failed", actor: "https://issuer.example|raw-subject", target: "pod:team/work-0", action: "pod.restart", state: "failed", error: "Synthetic failure detail", createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() },
    { id: "operation-expired", actor: "https://issuer.example|raw-subject", target: "pod:team/work-0", action: "pod.restart", state: "failed", historyExpired: true, createdAt: new Date(Date.now() - 1000).toISOString(), updatedAt: new Date(Date.now() - 1000).toISOString() }
  ] });
  await failureTest.click("history");
  const failureContainer = failureTest.get('[data-table="historyOperations"]');
  const failureText = failureTest.textOf(failureContainer);
  assert.ok(failureText.includes("Legacy administrator") && !failureText.includes("raw-subject"), "history shows only the readable actor label");
  assert.ok(failureText.includes("Detailed operation history expired; compact retry record retained."), "history-expired records keep a compact explanation");
  const errorDots = failureTest.walk(failureContainer, node => String(node.className).includes("error-dot"));
  const errorDot = errorDots.find(dot => String(dot.attributes["aria-label"]).includes("Synthetic failure detail"));
  const errorTooltip = failureTest.walk(failureContainer, node => String(node.className).includes("error-tooltip"))
    .find(tooltip => tooltip.textContent === "Synthetic failure detail");
  assert.ok(errorDot && errorDot.attributes["aria-label"] === "Failure details: Synthetic failure detail", "failed operations expose an accessible failure indicator");
  assert.equal(errorDot.title, undefined, "the failure indicator does not duplicate the message in a native title");
  assert.ok(errorTooltip && errorTooltip.attributes.role === "tooltip" && errorTooltip.id && errorDot.attributes["aria-describedby"] === errorTooltip.id, "the tooltip has a unique id linked by aria-describedby");
  errorDot.listeners.click[0]();
  assert.equal(errorDot.classList.contains("open"), true, "clicking the dot keeps the tooltip open");
  let tooltipPrevented = false;
  failureTest.handlers.keydown[0]({ key: "Escape", preventDefault() { tooltipPrevented = true; } });
  assert.equal(tooltipPrevented, true, "Escape closes an open failure tooltip");
  assert.equal(errorDot.classList.contains("open"), false, "Escape removes the open state");

  // Legacy audit rows show one readable Actor label in the summary and stages.
  const legacyAuditTest = await bootHarness({ audit: [
    { id: "legacy-q", operationID: "legacy-op", actor: "https://issuer.example|raw-subject", action: "notebook.stop", target: "notebook:team/work", result: "queued", reason: "legacy reason", time: "2026-01-01T00:00:00Z" },
    { id: "legacy-s", operationID: "legacy-op", actor: "https://issuer.example|raw-subject", action: "notebook.stop", target: "notebook:team/work", result: "succeeded", reason: "", time: "2026-01-01T00:00:05Z" }
  ] });
  await legacyAuditTest.click("history");
  await legacyAuditTest.clickTab("audit");
  const legacyAuditContainer = legacyAuditTest.get('[data-table="historyAudit"]');
  const legacyAuditText = legacyAuditTest.textOf(legacyAuditContainer);
  assert.ok(legacyAuditText.includes("Legacy administrator") && !legacyAuditText.includes("raw-subject"), "legacy audit summaries show the fallback actor label only");
  const legacyDetails = legacyAuditTest.walk(legacyAuditContainer, node => String(node.className).includes("stage-details"))[0];
  legacyDetails.open = true;
  for (const handler of legacyDetails.listeners.toggle || []) handler();
  const legacyStageText = legacyAuditTest.textOf(legacyDetails);
  assert.ok(legacyStageText.includes("Legacy administrator") && !legacyStageText.includes("raw-subject"), "legacy audit stages never reveal the encoded actor key");

  // Namespaces page is cluster-wide even when a page-scope namespace is active.
  appTest.get("#overview-namespace").value = "team";
  for (const handler of appTest.get("#overview-namespace").listeners.change || []) handler();
  await appTest.settle();
  assert.equal(appTest.location.search, "?namespace=team");
  await appTest.click("namespaces");
  const namespacesContainer = appTest.get('[data-table="namespaces"]');
  const namespacesText = appTest.textOf(namespacesContainer);
  assert.ok(namespacesText.includes("team") && namespacesText.includes("other"), "Namespaces always lists every observed namespace");
  assert.ok(namespacesText.includes("Active"), "Namespaces table lists observed Namespace state");
  assert.equal(namespacesContainer.querySelector("[data-namespace]"), null, "cluster-wide tables omit the namespace selector");
  const namespaceButton = appTest.walk(namespacesContainer, node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  assert.ok(namespaceButton, "Namespace names are buttons that open the summary drawer");
  namespaceButton.listeners.click[0]();
  const namespaceDrawer = appTest.get("#namespace-summary-drawer");
  assert.equal(namespaceDrawer.hidden, false, "Namespace summary opens on the right");
  const namespaceBody = appTest.textOf(appTest.get("#namespace-summary-body"));
  assert.ok(namespaceBody.includes("Notebooks") && namespaceBody.includes("work") && namespaceBody.includes("Pods") && namespaceBody.includes("Events"), "Namespace summary groups observed resources");
  appTest.closers[0].listeners.click[0]();

  // A scoped Events link opens Overview with the detailed table already expanded.
  namespaceButton.listeners.click[0]();
  const eventsLink = appTest.walk(appTest.get("#namespace-summary-body"), node => node.tagName === "A" && node.href === "/?namespace=team")[0];
  assert.ok(eventsLink, "namespace summary exposes a scoped Events link to Overview");
  for (const handler of appTest.handlers.click || []) handler({ target: eventsLink, defaultPrevented: false, button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, preventDefault() {} });
  await appTest.settle();
  assert.equal(appTest.get("#overview-table").hidden, false, "scoped Overview links auto-expand the detailed table");

  // Profiles also stay cluster-wide under an active namespace context.
  await appTest.click("profiles");
  const profilesContainer = appTest.get('[data-table="profiles"]');
  assert.ok(appTest.textOf(profilesContainer).includes("team"), "Profiles always lists every Profile");
  assert.equal(profilesContainer.querySelector("[data-namespace]"), null, "Profiles table has no namespace selector");

  // Nodes remain cluster-wide under an active namespace context.
  await appTest.click("nodes");
  const nodesContainer = appTest.get('[data-table="nodes"]');
  const nodesText = appTest.textOf(nodesContainer);
  assert.ok(nodesText.includes("node-1") && nodesText.includes("node-2") && nodesText.includes("node-3"), "Nodes always lists every cluster node");
  assert.ok(nodesText.includes("Ready,SchedulingDisabled") && nodesText.includes("containerd://2.0"), "Nodes table shows wide node details");
  assert.equal(nodesContainer.querySelector("[data-namespace]"), null, "Nodes table has no namespace selector");
  assert.ok(appTest.walk(nodesContainer, node => node.tagName === "TH").map(node => node.textContent).includes("Cordon / Uncordon"));
  const uncordon = appTest.walk(nodesContainer, node => node.tagName === "BUTTON" && node.textContent === "Uncordon")[0];
  assert.ok(uncordon && !uncordon.disabled, "cordoned nodes offer an enabled Uncordon action");
  uncordon.listeners.click[0]();
  assert.equal(appTest.get("#node-cordon-drawer").hidden, false, "cordon/uncordon opens its modal drawer");
  assert.ok(appTest.get("#node-cordon-description").textContent.includes("spec.unschedulable=false"));
  assert.equal(appTest.get("#node-cordon-warning").hidden, true, "uncordoning a control-plane node does not require Force");
  assert.equal(appTest.get("#node-cordon-warning").textContent, "");
  assert.equal(appTest.get("#node-cordon-form").elements.force.checked, false, "force is opt-in and starts unchecked");
  appTest.closers[0].listeners.click[0]();

  const cordonNode = name => appTest.walk(nodesContainer, node => node.tagName === "BUTTON" && node.textContent === "Cordon" && node.attributes["aria-label"] === "Cordon node " + name)[0];
  cordonNode("node-2").listeners.click[0]();
  assert.equal(appTest.get("#node-cordon-warning").hidden, false, "cordoning a schedulable control-plane node warns about Force");
  assert.ok(appTest.get("#node-cordon-warning").textContent.includes("control-plane"));
  appTest.closers[0].listeners.click[0]();
  cordonNode("node-3").listeners.click[0]();
  assert.equal(appTest.get("#node-cordon-warning").hidden, false, "cordoning a NotReady node warns about Force");
  assert.ok(appTest.get("#node-cordon-warning").textContent.includes("NotReady"));
  appTest.closers[0].listeners.click[0]();

  // Drawers inert the background, preserve/restore focus input, and close cleanly.
  appTest.openers[0].focus();
  appTest.openers[0].listeners.click[0]();
  assert.equal(appTest.drawers[0].hidden, false, "management button opens its drawer");
  assert.equal(appTest.get("#drawer-scrim").hidden, false, "drawer scrim is shown");
  assert.equal(appTest.get("main").inert, true, "background main is inert");
  assert.equal(appTest.get(".sidebar").inert, true, "background sidebar is inert");
  assert.equal(appTest.drawers.some(drawer => !drawer.hidden && drawer !== appTest.drawers[0]), false, "only one drawer opens at a time");
  appTest.closers[0].listeners.click[0]();
  assert.equal(appTest.drawers.every(drawer => drawer.hidden), true, "close control hides the drawer");
  assert.equal(appTest.get("#drawer-scrim").hidden, true, "drawer scrim is hidden again");
  assert.equal(appTest.get("main").inert, false, "background inert is released");
  assert.equal(appTest.activeElement, appTest.openers[0], "focus returns to the drawer trigger");

  // Plan preparation failure is visible in the drawer, survives toast dismissal
  // and retry, and action-error dismissals are never stored.
  appTest.openers[2].listeners.click[0]();
  await appTest.submitForm(2);
  const errorRegion = appTest.error;
  assert.equal(errorRegion.hidden, false, "form-scoped plan error is visible");
  assert.ok(errorRegion.textContent.includes("Synthetic plan rejected"));
  let actionNotice = appTest.walk(appTest.get("#notifications"), node => node.tagName === "ARTICLE" && appTest.textOf(node).includes("Synthetic plan rejected"))[0];
  assert.ok(actionNotice, "action error also has a nonpersistent notification");
  const dismiss = appTest.walk(actionNotice, node => node.tagName === "BUTTON" && node.textContent === "Dismiss")[0];
  const writesBefore = appTest.writes.length;
  dismiss.listeners.click[0]();
  assert.equal(appTest.writes.length, writesBefore, "action-error dismissal is not remembered");
  await appTest.submitForm(2);
  assert.equal(appTest.error.hidden, false, "retry shows feedback again");
  actionNotice = appTest.walk(appTest.get("#notifications"), node => node.tagName === "ARTICLE" && appTest.textOf(node).includes("Synthetic plan rejected"))[0];
  assert.ok(actionNotice, "retrying the same failure re-notifies after dismissal");
  appTest.closers[0].listeners.click[0]();
  assert.equal(appTest.error.hidden, true, "closing the drawer clears the form error");

  // Accepted mutations invalidate both history caches and force the next load.
  const mutationTest = await bootHarness({ plansFailures: 0 });
  await mutationTest.click("history");
  await mutationTest.click("notebooks");
  mutationTest.openers[2].listeners.click[0]();
  await mutationTest.submitForm(2);
  mutationTest.get("#confirm-plan").checked = true;
  const submitPlan = mutationTest.get("#submit-plan");
  for (const handler of submitPlan.listeners.click || []) await handler();
  await mutationTest.settle();
  assert.ok(mutationTest.textOf(mutationTest.get("#notifications")).includes("History will reload"), "accepted mutation does not claim stale history is current");
  const operationsAfterSubmit = mutationTest.calls.filter(url => url.endsWith("/operations")).length;
  await mutationTest.click("history");
  const operationsAfter = mutationTest.calls.filter(url => url.endsWith("/operations")).length;
  assert.equal(operationsAfter, operationsAfterSubmit + 1, "accepted mutation forces the next history load");

  const keydown = appTest.handlers.keydown && appTest.handlers.keydown[0];
  assert.equal(typeof keydown, "function");
  let prevented = false;
  keydown({ key: "/", target: appTest.get("#refresh"), preventDefault() { prevented = true; }, metaKey: false, ctrlKey: false, altKey: false });
  assert.equal(prevented, true, "the / shortcut focuses the active table search");

  // Slow inventory must not leave History loading or block its own request.
  const raceTest = await bootHarness({ deferInventory: true });
  await raceTest.click("history");
  assert.equal(raceTest.calls.filter(url => url.endsWith("operations")).length, 1, "History fetches while inventory is still pending");
  assert.equal(raceTest.textOf(raceTest.get('[data-table="historyOperations"]')).includes("Loading history"), false, "History leaves Loading once its independent request resolves");
  assert.ok(raceTest.textOf(raceTest.get('[data-table="historyOperations"]')).includes("notebook.stop"), "loaded operation rows render without an ID column");
  raceTest.resolveInventory();
  await raceTest.settle();
  assert.equal(raceTest.calls.filter(url => url.endsWith("inventory")).length, 1);

  // A hidden boot defers the inventory read until visibility returns.
  const hiddenTest = await bootHarness({ hidden: true });
  assert.equal(hiddenTest.calls.filter(url => url.endsWith("inventory")).length, 0, "hidden boot does not wait 30s before loading");
  hiddenTest.setHidden(false);
  await hiddenTest.settle();
  assert.equal(hiddenTest.calls.filter(url => url.endsWith("inventory")).length, 1, "visibilitychange loads inventory immediately");

  // Pod restart requires the confirmed Notebook controller contract.
  const noController = await bootHarness({ notebookWrites: false });
  assert.ok(appTest.textOf(noController.get("#notifications")).includes("Notebook changes are disabled"), "controller policy becomes a dismissible notice");
  const noPodRestart = await bootHarness({ podRestarts: false });
  assert.ok(appTest.textOf(noPodRestart.get("#notifications")).includes("Pod restarts are disabled"), "pod-restart policy becomes a dismissible notice");
  const noNodeCordon = await bootHarness({ nodeCordon: false });
  assert.ok(noNodeCordon.textOf(noNodeCordon.get("#notifications")).includes("Node cordon and uncordon are disabled"), "node cordon policy becomes a dismissible notice");
  await noNodeCordon.click("nodes");
  const disabledCordons = noNodeCordon.walk(noNodeCordon.get('[data-table="nodes"]'), node => node.tagName === "BUTTON" && ["Cordon", "Uncordon"].includes(node.textContent));
  assert.ok(disabledCordons.length > 0 && disabledCordons.every(button => button.disabled), "cordon controls stay disabled when policy denies them");
  await noController.click("pods");
  const disabledRestarts = noController.walk(noController.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.textContent === "Restart");
  assert.ok(disabledRestarts.length > 0 && disabledRestarts.every(button => button.disabled), "pod restart stays disabled without the controller contract");

  // ---- Clear filters --------------------------------------------------------
  // Every table offers a clear control that is only actionable once a search
  // text or a namespace scope is applied, and it resets search, scope and page.
  const filterTest = await bootHarness({});
  await filterTest.click("pods");
  const filterPods = filterTest.get('[data-table="pods"]');
  const filterSearch = filterPods.querySelector("[data-search]");
  const filterClear = filterPods.querySelector("[data-clear-filters]");
  assert.ok(filterSearch && filterClear, "every table exposes a search box and a clear control");
  assert.equal(filterClear.disabled, true, "the clear control is inert while no filter is applied");
  assert.equal(filterClear.attributes["aria-disabled"], "true", "the inert clear control is announced as disabled");
  filterSearch.value = "work-0";
  for (const handler of filterSearch.listeners.input || []) handler();
  assert.equal(filterClear.disabled, false, "an applied search makes the clear control actionable");
  assert.equal(filterClear.attributes["aria-disabled"], "false", "the actionable clear control is announced as enabled");
  assert.ok(filterTest.textOf(filterPods).includes("work-0") && !filterTest.textOf(filterPods).includes("legacy-0"), "the search narrows the table");
  filterClear.listeners.click[0]();
  assert.equal(filterSearch.value, "", "clearing empties the search box");
  assert.equal(filterClear.disabled, true, "the clear control is inert again once the filters are gone");
  assert.ok(filterTest.textOf(filterPods).includes("legacy-0"), "clearing restores every loaded row");
  assert.equal(filterTest.activeElement, filterSearch, "clearing returns keyboard focus to the search box");

  // A namespace scope is a shared filter and clearing it updates the URL.
  const scopeTest = await bootHarness({});
  await scopeTest.click("storage");
  const scopeStorage = scopeTest.get('[data-table="storage"]');
  const scopeSelect = scopeStorage.querySelector("[data-namespace]");
  scopeSelect.value = "team";
  for (const handler of scopeSelect.listeners.change || []) handler();
  await scopeTest.settle();
  assert.equal(scopeTest.location.search, "?namespace=team", "the namespace scope is reflected in the URL");
  const scopeClear = scopeStorage.querySelector("[data-clear-filters]");
  assert.equal(scopeClear.disabled, false, "an applied namespace scope makes the clear control actionable");
  scopeClear.listeners.click[0]();
  assert.equal(scopeTest.location.search, "", "clearing removes the namespace scope from the URL");
  assert.equal(scopeStorage.querySelector("[data-namespace]").value, "", "the namespace control returns to its default");
  // Cluster-wide tables share the same namespace scope, so they offer the same
  // reset even though they never showed a namespace control.
  await scopeTest.click("nodes");
  scopeTest.get("#overview-namespace").value = "team";
  for (const handler of scopeTest.get("#overview-namespace").listeners.change || []) handler();
  await scopeTest.settle();
  assert.equal(scopeTest.location.search, "?namespace=team", "the shared namespace scope is applied to the console");
  const scopeNodes = scopeTest.get('[data-table="nodes"]');
  const scopeNodeClear = scopeNodes.querySelector("[data-clear-filters]");
  assert.equal(scopeNodeClear.disabled, false, "a cluster-wide table offers the same clear control");
  scopeNodeClear.listeners.click[0]();
  assert.equal(scopeTest.location.search, "", "the cluster-wide clear control removes the shared namespace scope");

  // ---- Observed resource details drawer -------------------------------------
  // Names open the right-side modal drawer with the same inert, focus and
  // Escape behaviour. Profiles, Notebooks, Pods and Nodes no longer carry a
  // separate inline Details column or button.
  const detailsTest = await bootHarness({});
  for (const table of ["profiles", "pods", "nodes", "notebooks"]) {
    await detailsTest.click(table);
    const container = detailsTest.get(`[data-table="${table}"]`);
    assert.equal(detailsTest.walk(container, node => node.tagName === "DETAILS").length, 0, `${table} no longer uses an inline details disclosure`);
    assert.equal(detailsTest.walk(container, node => node.tagName === "BUTTON" && node.textContent === "Details").length, 0, `${table} removes the separate Details control`);
  }
  await detailsTest.click("storage");
  assert.ok(detailsTest.walk(detailsTest.get('[data-table="storage"]'), node => node.tagName === "BUTTON" && node.textContent === "Details").length > 0, "storage keeps its existing details control");
  await detailsTest.click("nodes");
  const nodeName = detailsTest.walk(detailsTest.get('[data-table="nodes"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Node node-1")[0];
  assert.ok(nodeName && nodeName.textContent === "node-1", "the Node name opens its resource details");
  nodeName.click();
  assert.equal(detailsTest.get("#resource-details-drawer").hidden, false, "the Node name opens the right-side resource details");
  assert.ok(detailsTest.textOf(detailsTest.get("#resource-details-body")).includes("containerd://2.0"), "the Node details keep the observed facts");
  detailsTest.closers[0].listeners.click[0]();
  await detailsTest.click("pods");
  const detailsPods = detailsTest.get('[data-table="pods"]');
  const detailsButton = detailsTest.walk(detailsPods, node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  assert.ok(detailsButton, "the Pod name is the control that opens its own details");
  assert.equal(detailsButton.textContent, "work-0", "the Pod name itself opens the details drawer");
  assert.equal(detailsButton.attributes["aria-label"], "Details for Pod work-0", "the name control names the observed resource");
  // A pointer click does not always move the focus. The clicked control is the
  // opener, not whatever happened to be focused before.
  detailsButton.click();
  const detailsDrawer = detailsTest.get("#resource-details-drawer");
  assert.equal(detailsDrawer.hidden, false, "the resource details drawer opens on the right");
  assert.equal(detailsTest.get("#drawer-scrim").hidden, false, "the drawer covers the background with a scrim");
  assert.equal(detailsTest.get("main").inert, true, "the background is inert while the drawer is open");
  assert.equal(detailsTest.get(".sidebar").inert, true, "the sidebar is inert while the drawer is open");
  assert.equal(detailsTest.textOf(detailsTest.get("#resource-details-title")).trim(), "work-0", "the drawer title is the resource name");
  const detailsText = detailsTest.textOf(detailsTest.get("#resource-details-body"));
  assert.ok(detailsText.includes("node: node-1"), "the projected details keep their labels");
  assert.ok(detailsText.includes("Pod") && detailsText.includes("team"), "the drawer names the observed kind and namespace");
  assert.ok(detailsText.includes("StatefulSet/work"), "the observed owner is shown");
  detailsTest.handlers.keydown[0]({ key: "Escape", preventDefault() {} });
  assert.equal(detailsDrawer.hidden, true, "Escape closes the resource details drawer");
  assert.equal(detailsTest.get("main").inert, false, "the background is interactive again");
  assert.equal(detailsTest.activeElement, detailsButton, "focus returns to the clicked control even when it never had the focus");

  // A refresh replaces the row. The opener is gone, so the focus may only go
  // back to the control of the very same observed resource.
  detailsButton.click();
  detailsTest.get("#refresh").listeners.click[0]();
  await detailsTest.settle();
  assert.equal(detailsDrawer.hidden, false, "a refresh never closes the details drawer");
  const refreshedButton = detailsTest.walk(detailsTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  assert.ok(refreshedButton && refreshedButton !== detailsButton, "the refreshed row replaces the control");
  detailsTest.get("#drawer-scrim").listeners.click[0]();
  assert.equal(detailsDrawer.hidden, true, "the scrim closes the details drawer");
  assert.equal(detailsTest.activeElement, refreshedButton, "a stale opener never focuses the first details control of the document");
  assert.notEqual(detailsTest.activeElement, detailsTest.get("#page-title"), "the exact resource control takes the focus back");

  // A replaced resource is reported: the drawer never shows an old body as if
  // it were the latest observation.
  const replacedTest = await bootHarness({});
  await replacedTest.click("pods");
  const replacedButton = replacedTest.walk(replacedTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  replacedButton.click();
  replacedTest.setResources(replacedTest.resources().map(resource =>
    resource.kind === "Pod" && resource.name === "work-0" && resource.namespace === "team" ? { ...resource, uid: "uid-pod-team-work-0-replaced" } : resource));
  replacedTest.get("#refresh").listeners.click[0]();
  await replacedTest.settle();
  assert.equal(replacedTest.get("#resource-details-drawer").hidden, false, "a replaced resource keeps the drawer open");
  assert.ok(replacedTest.textOf(replacedTest.get("#resource-details-body")).includes("replaced"), "a replaced resource is reported in the drawer");

  // A resource that is gone is reported in the drawer instead of being shown
  // as if it were the latest observation.
  const goneTest = await bootHarness({});
  await goneTest.click("pods");
  goneTest.walk(goneTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0].click();
  goneTest.setResources(goneTest.resources().filter(resource => !(resource.kind === "Pod" && resource.name === "work-0" && resource.namespace === "team")));
  goneTest.get("#refresh").listeners.click[0]();
  await goneTest.settle();
  assert.equal(goneTest.get("#resource-details-drawer").hidden, false, "a refresh never closes the details drawer");
  const goneText = goneTest.textOf(goneTest.get("#resource-details-body"));
  assert.ok(goneText.includes("no longer present"), "a resource that is gone is reported in the drawer");
  assert.equal(goneText.includes("latest inventory observation"), false, "a stale body never claims to be the latest observation");
  assert.ok(goneTest.textOf(goneTest.get("#notifications")).includes("no longer present"), "the missing resource is also reported as a notice");

  // ---- Notebooks are the Notebook custom resource only ----------------------
  const notebookTest = await bootHarness({});
  await notebookTest.click("notebooks");
  const notebookTable = notebookTest.get('[data-table="notebooks"]');
  const notebookText = notebookTest.textOf(notebookTable);
  assert.equal(notebookTest.walk(notebookTable, node => node.tagName === "TR").length, 2, "only the observed Notebook CR is listed");
  assert.ok(notebookText.includes("Notebook") && notebookText.includes("work"), "the Notebook row is shown");
  assert.equal(notebookText.includes("StatefulSet"), false, "a same-name StatefulSet is never a Notebook row");
  assert.equal(notebookText.includes("work-0"), false, "a same-name Pod is never a Notebook row");
  assert.equal(notebookTest.walk(notebookTable, node => node.tagName === "BUTTON" && node.textContent === "Details").length, 0, "the Notebooks table has no separate Details control");
  const notebookName = notebookTest.walk(notebookTable, node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0];
  assert.ok(notebookName && notebookName.textContent === "work", "the Notebook name opens its resource details");
  notebookName.click();
  assert.equal(notebookTest.get("#resource-details-drawer").hidden, false, "the Notebook name opens the right-side resource details");
  assert.ok(notebookTest.textOf(notebookTest.get("#resource-details-body")).includes("image.main"), "the Notebook details show the observed container image");
  const manageNotebook = notebookTest.walk(notebookTest.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0];
  assert.ok(manageNotebook, "the Notebook resource details offer Manage Notebook");
  assert.equal(manageNotebook.disabled, false, "Manage Notebook is enabled when writes and the controller contract are confirmed");
  manageNotebook.click();
  assert.equal(notebookTest.get("#notebook-drawer").hidden, false, "Manage Notebook opens the Notebook management drawer");
  assert.equal(notebookTest.get("#resource-details-drawer").hidden, true, "the resource details drawer hands over to Notebook management");
  assert.equal(notebookTest.get('#notebook-form [name="namespace"]').value, "team", "the exact Notebook namespace is preselected");
  assert.equal(notebookTest.get("#notebook-selector").value, "work", "the exact Notebook name is preselected");
  assert.equal(notebookTest.get("#container-selector").value, "main", "the exact observed container is preselected");
  assert.equal(notebookTest.get("#notebook-kind").value, "notebook.start", "the selected action is not silently switched to Edit");
  assert.equal(notebookTest.get("#notebook-target-warning").hidden, true, "a bound target starts without a warning");
  notebookTest.closers[0].listeners.click[0]();
  assert.equal(notebookTest.get("#notebook-drawer").hidden, true, "the management drawer closes");
  assert.equal(notebookTest.activeElement, notebookName, "closing Notebook management restores the exact visible Notebook row control");
  assert.notEqual(notebookTest.activeElement, manageNotebook, "the focus never returns to a control inside the closed details drawer");

  // Notebook management follows the existing policy gates.
  const noNotebookContract = await bootHarness({ notebookWrites: false });
  await noNotebookContract.click("notebooks");
  noNotebookContract.walk(noNotebookContract.get('[data-table="notebooks"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0].click();
  const contractManage = noNotebookContract.walk(noNotebookContract.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0];
  assert.ok(contractManage && contractManage.disabled, "Manage Notebook stays disabled without the confirmed controller contract");
  assert.ok(String(contractManage.title).includes("controller contract"), "the disabled action explains the contract gate");
  noNotebookContract.closers[0].listeners.click[0]();
  noNotebookContract.openers[2].listeners.click[0]();
  assert.equal(noNotebookContract.get("#notebook-drawer").hidden, true, "the page-level Notebook opener is defensively refused without the controller contract");
  const noWrites = await bootHarness({ writes: false });
  await noWrites.click("notebooks");
  noWrites.walk(noWrites.get('[data-table="notebooks"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0].click();
  const writesManage = noWrites.walk(noWrites.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0];
  assert.ok(writesManage && writesManage.disabled, "Manage Notebook stays disabled when writes are disabled by policy");
  assert.ok(String(writesManage.title).includes("disabled by server policy"), "the disabled action explains the write policy gate");
  noWrites.closers[0].listeners.click[0]();
  noWrites.openers[2].listeners.click[0]();
  assert.equal(noWrites.get("#notebook-drawer").hidden, true, "the page-level Notebook opener is defensively refused when writes are disabled");

  // A refresh that replaces the bound Notebook is reported and refused before
  // any request; the form is never retargeted to another Notebook.
  const notebookRefreshTest = await bootHarness({ resources: [
    { kind: "Profile", name: "team", uid: "uid-profile-team", state: "Active", owner: "alice@example.test", details: { configDigest: "sha256:teamdigest", "quota.requests.cpu": "4" } },
    { kind: "Namespace", name: "team", uid: "uid-namespace-team", state: "Active" },
    { kind: "Notebook", name: "work", namespace: "team", uid: "uid-notebook-work", state: "Running / Ready", details: { "image.main": "image" } },
    { kind: "Notebook", name: "other-work", namespace: "team", uid: "uid-notebook-other", state: "Running / Ready", details: { "image.other": "image" } }
  ] });
  await notebookRefreshTest.click("notebooks");
  notebookRefreshTest.walk(notebookRefreshTest.get('[data-table="notebooks"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0].click();
  const refreshManage = notebookRefreshTest.walk(notebookRefreshTest.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0];
  refreshManage.click();
  assert.equal(notebookRefreshTest.get("#notebook-selector").value, "work", "the selected resource is the exact Notebook, not the other one");
  // An explicit selection of another observed Notebook is a new target.
  const notebookSelect = notebookRefreshTest.get("#notebook-selector");
  notebookSelect.value = "other-work";
  for (const handler of notebookSelect.listeners.change || []) handler();
  assert.equal(notebookRefreshTest.get('#notebook-form [name="namespace"]').value, "team", "the namespace stays the observed one");
  // Reopen the first Notebook, then replace it under the open form.
  notebookSelect.value = "work";
  for (const handler of notebookSelect.listeners.change || []) handler();
  notebookRefreshTest.setResources(notebookRefreshTest.resources().map(resource =>
    resource.kind === "Notebook" && resource.name === "work" ? { ...resource, uid: "uid-notebook-work-replaced" } : resource));
  notebookRefreshTest.get("#refresh").listeners.click[0]();
  await notebookRefreshTest.settle();
  assert.equal(notebookRefreshTest.get("#notebook-target-warning").hidden, false, "a replaced bound Notebook is visible in the drawer");
  assert.ok(notebookRefreshTest.textOf(notebookRefreshTest.get("#notebook-target-warning")).includes("replaced"), "the warning names the replacement");
  assert.equal(notebookRefreshTest.get("#notebook-selector").value, "work", "a refresh never silently retargets the form to another Notebook");
  const notebookBefore = notebookRefreshTest.bodies.length;
  await notebookRefreshTest.submitForm(2);
  assert.equal(notebookRefreshTest.bodies.length, notebookBefore, "a replaced Notebook never reaches the API");
  assert.ok(notebookRefreshTest.error.textContent.includes("replaced"), "the refusal names the replaced target");

  // A resource that disappears under the open details drawer cannot be managed.
  const notebookGoneTest = await bootHarness({});
  await notebookGoneTest.click("notebooks");
  notebookGoneTest.walk(notebookGoneTest.get('[data-table="notebooks"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0].click();
  notebookGoneTest.setResources(notebookGoneTest.resources().filter(resource => !(resource.kind === "Notebook" && resource.name === "work")));
  notebookGoneTest.get("#refresh").listeners.click[0]();
  await notebookGoneTest.settle();
  const goneManage = notebookGoneTest.walk(notebookGoneTest.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0];
  assert.ok(goneManage && goneManage.disabled, "a Notebook that is gone cannot be managed");

  // Changing the namespace, Notebook or container never carries the previous
  // Notebook's typed resources, mounts, restart or replace flag into another
  // target. Blank fields keep their existing "preserve" meaning.
  const notebookBleedTest = await bootHarness({ resources: [
    { kind: "Namespace", name: "team", uid: "uid-namespace-team", state: "Active" },
    { kind: "Namespace", name: "team-two", uid: "uid-namespace-team-two", state: "Active" },
    { kind: "Notebook", name: "work", namespace: "team", uid: "uid-notebook-work", state: "Running / Ready", details: { "image.main": "image" } },
    { kind: "Notebook", name: "other-work", namespace: "team-two", uid: "uid-notebook-other", state: "Running / Ready", details: { "image.main": "image", "image.sidecar": "proxy" } }
  ] });
  await notebookBleedTest.click("notebooks");
  notebookBleedTest.walk(notebookBleedTest.get('[data-table="notebooks"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0].click();
  notebookBleedTest.walk(notebookBleedTest.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0].click();
  const bleedKind = notebookBleedTest.get("#notebook-kind");
  bleedKind.value = "notebook.edit";
  for (const handler of bleedKind.listeners.change || []) handler();
  const bleedField = selector => notebookBleedTest.get(selector);
  bleedField('#notebook-form [name="cpuRequest"]').value = "4";
  bleedField('#notebook-form [name="cpuLimit"]').value = "6";
  bleedField('#notebook-form [name="memoryRequest"]').value = "1Gi";
  bleedField('#notebook-form [name="memoryLimit"]').value = "2Gi";
  bleedField('#notebook-form [name="image"]').value = "registry.example/work:v1";
  bleedField('#notebook-form [name="gpuPreset"]').value = "gpu";
  bleedField('#notebook-form [name="gpuCount"]').value = "2";
  bleedField('#notebook-form [name="restart"]').checked = true;
  notebookBleedTest.get("#replace-mounts").checked = true;
  // Namespace change clears every edit value before the new options are built.
  const bleedNamespace = bleedField('#notebook-form [name="namespace"]');
  bleedNamespace.value = "team-two";
  for (const handler of bleedNamespace.listeners.change || []) handler();
  assert.equal(bleedField('#notebook-form [name="cpuRequest"]').value, "", "a namespace change clears the previous CPU request");
  assert.equal(bleedField('#notebook-form [name="image"]').value, "", "a namespace change clears the previous image");
  assert.equal(bleedField('#notebook-form [name="gpuCount"]').value, "", "a namespace change clears the previous GPU count");
  assert.equal(bleedField('#notebook-form [name="restart"]').checked, false, "a namespace change clears the previous restart flag");
  assert.equal(notebookBleedTest.get("#replace-mounts").checked, false, "a namespace change clears the previous replace-mounts flag");
  // An explicit new Notebook selection binds the new observed UID.
  const bleedNotebook = notebookBleedTest.get("#notebook-selector");
  bleedNotebook.value = "other-work";
  for (const handler of bleedNotebook.listeners.change || []) handler();
  assert.equal(bleedField('#notebook-form [name="namespace"]').value, "team-two", "the selected namespace is kept");
  assert.equal(notebookBleedTest.get("#notebook-target-warning").hidden, true, "an explicit new Notebook choice binds the current observed UID");
  // A container change preserves the chosen container but clears its fields.
  const bleedContainer = notebookBleedTest.get("#container-selector");
  bleedContainer.value = "sidecar";
  for (const handler of bleedContainer.listeners.change || []) handler();
  assert.equal(bleedField('#notebook-form [name="cpuRequest"]').value, "", "a container change clears the previous CPU request");
  assert.equal(bleedContainer.value, "sidecar", "a container change keeps the newly chosen container");
  bleedField('#notebook-form [name="cpuRequest"]').value = "3";
  // Switch back to the blank preserve contract: changing the container again
  // drops the value just typed for the other container.
  bleedContainer.value = "main";
  for (const handler of bleedContainer.listeners.change || []) handler();
  assert.equal(bleedField('#notebook-form [name="cpuRequest"]').value, "", "another container never inherits the previous container's resource edit");
  await notebookBleedTest.submitForm(2);
  const bleedBody = notebookBleedTest.bodies[notebookBleedTest.bodies.length - 1];
  assert.equal(bleedBody.kind, "notebook.edit", "the explicitly selected action is preserved");
  assert.equal(bleedBody.namespace, "team-two", "the plan targets the newly selected namespace");
  assert.equal(bleedBody.name, "other-work", "the plan targets the newly selected Notebook");
  assert.equal(bleedBody.container, "main", "the plan targets the newly selected container");
  for (const key of ["cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "image", "gpuPreset", "gpuCount"]) {
    assert.equal(bleedBody[key], undefined, "the old " + key + " is never sent to another target");
  }
  assert.equal(bleedBody.restart, false, "the old restart flag is never sent to another target");
  assert.equal(bleedBody.mounts, undefined, "the old mount replacement is never sent to another target");

  // Reopening the page-level control after a closed details drawer starts a
  // fresh context: stale values, reason and error are cleared and the current
  // observed UID is bound, so no stale target can be submitted.
  const notebookReopenTest = await bootHarness({ resources: [
    { kind: "Namespace", name: "team", uid: "uid-namespace-team", state: "Active" },
    { kind: "Notebook", name: "work", namespace: "team", uid: "uid-notebook-work", state: "Running / Ready", details: { "image.main": "image" } }
  ] });
  await notebookReopenTest.click("notebooks");
  notebookReopenTest.walk(notebookReopenTest.get('[data-table="notebooks"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Notebook work")[0].click();
  notebookReopenTest.walk(notebookReopenTest.get("#resource-details-body"), node => node.tagName === "BUTTON" && node.textContent === "Manage Notebook")[0].click();
  const reopenKind = notebookReopenTest.get("#notebook-kind");
  reopenKind.value = "notebook.edit";
  for (const handler of reopenKind.listeners.change || []) handler();
  notebookReopenTest.get('#notebook-form [name="cpuRequest"]').value = "8";
  notebookReopenTest.get('#notebook-form [name="gpuCount"]').value = "4";
  notebookReopenTest.get('#notebook-form [name="restart"]').checked = true;
  notebookReopenTest.get('#notebook-form [name="reason"]').value = "old typed reason";
  notebookReopenTest.error.textContent = "old form error";
  notebookReopenTest.error.hidden = false;
  notebookReopenTest.closers[0].listeners.click[0]();
  // The observation replaces the Notebook under the same name.
  notebookReopenTest.setResources(notebookReopenTest.resources().map(resource =>
    resource.kind === "Notebook" && resource.name === "work" ? { ...resource, uid: "uid-notebook-work-fresh" } : resource));
  notebookReopenTest.get("#refresh").listeners.click[0]();
  await notebookReopenTest.settle();
  notebookReopenTest.openers[2].listeners.click[0]();
  assert.equal(notebookReopenTest.get("#notebook-drawer").hidden, false, "the page-level Notebook opener opens a fresh management context");
  assert.equal(notebookReopenTest.get('#notebook-form [name="cpuRequest"]').value, "", "stale CPU edits are cleared on a fresh management open");
  assert.equal(notebookReopenTest.get('#notebook-form [name="gpuCount"]').value, "", "stale GPU edits are cleared on a fresh management open");
  assert.equal(notebookReopenTest.get('#notebook-form [name="restart"]').checked, false, "a stale restart flag is cleared on a fresh management open");
  assert.equal(notebookReopenTest.get('#notebook-form [name="reason"]').value, "", "a stale reason is cleared on a fresh management open");
  assert.equal(notebookReopenTest.error.hidden, true, "a stale form error is cleared on a fresh management open");
  assert.equal(notebookReopenTest.get("#notebook-target-warning").hidden, true, "the fresh observation is bound without a warning");
  await notebookReopenTest.submitForm(2);
  const reopenBody = notebookReopenTest.bodies[notebookReopenTest.bodies.length - 1];
  assert.equal(reopenBody.kind, "notebook.edit", "the explicitly selected action survives the fresh open");
  assert.equal(reopenBody.namespace, "team", "the fresh open keeps the observed namespace");
  assert.equal(reopenBody.name, "work", "the fresh open keeps the observed Notebook name");
  for (const key of ["cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "image", "gpuPreset", "gpuCount"]) {
    assert.equal(reopenBody[key], undefined, "a fresh open never resubmits the stale " + key);
  }
  assert.equal(reopenBody.restart, false, "a fresh open never resubmits a stale restart");
  assert.equal(reopenBody.reason, undefined, "a fresh open never resubmits a stale reason");

  // Storage claim and volume names open the same observed-details drawer.
  const storageTest = await bootHarness({});
  await storageTest.click("storage");
  const storageContainer = storageTest.get('[data-table="storage"]');
  const claimName = storageTest.walk(storageContainer, node => node.tagName === "BUTTON" && node.textContent === "data" && node.attributes["aria-label"] === "Details for PersistentVolumeClaim data")[0];
  assert.ok(claimName, "a storage claim name is a control");
  claimName.click();
  assert.equal(storageTest.get("#resource-details-drawer").hidden, false, "the storage name opens the resource details drawer");
  assert.ok(storageTest.textOf(storageTest.get("#resource-details-body")).includes("storageClass: team-class"), "the storage details keep their labels");
  storageTest.closers[0].listeners.click[0]();
  const volumeName = storageTest.walk(storageContainer, node => node.tagName === "BUTTON" && node.textContent === "data" && node.attributes["aria-label"] === "Details for PersistentVolume data")[0];
  assert.ok(volumeName, "a storage volume name is a control");
  volumeName.click();
  assert.equal(storageTest.get("#resource-details-drawer").hidden, false, "the volume name opens the resource details drawer");
  assert.ok(storageTest.textOf(storageTest.get("#resource-details-body")).includes("claim: team/data"), "the volume details keep their labels");

  // ---- Namespace summary resource drilldown ---------------------------------
  // Individual resources lead to the scoped page filtered to that resource,
  // while the group counts and scoped views keep working. The identity is the
  // canonical triple kind, namespace and name.
  const scopedTest = await bootHarness({});
  await scopedTest.click("namespaces");
  const scopedNamespace = scopedTest.walk(scopedTest.get('[data-table="namespaces"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  scopedNamespace.click();
  const scopedBody = scopedTest.get("#namespace-summary-body");
  assert.ok(scopedTest.textOf(scopedBody).includes("Scoped views"), "the scoped group links continue to work");
  const linkTo = href => scopedTest.walk(scopedBody, node => node.tagName === "A" && node.href === href)[0];
  const scopedNotebook = linkTo("/notebooks?namespace=team&kind=Notebook&resource=work");
  assert.ok(scopedNotebook, "the namespace summary links an individual notebook");
  assert.ok(linkTo("/?namespace=team&kind=StatefulSet&resource=work"), "a StatefulSet links to the Overview table, not the Notebooks page");
  assert.ok(linkTo("/pods?namespace=team&kind=Pod&resource=work-0"), "the namespace summary links an individual pod");
  assert.ok(linkTo("/storage?namespace=team&kind=PersistentVolumeClaim&resource=data"), "the namespace summary links an individual claim");
  assert.ok(linkTo("/?namespace=team&kind=Event&resource=scheduled"), "the namespace summary links an individual event");
  assert.ok(linkTo("/?namespace=team&kind=ResourceQuota&resource=scheduled"), "the namespace summary links an individual quota");
  for (const handler of scopedTest.handlers.click || []) handler({ target: scopedNotebook, defaultPrevented: false, button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, preventDefault() {} });
  await scopedTest.settle();
  assert.equal(scopedTest.location.pathname, "/notebooks", "the resource link opens the scoped page");
  assert.ok(scopedTest.location.search.includes("namespace=team") && scopedTest.location.search.includes("kind=Notebook") && scopedTest.location.search.includes("resource=work"), "the URL keeps the namespace scope and the resource identity");
  const scopedNotebooks = scopedTest.get('[data-table="notebooks"]');
  assert.equal(scopedNotebooks.querySelector("[data-search]").value, "work", "the scoped page shows the resource focus in the visible filter");
  assert.equal(scopedTest.walk(scopedNotebooks, node => node.tagName === "TR").length, 2, "only the selected Notebook remains, not the StatefulSet with the same name");
  const scopedClear = scopedNotebooks.querySelector("[data-clear-filters]");
  assert.equal(scopedClear.disabled, false, "the scoped page offers an actionable clear control");
  scopedClear.listeners.click[0]();
  assert.equal(scopedNotebooks.querySelector("[data-search]").value, "", "clearing removes the resource focus from the visible filter");
  assert.equal(scopedTest.location.search, "", "clearing removes the namespace scope and the resource identity from the URL");

  // A StatefulSet kind deep link lands on the Overview table, which keeps the
  // aggregate StatefulSet observation. It never opens an empty Notebooks view.
  const statefulScopeTest = await bootHarness({ path: "/?namespace=team&kind=StatefulSet&resource=work" });
  await statefulScopeTest.settle();
  assert.equal(statefulScopeTest.get("#overview-table").hidden, false, "a StatefulSet resource link expands the Overview table");
  const statefulOverview = statefulScopeTest.get('[data-table="overview"]');
  assert.equal(statefulScopeTest.walk(statefulOverview, node => node.tagName === "TR").length, 2, "only the selected StatefulSet remains");
  assert.ok(statefulScopeTest.textOf(statefulOverview).includes("StatefulSet"), "the selected resource is the StatefulSet");

  // A claim is never confused with the volume or the storage class that share
  // its name.
  const claimScopeTest = await bootHarness({});
  await claimScopeTest.click("namespaces");
  claimScopeTest.walk(claimScopeTest.get('[data-table="namespaces"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0].click();
  const claimLink = claimScopeTest.walk(claimScopeTest.get("#namespace-summary-body"), node => node.tagName === "A" && node.href === "/storage?namespace=team&kind=PersistentVolumeClaim&resource=data")[0];
  for (const handler of claimScopeTest.handlers.click || []) handler({ target: claimLink, defaultPrevented: false, button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, preventDefault() {} });
  await claimScopeTest.settle();
  const claimTable = claimScopeTest.get('[data-table="storage"]');
  assert.equal(claimScopeTest.walk(claimTable, node => node.tagName === "TR").length, 2, "only the claim of the selected kind remains");
  assert.ok(claimScopeTest.textOf(claimTable).includes("PersistentVolumeClaim"), "the selected kind is the claim");
  assert.equal(claimScopeTest.textOf(claimTable).includes("PersistentVolume ·"), false, "the volume with the same name never leaks into the claim");

  // The same Pod name in another namespace is a different Pod.
  const podScopeTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await podScopeTest.settle();
  const podTable = podScopeTest.get('[data-table="pods"]');
  assert.equal(podScopeTest.walk(podTable, node => node.tagName === "TR").length, 2, "only the Pod of the selected namespace remains");
  const otherPodScope = await bootHarness({ path: "pods?namespace=other&kind=Pod&resource=work-0" });
  await otherPodScope.settle();
  const otherPodTable = otherPodScope.get('[data-table="pods"]');
  assert.equal(otherPodScope.walk(otherPodTable, node => node.tagName === "TR").length, 2, "the Pod of the other namespace is a different resource");
  assert.ok(otherPodScope.textOf(otherPodTable).includes("node-2"), "the other namespace keeps its own observation");

  // A deep link focuses the requested resource when the page boots.
  const deepLinkTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await deepLinkTest.settle();
  const deepPods = deepLinkTest.get('[data-table="pods"]');
  assert.equal(deepPods.querySelector("[data-search]").value, "work-0", "a deep link focuses the requested resource in the visible filter");
  assert.equal(deepLinkTest.walk(deepPods, node => node.tagName === "TR").length, 2, "a deep link shows only the requested resource");

  // A cluster-wide page identifies its resource by its own kind.
  const nodeDeepLinkTest = await bootHarness({ path: "nodes?resource=node-1" });
  await nodeDeepLinkTest.settle();
  const deepNodes = nodeDeepLinkTest.get('[data-table="nodes"]');
  assert.equal(deepNodes.querySelector("[data-search]").value, "node-1", "a node deep link mirrors the name in the visible filter");
  assert.equal(nodeDeepLinkTest.walk(deepNodes, node => node.tagName === "TR").length, 2, "a node deep link shows exactly the requested node");
  assert.ok(nodeDeepLinkTest.textOf(deepNodes).includes("node-1") && !nodeDeepLinkTest.textOf(deepNodes).includes("node-2"), "only the requested node is shown");

  // A resource URL on the Overview page expands the detailed table, however
  // the page was reached.
  const overviewDeepTest = await bootHarness({ path: "/?namespace=team&kind=Event&resource=scheduled" });
  await overviewDeepTest.settle();
  assert.equal(overviewDeepTest.get("#overview-table").hidden, false, "a resource URL expands the detailed overview table");
  assert.equal(overviewDeepTest.walk(overviewDeepTest.get('[data-table="overview"]'), node => node.tagName === "TR").length, 2, "the overview table is filtered to the selected resource");
  const overviewQuotaTest = await bootHarness({ path: "/?namespace=team&kind=ResourceQuota&resource=scheduled" });
  await overviewQuotaTest.settle();
  assert.equal(overviewQuotaTest.walk(overviewQuotaTest.get('[data-table="overview"]'), node => node.tagName === "TR").length, 2, "the quota with the same name is a different resource");
  assert.ok(overviewQuotaTest.textOf(overviewQuotaTest.get('[data-table="overview"]')).includes("ResourceQuota"), "the selected kind is the quota");

  // A quota or event link from the namespace summary opens the scoped Overview
  // table filtered to that resource.
  const overviewScopeTest = await bootHarness({});
  await overviewScopeTest.click("namespaces");
  overviewScopeTest.walk(overviewScopeTest.get('[data-table="namespaces"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0].click();
  const eventLink = overviewScopeTest.walk(overviewScopeTest.get("#namespace-summary-body"), node => node.tagName === "A" && node.href === "/?namespace=team&kind=Event&resource=scheduled")[0];
  assert.ok(eventLink, "the namespace summary links an individual event");
  for (const handler of overviewScopeTest.handlers.click || []) handler({ target: eventLink, defaultPrevented: false, button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, preventDefault() {} });
  await overviewScopeTest.settle();
  assert.equal(overviewScopeTest.location.pathname, "/", "the event link opens the Overview page");
  assert.equal(overviewScopeTest.get("#overview-table").hidden, false, "the detailed overview table is expanded for a scoped event link");
  assert.equal(overviewScopeTest.walk(overviewScopeTest.get('[data-table="overview"]'), node => node.tagName === "TR").length, 2, "the overview table is filtered to the selected resource");

  // ---- Focus and URL coherence ---------------------------------------------
  // A focus never outlives the URL that named it, and a typed search is never
  // overwritten by a focus.
  const focusTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await focusTest.settle();
  const focusSearch = focusTest.get('[data-table="pods"]').querySelector("[data-search]");
  assert.equal(focusSearch.value, "work-0", "the deep link is mirrored in the visible filter");
  await focusTest.popstate("/pods?namespace=team");
  assert.equal(focusSearch.value, "", "the mirrored focus search is cleared with the focus");
  assert.ok(focusTest.textOf(focusTest.get('[data-table="pods"]')).includes("legacy-0"), "the deep link no longer narrows the table");
  assert.equal(focusTest.get('[data-table="pods"]').querySelector("[data-clear-filters]").disabled, false, "the namespace scope keeps the clear control actionable");

  // A typed search is not a focus mirror and survives a navigation.
  const typedTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await typedTest.settle();
  const typedSearch = typedTest.get('[data-table="pods"]').querySelector("[data-search]");
  typedSearch.value = "legacy";
  for (const handler of typedSearch.listeners.input || []) handler();
  assert.equal(typedSearch.value, "legacy");
  await typedTest.popstate("/pods?namespace=team");
  assert.equal(typedSearch.value, "legacy", "an unrelated typed search survives a navigation");
  assert.ok(typedTest.textOf(typedTest.get('[data-table="pods"]')).includes("legacy-0"), "the typed search still filters the rows");

  // The sidebar drops the resource identity, and the focus goes with it.
  const sidebarTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await sidebarTest.settle();
  await sidebarTest.click("pods");
  assert.equal(sidebarTest.location.search, "?namespace=team", "the sidebar link carries no resource identity");
  assert.equal(sidebarTest.get('[data-table="pods"]').querySelector("[data-search]").value, "", "the focus does not survive a sidebar navigation");
  assert.ok(sidebarTest.textOf(sidebarTest.get('[data-table="pods"]')).includes("legacy-0"), "every row of the scope is shown again");

  // A navigation to another page and back restores the deep link.
  const backTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await backTest.settle();
  await backTest.click("storage");
  await backTest.popstate("/pods?namespace=team&kind=Pod&resource=work-0");
  assert.equal(backTest.get('[data-table="pods"]').querySelector("[data-search]").value, "work-0", "the deep link is applied again after a navigation back");
  assert.equal(backTest.walk(backTest.get('[data-table="pods"]'), node => node.tagName === "TR").length, 2, "only the requested resource is shown again");
  assert.equal(backTest.activeElement, backTest.get("#page-title"), "a popstate navigation moves the focus to the page heading");

  // A namespace change drops the resource focus: the new scope shows its own
  // rows instead of the resource of the old scope.
  const rescopeTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await rescopeTest.settle();
  const rescopeSelect = rescopeTest.get('[data-table="pods"]').querySelector("[data-namespace]");
  rescopeSelect.value = "other";
  for (const handler of rescopeSelect.listeners.change || []) handler();
  await rescopeTest.settle();
  assert.equal(rescopeTest.location.search, "?namespace=other", "the namespace scope is reflected in the URL");
  assert.equal(rescopeTest.get('[data-table="pods"]').querySelector("[data-search]").value, "", "the focus of the old scope is dropped");
  assert.ok(rescopeTest.textOf(rescopeTest.get('[data-table="pods"]')).includes("node-2"), "the new scope shows its own rows");

  // A cached focus of another table never outlives the scope it belongs to.
  const cachedTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0" });
  await cachedTest.settle();
  await cachedTest.click("storage");
  assert.equal(cachedTest.get('[data-table="pods"]').querySelector("[data-search]").value, "work-0", "the cached focus of the other table is still there");
  const cachedSelect = cachedTest.get('[data-table="storage"]').querySelector("[data-namespace]");
  cachedSelect.value = "other";
  for (const handler of cachedSelect.listeners.change || []) handler();
  await cachedTest.settle();
  await cachedTest.click("pods");
  assert.equal(cachedTest.get('[data-table="pods"]').querySelector("[data-search]").value, "", "a namespace change drops the cached focus of every table");
  assert.equal(cachedTest.walk(cachedTest.get('[data-table="pods"]'), node => node.tagName === "TR").length, 2, "every row of the new scope is shown again");
  assert.ok(cachedTest.textOf(cachedTest.get('[data-table="pods"]')).includes("node-2"), "the new scope shows its own observation");

  // Clearing removes the identity from the URL and keeps every other
  // parameter.
  const preserveTest = await bootHarness({ path: "pods?namespace=team&kind=Pod&resource=work-0&view=compact" });
  await preserveTest.settle();
  preserveTest.get('[data-table="pods"]').querySelector("[data-clear-filters]").listeners.click[0]();
  assert.ok(preserveTest.location.search.includes("view=compact"), "an unrelated query parameter is preserved");
  assert.equal(preserveTest.location.search.includes("resource="), false, "the resource identity is removed from the URL");
  assert.equal(preserveTest.location.search.includes("kind="), false, "the kind is removed from the URL");
  assert.equal(preserveTest.get('[data-table="pods"]').querySelector("[data-search]").value, "", "the mirrored search is cleared with the identity");
  // The same reset without a namespace scope drops the identity as well.
  const unscopedTest = await bootHarness({ path: "pods?kind=Pod&resource=work-0&view=compact" });
  await unscopedTest.settle();
  unscopedTest.get('[data-table="pods"]').querySelector("[data-clear-filters]").listeners.click[0]();
  assert.ok(unscopedTest.location.search.includes("view=compact"), "an unrelated query parameter is preserved without a namespace scope");
  assert.equal(unscopedTest.location.search.includes("resource="), false, "the resource identity is removed from the URL without a namespace scope");
  assert.equal(unscopedTest.location.search.includes("kind="), false, "the kind is removed from the URL without a namespace scope");

  // A legacy link without a kind keeps working as a name filter.
  const legacyTest = await bootHarness({ path: "pods?namespace=team&resource=work-0" });
  await legacyTest.settle();
  assert.equal(legacyTest.get('[data-table="pods"]').querySelector("[data-search]").value, "work-0", "a legacy link still mirrors the name");
  assert.equal(legacyTest.walk(legacyTest.get('[data-table="pods"]'), node => node.tagName === "TR").length, 2, "a legacy link filters by the inferred kind of the page");

  // ---- Focus eligibility ----------------------------------------------------
  // A closed drawer keeps its DOM. A control that is connected but hidden may
  // never take the focus back.
  const hiddenOpenerTest = await bootHarness({});
  await hiddenOpenerTest.click("pods");
  const hiddenOpener = hiddenOpenerTest.walk(hiddenOpenerTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  hiddenOpener.click();
  hiddenOpener.hidden = true;
  hiddenOpenerTest.handlers.keydown[0]({ key: "Escape", preventDefault() {} });
  assert.equal(hiddenOpenerTest.get("#resource-details-drawer").hidden, true, "Escape closes the details drawer");
  assert.notEqual(hiddenOpenerTest.activeElement, hiddenOpener, "a hidden opener never takes the focus back");
  assert.equal(hiddenOpenerTest.activeElement, hiddenOpenerTest.get("#page-title"), "a hidden opener falls back to the page heading");

  // A control hidden by its ancestor is not focusable either.
  const hiddenAncestorTest = await bootHarness({});
  await hiddenAncestorTest.click("pods");
  const ancestorOpener = hiddenAncestorTest.walk(hiddenAncestorTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  ancestorOpener.click();
  hiddenAncestorTest.get('[data-table="pods"]').hidden = true;
  hiddenAncestorTest.handlers.keydown[0]({ key: "Escape", preventDefault() {} });
  assert.notEqual(hiddenAncestorTest.activeElement, ancestorOpener, "a control of a hidden table never takes the focus back");
  assert.equal(hiddenAncestorTest.activeElement, hiddenAncestorTest.get("#page-title"), "a control of a hidden table falls back to the page heading");

  // A control that is not rendered and a disabled control may never take the
  // focus back either.
  const ineligibleTest = await bootHarness({});
  await ineligibleTest.click("pods");
  const ineligible = ineligibleTest.walk(ineligibleTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  ineligible.click();
  ineligible.getClientRects = () => [];
  ineligibleTest.handlers.keydown[0]({ key: "Escape", preventDefault() {} });
  assert.notEqual(ineligibleTest.activeElement, ineligible, "a control that is not rendered never takes the focus back");
  assert.equal(ineligibleTest.activeElement, ineligibleTest.get("#page-title"), "a control that is not rendered falls back to the page heading");
  const disabledTest = await bootHarness({});
  await disabledTest.click("pods");
  const disabled = disabledTest.walk(disabledTest.get('[data-table="pods"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Pod work-0")[0];
  disabled.click();
  disabled.disabled = true;
  disabledTest.handlers.keydown[0]({ key: "Escape", preventDefault() {} });
  assert.notEqual(disabledTest.activeElement, disabled, "a disabled control never takes the focus back");
  assert.equal(disabledTest.activeElement, disabledTest.get("#page-title"), "a disabled control falls back to the page heading");

  // The hidden Overview table keeps a copy of every resource. The visible
  // control of the same resource takes the focus back, never the hidden one.
  const duplicateKeyTest = await bootHarness({});
  await duplicateKeyTest.click("overview");
  duplicateKeyTest.get("#toggle-overview-table").listeners.click[0]();
  await duplicateKeyTest.click("storage");
  const storageKey = "PersistentVolumeClaim|team|data";
  const storageControl = duplicateKeyTest.walk(duplicateKeyTest.get('[data-table="storage"]'), node => node.tagName === "BUTTON" && node.dataset.resourceKey === storageKey && node.textContent === "Details")[0];
  const overviewControl = duplicateKeyTest.walk(duplicateKeyTest.get('[data-table="overview"]'), node => node.tagName === "BUTTON" && node.dataset.resourceKey === storageKey)[0];
  assert.ok(storageControl && overviewControl, "the same resource is rendered in the Overview table and in the Storage table");
  storageControl.click();
  storageControl.hidden = true;
  duplicateKeyTest.handlers.keydown[0]({ key: "Escape", preventDefault() {} });
  assert.notEqual(duplicateKeyTest.activeElement, storageControl, "the hidden control never takes the focus back");
  assert.notEqual(duplicateKeyTest.activeElement, overviewControl, "the hidden Overview control never takes the focus back");
  const visibleKeyControl = duplicateKeyTest.walk(duplicateKeyTest.get('[data-table="storage"]'), node => node.tagName === "BUTTON" && node.dataset.resourceKey === storageKey && node.textContent === "data")[0];
  assert.equal(duplicateKeyTest.activeElement, visibleKeyControl, "the visible control of the same resource takes the focus back");

  // The Profile summary hands over to the editor. The editor is closed again
  // and the focus never stays on a control of the closed summary.
  const profileFocusTest = await bootHarness({});
  await profileFocusTest.click("profiles");
  profileFocusTest.walk(profileFocusTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0].click();
  const summaryEditButton = profileFocusTest.walk(profileFocusTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0];
  summaryEditButton.focus();
  summaryEditButton.click();
  assert.equal(profileFocusTest.get("#profile-drawer").hidden, false, "the editor drawer opens");
  assert.equal(profileFocusTest.get("#profile-summary-drawer").hidden, true, "the summary drawer is closed");
  profileFocusTest.closers[0].listeners.click[0]();
  assert.equal(profileFocusTest.get("#profile-drawer").hidden, true, "the editor drawer closes");
  assert.notEqual(profileFocusTest.activeElement, summaryEditButton, "the focus never stays on a control of the closed summary");
  assert.notEqual(profileFocusTest.activeElement, profileFocusTest.get("#page-title"), "the visible management control takes the focus back");
  assert.equal(profileFocusTest.activeElement, profileFocusTest.openers[0], "the visible management control of the page takes the focus back");

  // ---- Profile management ---------------------------------------------------
  // The create form owns its name input. A dotted name is refused with a
  // suggestion that is only applied when the administrator asks for it.
  const profileTest = await bootHarness({});
  profileTest.openers[0].listeners.click[0]();
  assert.equal(profileTest.get("#profile-drawer").hidden, false, "the profile drawer opens");
  const createName = profileTest.get("#profile-create-name");
  createName.value = "my.team";
  for (const handler of createName.listeners.input || []) handler();
  assert.equal(profileTest.get("#profile-name-suggestion").hidden, false, "a dotted name offers a suggestion");
  assert.ok(profileTest.get("#profile-name-suggestion-text").textContent.includes("my-team"), "the exact replacement is named");
  assert.equal(createName.value, "my.team", "the typed name is never rewritten");
  assert.ok(profileTest.get("#profile-name-message").textContent.includes("dots or underscores"), "the reason is concise and accessible");
  assert.ok(String(createName.attributes["aria-describedby"]).includes("profile-name-suggestion-text"), "the suggestion is linked to the input");
  profileTest.get("#profile-use-suggestion").listeners.click[0]();
  assert.equal(createName.value, "my-team", "the suggestion is applied only on an explicit request");

  // An existing namespace reserves the name: the request never reaches the API.
  createName.value = "reserved-ns";
  for (const handler of createName.listeners.input || []) handler();
  profileTest.get("#profile-create-owner").value = "alice@example.test";
  const reservedBefore = profileTest.bodies.length;
  await profileTest.submitForm(0);
  assert.equal(profileTest.bodies.length, reservedBefore, "a reserved name never reaches the API");
  assert.equal(profileTest.error.hidden, false, "the reserved name is refused in the drawer");
  assert.ok(profileTest.error.textContent.includes("already exists"), "the refusal names the conflict");

  // A valid create carries the typed name, the typed owner and no precondition.
  createName.value = "new-team";
  await profileTest.submitForm(0);
  const createBody = profileTest.bodies[profileTest.bodies.length - 1];
  assert.equal(createBody.kind, "profile.create");
  assert.equal(createBody.name, "new-team");
  assert.equal(createBody.owner, "alice@example.test");
  assert.equal(createBody.baseConfigDigest, undefined, "a create carries no configuration precondition");

  // The Profile summary shows the observed configuration and the management
  // actions after the scoped views.
  const summaryTest = await bootHarness({});
  await summaryTest.click("profiles");
  const profileRow = summaryTest.walk(summaryTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  assert.ok(profileRow, "a Profile name opens its summary");
  profileRow.listeners.click[0]();
  const summaryText = summaryTest.textOf(summaryTest.get("#profile-summary-body"));
  assert.ok(summaryText.includes("Scoped views"), "the scoped views stay above the configuration");
  assert.ok(summaryText.includes("kubeflow.org/v1"), "the summary shows the observed API version");
  assert.ok(summaryText.includes("alice@example.test"), "the summary shows the observed owner");
  assert.ok(summaryText.includes("Active") && summaryText.includes("Cluster scoped"), "the summary shows the observed state and scope");
  assert.ok(summaryText.includes("2026-01-01T00:00:00Z") && summaryText.includes("Resource version") && summaryText.includes("7"), "the summary shows the safe metadata facts");
  assert.ok(summaryText.includes("requests.cpu") && summaryText.includes("4"), "the summary shows the observed quota values");
  assert.ok(summaryText.includes("example.com/gpu"), "the summary shows every projected quota key");
  assert.equal(summaryText.split("alice@example.test").length - 1, 1, "the owner is shown once, never duplicated");
  assert.equal(summaryText.split("requests.cpu").length - 1, 1, "each observed quota key is shown once, never duplicated");
  const copyButton = summaryTest.walk(summaryTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Copy")[0];
  assert.ok(copyButton, "the owner email can be copied");

  // Edit opens the management drawer in update mode, preselected and prefilled.
  const editButton = summaryTest.walk(summaryTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0];
  assert.ok(editButton, "the summary offers the edit action");
  editButton.listeners.click[0]();
  assert.equal(summaryTest.get("#profile-drawer").hidden, false, "the edit action opens the management drawer");
  assert.equal(summaryTest.get("#profile-kind").value, "profile.update", "the drawer opens in update mode");
  assert.equal(summaryTest.get("#profile-update-name").value, "team", "the selected Profile is preselected");
  assert.equal(summaryTest.get("#profile-update-owner").value, "alice@example.test", "the exact observed owner is prefilled");
  assert.equal(summaryTest.get("#profile-update-owner").readOnly, true, "the owner is read-only");
  assert.equal(summaryTest.get("#profile-update-owner").disabled, false, "a read-only owner is still submitted");
  assert.equal(summaryTest.get("#profile-update-digest").value, "sha256:teamdigest", "the configuration precondition is carried");
  assert.equal(summaryTest.get("#profile-create-name").disabled, true, "the create name is disabled in update mode");
  assert.equal(summaryTest.get("#profile-create-owner").disabled, true, "the create owner is disabled in update mode");
  const cpuInput = summaryTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.cpu");
  assert.equal(cpuInput.value, "4", "the observed quota is prefilled");
  assert.equal(cpuInput.required, true, "an existing managed key may not be omitted");
  // An observed device key this console has no static field for is edited
  // under its own name. It is never renamed to a canonical requests.* key.
  const deviceInput = summaryTest.walk(summaryTest.get("#profile-observed-quota-fields"), node => node.dataset && node.dataset.profileQuota === "example.com/gpu")[0];
  assert.ok(deviceInput, "the observed device key has an editable field");
  assert.equal(deviceInput.value, "1", "the observed device key is prefilled");
  assert.equal(deviceInput.required, true, "an observed key may not be omitted");
  assert.ok(summaryTest.textOf(summaryTest.get("#profile-observed-quota-fields")).includes("example.com/gpu"), "the observed device key is labelled with its own name");

  // The update payload carries the unchanged owner, the precondition and every
  // managed quota key.
  cpuInput.value = "8";
  await summaryTest.submitForm(0);
  const updateBody = summaryTest.bodies[summaryTest.bodies.length - 1];
  assert.equal(updateBody.kind, "profile.update");
  assert.equal(updateBody.name, "team");
  assert.equal(updateBody.owner, "alice@example.test", "the exact observed owner is submitted unchanged");
  assert.equal(updateBody.baseConfigDigest, "sha256:teamdigest", "the precondition travels with the request");
  assert.equal(updateBody.quota["requests.cpu"], "8", "the changed quota is submitted");
  assert.equal(updateBody.quota["requests.memory"], "8Gi", "an unchanged managed key is still submitted");
  assert.equal(updateBody.quota["example.com/gpu"], "1", "the observed device key round-trips unchanged");
  assert.equal(updateBody.quota["requests.nvidia.com/gpu"], undefined, "no canonical device key is added beside the observed one");
  assert.equal(updateBody.confirmName, undefined, "an update never carries a confirmation");
  assert.equal(updateBody.storageClass, undefined, "an update never carries an unrelated field");

  // A tampered read-only field is refused before the request: the server
  // revalidates the same values.
  summaryTest.get("#profile-update-owner").value = "someone-else@example.test";
  const tamperedBefore = summaryTest.bodies.length;
  await summaryTest.submitForm(0);
  assert.equal(summaryTest.bodies.length, tamperedBefore, "a tampered form never reaches the API");
  assert.ok(summaryTest.error.textContent.includes("no longer matches"), "the tampered binding is refused in the drawer");

  // An update that would change nothing is refused as well.
  summaryTest.get("#profile-update-owner").value = "alice@example.test";
  cpuInput.value = "4";
  const noopBefore = summaryTest.bodies.length;
  await summaryTest.submitForm(0);
  assert.equal(summaryTest.bodies.length, noopBefore, "a no-op update never reaches the API");
  assert.ok(summaryTest.error.textContent.includes("Nothing would change"), "a no-op update is refused in the drawer");

  // A managed key that is already set may not be omitted.
  cpuInput.value = "";
  const removalBefore = summaryTest.bodies.length;
  await summaryTest.submitForm(0);
  assert.equal(summaryTest.bodies.length, removalBefore, "a removed quota key never reaches the API");
  assert.ok(summaryTest.error.textContent.includes("removing a quota key is unsupported"), "the removal of an existing key is refused in the drawer");
  cpuInput.value = "4";

  // An existing Profile may declare quota keys this console has no static
  // field for: the cpu and memory aliases and a device key of its own. They
  // are edited under their own names. The declared key set never changes, no
  // canonical requests.* key is added beside them and a key this console does
  // not manage stays read-only.
  const legacyResources = [
    { kind: "Profile", name: "legacy", uid: "uid-profile-legacy", state: "Active", owner: "legacy.owner@example.test",
      details: { apiVersion: "kubeflow.org/v1", ownerKind: "User", configDigest: "sha256:legacydigest",
        "quota.cpu": "128", "quota.memory": "256Gi", "quota.nvidia.com/gpu": "4",
        "quota.persistentvolumeclaims": "20", "quota.requests.storage": "500Gi",
        // A key this console does not manage. It stays read-only and is
        // returned unchanged.
        "quota.requests.ephemeral-storage": "5Gi" } },
    { kind: "Namespace", name: "legacy", uid: "uid-namespace-legacy", state: "Active", details: { createdAt: "2026-01-04T00:00:00Z" } }
  ];
  const legacyQuotaTest = await bootHarness({ resources: legacyResources });
  await legacyQuotaTest.click("profiles");
  const legacyRow = legacyQuotaTest.walk(legacyQuotaTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "legacy")[0];
  assert.ok(legacyRow, "the legacy Profile opens its summary");
  legacyRow.listeners.click[0]();
  const legacySummaryText = legacyQuotaTest.textOf(legacyQuotaTest.get("#profile-summary-body"));
  assert.ok(legacySummaryText.includes("nvidia.com/gpu"), "the consolidated summary shows the observed bare device key");
  assert.ok(legacySummaryText.includes("requests.<device>") && legacySummaryText.includes("normally not enforced"), "an existing bare GPU key is visibly explained before any edit");
  assert.ok(legacySummaryText.includes("does not rename the key") && legacySummaryText.includes("does not activate enforcement"), "the warning never claims that editing migrates the key");
  const legacyEdit = legacyQuotaTest.walk(legacyQuotaTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0];
  assert.ok(legacyEdit, "the legacy Profile offers the edit action");
  legacyEdit.listeners.click[0]();
  assert.equal(legacyQuotaTest.get("#profile-kind").value, "profile.update", "the legacy Profile opens in update mode");
  const observedField = key => legacyQuotaTest.walk(legacyQuotaTest.get("#profile-observed-quota-fields"), node => node.dataset && node.dataset.profileQuota === key)[0];
  const legacyCpu = observedField("cpu");
  assert.ok(legacyCpu, "the observed cpu alias has an editable field");
  assert.equal(legacyCpu.value, "128", "the observed cpu alias is prefilled");
  assert.equal(legacyCpu.required, true, "an observed key may not be omitted");
  const legacyMemory = observedField("memory");
  assert.ok(legacyMemory, "the observed memory alias has an editable field");
  assert.equal(legacyMemory.value, "256Gi", "the observed memory alias is prefilled");
  const legacyGpu = observedField("nvidia.com/gpu");
  assert.ok(legacyGpu, "the observed device key has an editable field");
  assert.equal(legacyGpu.value, "4", "the observed device key is prefilled");
  assert.equal(legacyGpu.type, "number", "the device key counts whole devices");
  assert.equal(legacyGpu.min, "0", "the device key accepts a zero limit");
  assert.equal(legacyGpu.step, "1", "the device key stays an integer");
  const legacyClaims = legacyQuotaTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "persistentvolumeclaims");
  assert.equal(legacyClaims.value, "20", "the observed claim count is prefilled");
  const legacyStorage = legacyQuotaTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.storage");
  assert.equal(legacyStorage.value, "500Gi", "the observed storage request is prefilled");
  assert.ok(legacyQuotaTest.textOf(legacyQuotaTest.get("#profile-observed-quota-fields")).includes("nvidia.com/gpu"), "the observed device key is labelled with its own name");
  assert.ok(legacyQuotaTest.textOf(legacyQuotaTest.get("#profile-observed-quota-fields")).includes("normally not enforced"), "the bare device warning is visible before the plan is prepared");
  assert.ok(legacyQuotaTest.textOf(legacyQuotaTest.get("#profile-extra-quota-fields")).includes("requests.ephemeral-storage"), "an unmanaged observed key stays read-only");

  // A device count is a whole number of objects.
  legacyGpu.value = "0.5";
  const fractionBefore = legacyQuotaTest.bodies.length;
  await legacyQuotaTest.submitForm(0);
  assert.equal(legacyQuotaTest.bodies.length, fractionBefore, "a fractional device count never reaches the API");
  assert.ok(legacyQuotaTest.error.textContent.includes("whole number"), "a fractional device count is refused in the drawer");

  // Only the CPU alias changes: the payload carries exactly the observed key
  // set. No canonical requests.cpu, requests.memory or requests.nvidia.com/gpu
  // key is added beside the observed ones.
  legacyCpu.value = "96";
  legacyGpu.value = "4";
  await legacyQuotaTest.submitForm(0);
  const legacyQuotaBody = legacyQuotaTest.bodies[legacyQuotaTest.bodies.length - 1];
  assert.equal(legacyQuotaBody.kind, "profile.update");
  assert.equal(legacyQuotaBody.name, "legacy");
  assert.equal(legacyQuotaBody.quota["cpu"], "96", "the changed alias is submitted under its own name");
  assert.equal(legacyQuotaBody.quota["memory"], "256Gi", "the unchanged alias is submitted");
  assert.equal(legacyQuotaBody.quota["nvidia.com/gpu"], "4", "the device key is submitted under its own name");
  assert.equal(legacyQuotaBody.quota["persistentvolumeclaims"], "20", "the claim count is submitted");
  assert.equal(legacyQuotaBody.quota["requests.storage"], "500Gi", "the storage request is submitted");
  assert.equal(legacyQuotaBody.quota["requests.ephemeral-storage"], "5Gi", "the unmanaged key round-trips unchanged");
  assert.deepEqual(Object.keys(legacyQuotaBody.quota).sort(), ["cpu", "memory", "nvidia.com/gpu", "persistentvolumeclaims", "requests.ephemeral-storage", "requests.storage"],
    "the update carries exactly the observed key set");
  for (const key of ["requests.cpu", "requests.memory", "requests.nvidia.com/gpu", "limits.cpu", "limits.memory", "pods"]) {
    assert.equal(legacyQuotaBody.quota[key], undefined, "the undeclared key " + key + " is never added");
  }

  // Only the device key changes: the same key set travels with the request.
  legacyCpu.value = "128";
  legacyGpu.value = "2";
  await legacyQuotaTest.submitForm(0);
  const legacyGpuBody = legacyQuotaTest.bodies[legacyQuotaTest.bodies.length - 1];
  assert.equal(legacyGpuBody.quota["cpu"], "128", "the unchanged alias travels with the request");
  assert.equal(legacyGpuBody.quota["nvidia.com/gpu"], "2", "the changed device key travels under its own name");
  assert.deepEqual(Object.keys(legacyGpuBody.quota).sort(), ["cpu", "memory", "nvidia.com/gpu", "persistentvolumeclaims", "requests.ephemeral-storage", "requests.storage"],
    "the device edit never changes the key set");

  // A zero device limit is a real value and is submitted.
  legacyGpu.value = "0";
  await legacyQuotaTest.submitForm(0);
  const legacyZeroBody = legacyQuotaTest.bodies[legacyQuotaTest.bodies.length - 1];
  assert.equal(legacyZeroBody.quota["nvidia.com/gpu"], "0", "a zero device limit is submitted");

  // Native whole-number spellings are accepted exactly as typed. The raw text
  // is submitted unchanged; the server owns the authoritative whole-quantity
  // validation.
  const countNotationTest = await bootHarness({ resources: [
    { kind: "Profile", name: "counts", uid: "uid-profile-counts", state: "Active", owner: "counts@example.test",
      details: { apiVersion: "kubeflow.org/v1", configDigest: "sha256:countsdigest", "quota.pods": "10", "quota.nvidia.com/gpu": "1" } }
  ] });
  await countNotationTest.click("profiles");
  countNotationTest.walk(countNotationTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "counts")[0].click();
  countNotationTest.walk(countNotationTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0].click();
  const countPods = countNotationTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "pods");
  const countGpu = countNotationTest.walk(countNotationTest.get("#profile-observed-quota-fields"), node => node.dataset && node.dataset.profileQuota === "nvidia.com/gpu")[0];
  assert.ok(countPods && countGpu, "the pods count and the raw GPU key are editable");
  countPods.value = "1e2";
  countGpu.value = "1e0";
  await countNotationTest.submitForm(0);
  const countNotationBody = countNotationTest.bodies[countNotationTest.bodies.length - 1];
  assert.equal(countNotationBody.kind, "profile.update", "a whole-number notation prepares a plan");
  assert.equal(countNotationBody.quota["pods"], "1e2", "the typed pods count is submitted unchanged");
  assert.equal(countNotationBody.quota["nvidia.com/gpu"], "1e0", "the typed raw device count is submitted unchanged");
  assert.deepEqual(Object.keys(countNotationBody.quota).sort(), ["nvidia.com/gpu", "pods"], "the typed notation never renames or adds a key");

  // An observed key may not be omitted.
  legacyGpu.value = "";
  const legacyRemovalBefore = legacyQuotaTest.bodies.length;
  await legacyQuotaTest.submitForm(0);
  assert.equal(legacyQuotaTest.bodies.length, legacyRemovalBefore, "a removed observed key never reaches the API");
  assert.ok(legacyQuotaTest.error.textContent.includes("removing a quota key is unsupported"), "the removal of an observed key is refused in the drawer");

  // The deletion drawer is gated by the server opt-in and the protection of
  // the observed Profile.
  const deleteTest = await bootHarness({});
  await deleteTest.click("profiles");
  const deleteRow = deleteTest.walk(deleteTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  deleteRow.listeners.click[0]();
  const deleteButton = deleteTest.walk(deleteTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Delete profile")[0];
  assert.ok(deleteButton, "the deletion action is offered for an unprotected profile");
  deleteButton.listeners.click[0]();
  assert.equal(deleteTest.get("#profile-delete-drawer").hidden, false, "the deletion drawer opens");
  assert.equal(deleteTest.get("#profile-delete-name").value, "team", "the target is bound to the selected Profile");
  assert.ok(deleteTest.get("#profile-delete-target").textContent.includes("also deletes the namespace team"), "the drawer names the namespace that goes with the Profile");
  assert.ok(html.includes("which can be irreversible") && html.includes("Retain reclaim policy is not protection"), "the deletion drawer warns about the data");
  deleteTest.get("#profile-delete-reason").value = "decommission the pilot workspace";
  deleteTest.get("#profile-delete-confirm").value = " team ";
  const confirmBefore = deleteTest.bodies.length;
  await deleteTest.submitForm(4);
  assert.equal(deleteTest.bodies.length, confirmBefore, "a mismatched confirmation never reaches the API");
  assert.ok(deleteTest.error.textContent.includes("does not match"), "the exact confirmation is refused in the drawer");
  deleteTest.get("#profile-delete-confirm").value = "team";
  await deleteTest.submitForm(4);
  const deleteBody = deleteTest.bodies[deleteTest.bodies.length - 1];
  assert.equal(deleteBody.kind, "profile.delete");
  assert.equal(deleteBody.name, "team");
  assert.equal(deleteBody.reason, "decommission the pilot workspace");
  assert.equal(deleteBody.confirmName, "team");
  assert.equal(deleteBody.owner, undefined, "a deletion never carries an owner");
  assert.equal(deleteBody.baseConfigDigest, undefined, "a deletion never carries a precondition");

  const guardedTest = await bootHarness({});
  await guardedTest.click("profiles");
  const guardedRow = guardedTest.walk(guardedTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "guarded")[0];
  guardedRow.listeners.click[0]();
  assert.equal(guardedTest.walk(guardedTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Delete profile").length, 0, "a protected Profile is never deletable");
  const noDeletionTest = await bootHarness({ profileDeletion: false });
  await noDeletionTest.click("profiles");
  const noDeletionRow = noDeletionTest.walk(noDeletionTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  noDeletionRow.listeners.click[0]();
  assert.equal(noDeletionTest.walk(noDeletionTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Delete profile").length, 0, "deletion is not offered without the server opt-in");

  // An inventory refresh never edits the administrator's form values, and a
  // new selection never inherits the previous one.
  const refreshTest = await bootHarness({});
  refreshTest.openers[0].listeners.click[0]();
  refreshTest.get("#profile-create-name").value = "typed-name";
  refreshTest.get("#profile-create-owner").value = "typed@example.test";
  refreshTest.get("#refresh").listeners.click[0]();
  await refreshTest.settle();
  assert.equal(refreshTest.get("#profile-create-name").value, "typed-name", "an inventory refresh never edits the typed name");
  assert.equal(refreshTest.get("#profile-create-owner").value, "typed@example.test", "an inventory refresh never edits the typed owner");
  refreshTest.get("#profile-kind").value = "profile.update";
  for (const handler of refreshTest.get("#profile-kind").listeners.change || []) handler();
  const updateSelect = refreshTest.get("#profile-update-name");
  updateSelect.value = "team";
  for (const handler of updateSelect.listeners.change || []) handler();
  assert.equal(refreshTest.get("#profile-update-owner").value, "alice@example.test", "the typed create owner never bleeds into the update");
  const refreshCpu = refreshTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.cpu");
  assert.equal(refreshCpu.value, "4", "the observed quota is loaded");
  updateSelect.value = "guarded";
  for (const handler of updateSelect.listeners.change || []) handler();
  assert.equal(refreshTest.get("#profile-update-owner").value, "bob@example.test", "a new selection replaces the previous owner");
  assert.equal(refreshTest.get("#profile-update-digest").value, "sha256:guardeddigest", "a new selection replaces the previous precondition");
  assert.equal(refreshCpu.value, "", "a new selection clears the previous quota");

  // A Profile that changed under the open form is refused before the request:
  // the typed values and the precondition are kept, and the administrator is
  // asked to reselect instead of retrying a request that can only be refused.
  const staleProfileTest = await bootHarness({});
  await staleProfileTest.click("profiles");
  staleProfileTest.walk(staleProfileTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0].click();
  staleProfileTest.walk(staleProfileTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0].click();
  const staleCpu = staleProfileTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.cpu");
  staleCpu.value = "9";
  const staleDigest = staleProfileTest.get("#profile-update-digest").value;
  assert.equal(staleDigest, "sha256:teamdigest", "the precondition is loaded with the selection");
  // The observation changes under the open form.
  staleProfileTest.setResources(staleProfileTest.resources().map(resource =>
    resource.kind === "Profile" && resource.name === "team"
      ? { ...resource, details: { ...resource.details, configDigest: "sha256:teamdigest-new" } } : resource));
  staleProfileTest.get("#refresh").listeners.click[0]();
  await staleProfileTest.settle();
  assert.equal(staleProfileTest.get("#profile-update-digest").value, staleDigest, "a refresh never rewrites the precondition of the open form");
  assert.equal(staleCpu.value, "9", "a refresh never rewrites the typed quota");
  const staleBefore = staleProfileTest.bodies.length;
  await staleProfileTest.submitForm(0);
  assert.equal(staleProfileTest.bodies.length, staleBefore, "a form that no longer matches the observation never reaches the API");
  assert.ok(staleProfileTest.error.textContent.includes("Reselect or reopen the Profile"), "the administrator is asked to reselect");
  assert.ok(staleProfileTest.error.textContent.includes("your edits were not applied"), "the refusal states that the edits were not applied");
  // A reselection loads the current observation and the request is prepared.
  const staleSelect = staleProfileTest.get("#profile-update-name");
  staleSelect.value = "team";
  for (const handler of staleSelect.listeners.change || []) handler();
  assert.equal(staleProfileTest.get("#profile-update-digest").value, "sha256:teamdigest-new", "the reselection loads the current precondition");
  staleCpu.value = "9";
  await staleProfileTest.submitForm(0);
  const staleBody = staleProfileTest.bodies[staleProfileTest.bodies.length - 1];
  assert.equal(staleBody.kind, "profile.update", "a reselected form prepares the request");
  assert.equal(staleBody.baseConfigDigest, "sha256:teamdigest-new", "the current precondition travels with the request");

  // A Profile that was replaced under the open form is refused as well, even
  // when its configuration precondition is unchanged.
  const replacedProfileTest = await bootHarness({});
  await replacedProfileTest.click("profiles");
  replacedProfileTest.walk(replacedProfileTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0].click();
  replacedProfileTest.walk(replacedProfileTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0].click();
  replacedProfileTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.cpu").value = "9";
  replacedProfileTest.setResources(replacedProfileTest.resources().map(resource =>
    resource.kind === "Profile" && resource.name === "team" ? { ...resource, uid: "uid-profile-team-replaced" } : resource));
  replacedProfileTest.get("#refresh").listeners.click[0]();
  await replacedProfileTest.settle();
  const replacedBefore = replacedProfileTest.bodies.length;
  await replacedProfileTest.submitForm(0);
  assert.equal(replacedProfileTest.bodies.length, replacedBefore, "a replaced Profile never reaches the API");
  assert.ok(replacedProfileTest.error.textContent.includes("Reselect or reopen the Profile"), "a replaced Profile asks for a reselection");

  // An ordinary refresh of an unchanged Profile never refuses the request.
  const sameProfileTest = await bootHarness({});
  await sameProfileTest.click("profiles");
  sameProfileTest.walk(sameProfileTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0].click();
  sameProfileTest.walk(sameProfileTest.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0].click();
  sameProfileTest.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.cpu").value = "9";
  sameProfileTest.get("#refresh").listeners.click[0]();
  await sameProfileTest.settle();
  await sameProfileTest.submitForm(0);
  assert.equal(sameProfileTest.bodies.length > 0, true, "a refresh with an unchanged Profile never blocks the request");
  assert.equal(sameProfileTest.bodies[sameProfileTest.bodies.length - 1].baseConfigDigest, "sha256:teamdigest", "the unchanged precondition travels with the request");

  // The plan preview shows the validated before and after values of the raw
  // server change.
  const previewTest = await bootHarness({});
  await previewTest.submitForm(2);
  const changesNode = previewTest.walk(previewTest.get("#plan-preview"), node => node.id === "plan-changes")[0];
  assert.ok(changesNode, "the validated change table is rendered");
  const changesText = previewTest.textOf(changesNode);
  assert.ok(changesText.includes("requests.cpu") && changesText.includes("4") && changesText.includes("8"), "the server change is rendered with its actual before and after values");
  assert.ok(changesText.includes("Before") && changesText.includes("After"), "the change table keeps its before and after columns");

  // The configuration precondition never reaches the console.
  const digestTest = await bootHarness({});
  await digestTest.click("profiles");
  const digestRow = digestTest.walk(digestTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  digestRow.listeners.click[0]();
  assert.equal(digestTest.textOf(digestTest.get("#profile-summary-body")).includes("sha256:teamdigest"), false, "the Profile summary never shows the precondition hash");
  assert.equal(digestTest.walk(digestTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "Details").length, 0, "a Profile row exposes no generic Details control");
  assert.equal(digestTest.walk(digestTest.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.attributes["aria-label"] === "Details for Profile team").length, 0, "a Profile name opens the consolidated summary, not generic resource details");
  const digestText = digestTest.textOf(digestTest.get("#profile-summary-body"));
  assert.equal(digestText.includes("sha256:teamdigest"), false, "the summary never shows the precondition hash");
  assert.equal(digestText.includes("configDigest"), false, "the precondition key is not rendered at all");
  assert.ok(digestText.includes("kubeflow.org/v1") && digestText.includes("alice@example.test"), "the consolidated summary keeps the safe Profile facts");
  digestTest.closers[0].listeners.click[0];
  // The precondition hash is not searchable either.
  const hashSearch = digestTest.get('[data-table="profiles"]').querySelector("[data-search]");
  hashSearch.value = "sha256:teamdigest";
  for (const handler of hashSearch.listeners.input || []) handler();
  assert.equal(digestTest.walk(digestTest.get('[data-table="profiles"]'), node => node.tagName === "TR").length, 0, "the precondition hash matches no loaded row");
  assert.ok(digestTest.textOf(digestTest.get('[data-table="profiles"]')).includes("0 records matching"), "the precondition hash is not part of the searchable observation");
  hashSearch.value = "";
  for (const handler of hashSearch.listeners.input || []) handler();
  // The update request still carries the precondition.
  const digestUpdate = await bootHarness({});
  await digestUpdate.click("profiles");
  const digestUpdateRow = digestUpdate.walk(digestUpdate.get('[data-table="profiles"]'), node => node.tagName === "BUTTON" && node.textContent === "team")[0];
  digestUpdateRow.listeners.click[0]();
  const digestEdit = digestUpdate.walk(digestUpdate.get("#profile-summary-body"), node => node.tagName === "BUTTON" && node.textContent === "Edit profile")[0];
  digestEdit.listeners.click[0]();
  digestUpdate.profileQuotaInputs.find(input => input.dataset.profileQuota === "requests.cpu").value = "6";
  await digestUpdate.submitForm(0);
  const digestBody = digestUpdate.bodies[digestUpdate.bodies.length - 1];
  assert.equal(digestBody.baseConfigDigest, "sha256:teamdigest", "the update request still carries the precondition");

  console.log("Dashboard navigation, bounded viewers, drawer contracts, notification policy and history tests passed.");
})().catch(error => { console.error(error); process.exitCode = 1; });
