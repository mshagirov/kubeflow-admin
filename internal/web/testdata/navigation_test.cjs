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
assert.equal(html.includes("Administrative control plane"), false, "sidebar omits the administrative control plane label");
assert.equal(html.includes('<span class="dot"></span>'), false, "sidebar omits the status dot");
assert.equal(/data-page="(operations|audit)"/.test(html), false, "History replaces separate sidebar entries");
assert.equal(html.includes('<section id="operations"'), false);
assert.equal(html.includes('<section id="audit"'), false);
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
for (const drawer of ["profile-drawer", "storage-drawer", "notebook-drawer", "profile-summary-drawer", "pod-restart-drawer", "namespace-summary-drawer", "node-cordon-drawer"]) {
  assert.ok(html.includes(`id="${drawer}"`), `missing ${drawer}`);
}
assert.equal((html.match(/aria-modal="true"/g) || []).length >= 7, true);
for (const kind of ["profile", "storage", "notebook"]) assert.ok(html.includes(`data-open-drawer="${kind}"`));
assert.ok(html.includes('id="profile-form"') && html.includes('id="storage-form"') && html.includes('id="notebook-form"') && html.includes('id="pod-restart-form"') && html.includes('id="node-cordon-form"'));
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
    replaceChildren(...nodes) { this.children.forEach(node => { if (node) node.parent = null; }); nodes.forEach(node => { if (node) node.parent = this; }); this.children = nodes; }
    prepend(...nodes) { nodes.forEach(node => { if (node) node.parent = this; }); this.children.unshift(...nodes); }
    addEventListener(name, handler) { (this.listeners[name] || (this.listeners[name] = [])).push(handler); }
    querySelector(selector) {
      if (selector === "dialog[open]") return null;
      return descendants(this, selector)[0] || null;
    }
    querySelectorAll(selector) { return descendants(this, selector); }
    closest(selector) { return selector === "[hidden]" ? (this.hidden ? this : null) : this; }
    get parentElement() { return new Element(); }
    before() {}
    remove() {
      if (this.parent) { this.parent.children = this.parent.children.filter(child => child !== this); this.parent = null; }
    }
    focus() { document.activeElement = this; }
    click() { for (const handler of this.listeners.click || []) handler({ button: 0, defaultPrevented: false, preventDefault() {}, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false }); }
    select() {}
    showModal() { this.open = true; }
    close() { this.open = false; }
    getClientRects() { return this.hidden ? [] : [{}]; }
    contains(node) { return node === this || this.children.some(child => child && typeof child.contains === "function" && child.contains(node)); }
    setAttribute(name, value) { this.attributes[name] = value; }
    removeAttribute(name) { delete this.attributes[name]; }
    get textContent() { return this._text || ""; }
    set textContent(value) { this._text = String(value); }
  }
  class Option extends Element { constructor(text, value) { super("option"); this.textContent = text; this.value = value; } }
  class FakeFormData {
    constructor(form) { this.form = form; }
    *[Symbol.iterator]() {
      yield ["kind", "notebook.stop"];
      yield ["namespace", "team-a"];
      yield ["name", "work-000"];
      yield ["reason", "Synthetic retry reason"];
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
  const sidebar = [["overview", "/"], ["profiles", "/profiles"], ["namespaces", "/namespaces"], ["storage", "/storage"], ["notebooks", "/notebooks"], ["pods", "/pods"], ["nodes", "/nodes"], ["history", "/history"]].map(([page, href]) => {
    const link = new Element("a"); link.dataset.page = page; link.href = "https://admin.example.invalid" + href; return link;
  });
  const sections = ["overview", "profiles", "namespaces", "storage", "notebooks", "pods", "nodes", "history"].map(id => { const section = new Element("section"); section.id = id; return section; });
  const tabs = ["operations", "audit"].map(tab => { const button = new Element("button"); button.dataset.historyTab = tab; return button; });
  const drawers = ["profile", "storage", "notebook", "profile-summary", "pod-restart", "namespace-summary", "node-cordon"].map(kind => { const drawer = get("#" + kind + "-drawer"); drawer.hidden = true; return drawer; });
  const sharedError = new Element("p"); sharedError.className = "form-error"; sharedError.hidden = true;
  const sharedSubmit = new Element("button"); sharedSubmit.type = "submit"; sharedSubmit.textContent = "Prepare plan";
  const forms = ["profile-form", "storage-form", "notebook-form", "pod-restart-form"].map(id => {
    const form = new Element("form"); form.id = id; form.append(sharedError, sharedSubmit); return form;
  });
  const nodeCordonForm = get("#node-cordon-form");
  nodeCordonForm.elements = { kind: new Element("input"), name: new Element("input"), reason: new Element("input"), force: new Element("input") };
  const openers = ["profile", "storage", "notebook"].map(kind => { const button = new Element("button"); button.dataset.openDrawer = kind; return button; });
  const closers = [new Element("button")];
  const labels = ["pvc.create pvc.expand pv.nfs.create pv.reserve", "pvc.create pv.nfs.create pv.reserve", "pv.reserve", "pv.nfs.create", "pv.nfs.create"].map(value => { const label = new Element("label"); label.dataset.storage = value; return label; });
  const handlers = {}, intervals = [], writes = [], calls = [];
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
      return [];
    },
    createElement(tag) { return new Element(tag); },
    createTextNode(text) { const node = new Element("#text"); node._text = text; return node; },
    addEventListener(name, handler) { (handlers[name] || (handlers[name] = [])).push(handler); }
  };
  const notebookWrites = options.notebookWrites !== false, podRestarts = options.podRestarts !== false, nodeCordon = options.nodeCordon !== false;
  for (const name of ["csrf-token", "notebook-writes", "static-storage", "pvc-expansion", "pod-restarts", "node-cordon", "notification-scope"]) {
    get(`meta[name="${name}"]`).content = name === "csrf-token" ? "test-csrf"
      : name === "notebook-writes" ? String(notebookWrites)
      : name === "pod-restarts" ? String(podRestarts)
      : name === "node-cordon" ? String(nodeCordon)
      : name === "notification-scope" ? "random-test-scope" : "false";
  }
  get("#storage-kind").value = "pvc.create"; get("#notebook-kind").value = "notebook.start"; get("#profile-kind").value = "profile.create";
  get("#overview-table").hidden = true;
  get("#storage-template").selectedOptions = []; get("#pv-selector").selectedOptions = []; get("#replace-mounts").checked = false;
  const mountHint = new Element("p"); mountHint.className = "hint"; get("#replace-mounts").append(mountHint);
  const location = new URL("https://admin.example.invalid/");
  const window = {
    history: {
      pushState(_state, _title, url) { const next = new URL(url, "https://admin.example.invalid"); location.pathname = next.pathname; location.search = next.search; },
      replaceState(_state, _title, url) { const next = new URL(url, "https://admin.example.invalid"); location.pathname = next.pathname; location.search = next.search; }
    },
    addEventListener() {}
  };
  const inventory = {
    // Deliberately skewed server time proves the browser uses local receipt age.
    observedAt: options.observedAt || "2000-01-01T00:00:00Z",
    resources: [
      { kind: "Profile", name: "team" },
      { kind: "Namespace", name: "team", state: "Active", details: { createdAt: "2026-01-01T00:00:00Z" } },
      { kind: "Namespace", name: "other", state: "Active", details: { createdAt: "2026-01-02T00:00:00Z" } },
      { kind: "Notebook", name: "work", namespace: "team", state: "Running / Ready", details: { "image.main": "image" } },
      { kind: "Pod", name: "work-0", namespace: "team", state: "Running", owner: "StatefulSet/work", details: { node: "node-1", ready: "true", containers: "1/1 ready", restarts: "2", "requested.cpu": "500m", "requested.memory": "1Gi", "limit.cpu": "2", "limit.memory": "4Gi" } },
      { kind: "Pod", name: "legacy-0", namespace: "team", state: "Running", owner: "Deployment/legacy", details: { node: "node-1", ready: "true", containers: "1/1 ready", restarts: "0" } },
      { kind: "PersistentVolumeClaim", name: "data", namespace: "team", state: "Bound", details: { requested: "10Gi", capacity: "10Gi", storageClass: "team-class" } },
      { kind: "StorageClass", name: "team-class" },
      { kind: "Event", name: "scheduled", namespace: "team", state: "Warning" },
      { kind: "Node", name: "node-1", state: "Ready,SchedulingDisabled", details: { roles: "control-plane", createdAt: "2026-01-01T00:00:00Z", version: "v1.31.1", internalIP: "10.0.0.1", externalIP: "", osImage: "Flatcar", kernelVersion: "6.6.0", containerRuntime: "containerd://2.0", unschedulable: "true", controlPlane: "true" } },
      { kind: "Node", name: "node-2", state: "Ready", details: { roles: "control-plane", createdAt: "2026-01-01T00:00:00Z", version: "v1.31.1", internalIP: "10.0.0.2", osImage: "Flatcar", kernelVersion: "6.6.0", containerRuntime: "containerd://2.0", unschedulable: "false", controlPlane: "true" } },
      { kind: "Node", name: "node-3", state: "NotReady", details: { roles: "worker", createdAt: "2026-01-01T00:00:00Z", version: "v1.31.1", internalIP: "10.0.0.3", osImage: "Flatcar", kernelVersion: "6.6.0", containerRuntime: "containerd://2.0", unschedulable: "false", controlPlane: "false" } }
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
    if (url.endsWith("/inventory")) {
      if (options.deferInventory) await new Promise(resolve => { resolveInventory = resolve; });
      return response(inventory);
    }
    if (url.endsWith("/plans")) {
      if (plansFailures > 0) {
        plansFailures--;
        return response({ error: { code: "validation_failed", message: "Synthetic plan rejected for test.", retryable: false } }, false, 422);
      }
      return response({ id: "synthetic-plan", actor: "https://issuer.example|subject", actorDisplay: "Test Admin", createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 300000).toISOString(), prepared: { action: { kind: "notebook.stop" }, summary: "Synthetic preview", warnings: ["Synthetic interruption warning"], targetUID: "uid", resourceVersion: "1" } }, true, 201);
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
    get, calls, writes, drawers, openers, closers, tabs, forms, error: sharedError, location, handlers, textOf, walk,
    get activeElement() { return document.activeElement; },
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

  console.log("Dashboard navigation, bounded viewers, drawer contracts, notification policy and history tests passed.");
})().catch(error => { console.error(error); process.exitCode = 1; });
