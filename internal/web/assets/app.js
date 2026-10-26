"use strict";
(() => {
  const $ = (s, root = document) => root.querySelector(s);
  const $$ = (s, root = document) => [...root.querySelectorAll(s)];
  const el = (tag, text, cls) => { const n = document.createElement(tag); if (text !== undefined) n.textContent = text; if (cls) n.className = cls; return n; };
  const meta = name => $(`meta[name="${name}"]`)?.content || "";
  const csrf = meta("csrf-token");
  const writes = document.body.dataset.writes === "true";
  const notebookWrites = meta("notebook-writes") === "true";
  const podRestartFeature = meta("pod-restarts") === "true" && writes && notebookWrites;
  const nodeCordonFeature = meta("node-cordon") === "true" && writes;
  // Profile deletion is a separate, reviewed opt-in. It is only offered when
  // the server projects the capability and writes are enabled.
  const profileDeletionFeature = meta("profile-deletion") === "true" && writes;
  // Storage document editing and deletion are separate reviewed opt-ins. The
  // server projects them only with writes enabled; the browser repeats the
  // conjunction defensively and still shows the disabled action with a reason.
  const storageEditingFeature = meta("storage-editing") === "true" && writes;
  const storageDeletionFeature = meta("storage-deletion") === "true" && writes;
  const notificationScope = meta("notification-scope");
  const logic = globalThis.DashboardLogic;
  // The PodDefault capability is an independent opt-in and is only offered
  // when writes are enabled. The registry below carries only the approved,
  // non-secret registration-template projection.
  const podDefaultsFeature = meta("pod-defaults") === "true" && writes;
  // The approved registration templates come from the single server-owned
  // inventory.storageTemplates projection. No second bootstrap copy exists.
  let storageTemplates = [];
  // The approved class list is captured once, after the deferred script sees
  // the parsed template, before any action narrows it. Switching between a
  // registration action and a legacy action restores the full list.
  let allowedStorageClasses = null;

  const pageTitles = {
    overview: ["Overview", ""],
    profiles: ["Profiles", "Manage ownership, namespaces and resource quotas."],
    namespaces: ["Namespaces", "Observed namespaces and the resources they contain."],
    storage: ["Storage", "Provision and grow persistent storage with explicit safeguards."],
    notebooks: ["Notebooks", "Kubeflow Notebook custom resources only, with observed-state verification."],
    pods: ["Pods", "Observed workloads, ownership and requested resources."],
    nodes: ["Nodes", "Cluster node health, capacity context and cordon controls."],
    history: ["History", "Operations and audit records in one accountable view."]
  };

  const route = logic.routeFor(location.pathname, location.search);
  let page = route.page;
  let historyTab = route.tab || "operations";
  let namespaceContext = logic.queryNamespace(location.search);
  // Typed Pod filters are URL state, not a hidden table setting. They are
  // re-read on every navigation so a sidebar page change cannot inherit a
  // node filter from an earlier scoped view.
  let podFilters = logic.queryPodFilters(location.search);
  let inventory = null, inventoryAt = 0, inventoryError = false, loading = false;
  let currentPlan = null, submitKey = null, submitInflight = false;
  let drawerOpener = null;
  // The observed resource the open details drawer belongs to. It follows the
  // resource across an inventory refresh and identifies the control that may
  // take the focus back.
  let drawerResource = null;
  // The exact Notebook the management drawer was opened for. It binds the
  // namespace, name and observed UID so a refresh or a different selection can
  // never quietly submit an edit to another Notebook.
  let notebookTarget = null;
  let notebookManagementFromDetails = false;
  let notebookReturnKey = "";
  // The exact storage resource a YAML edit or deletion drawer belongs to. The
  // epoch invalidates every in-flight document read when the drawer closes or
  // another target opens. A document is only used when its kind, namespace,
  // name and UID still match the selected observed resource.
  let storageOperation = null;
  let storageEpoch = 0;
  let overviewTableExpanded = false;
  let errorTipSeq = 0;
  let rowCache = {};
  const historyCache = { operations: { rows: null, at: 0 }, audit: { rows: null, at: 0 } };
  const historyError = { operations: false, audit: false };
  const historyInflight = { operations: null, audit: null };
  const historyForce = { operations: false, audit: false };
  const historyEpoch = { operations: 0, audit: 0 };
  // Supervisor-owned retention header consumption is preserved.
  const historyRetention = { operations: null, audit: null };
  const historyLimit = { operations: null, audit: null };
  const historyWindow = { operations: null, audit: null };
  const tableStates = {
    overview: logic.defaultTableState("name"),
    profiles: logic.defaultTableState("name"),
    namespaces: logic.defaultTableState("name"),
    storage: logic.defaultTableState("name"),
    notebooks: logic.defaultTableState("name"),
    pods: logic.defaultTableState("name"),
    nodes: logic.defaultTableState("name"),
    historyOperations: logic.defaultTableState("time", "desc"),
    historyAudit: logic.defaultTableState("time", "desc")
  };
  // The table that owns each page. A deep-linked resource focus is applied to
  // the table of the page it was requested for.
  const pageTableKeys = {
    overview: "overview", profiles: "profiles", namespaces: "namespaces", storage: "storage",
    notebooks: "notebooks", pods: "pods", nodes: "nodes"
  };
  // A resource focus is the identity of one observed resource: its Kubernetes
  // kind, its namespace and its name. A name alone is never an identity: a
  // volume and its claim, a Notebook and its StatefulSet, or two Pods with the
  // same name in different namespaces can share a name.
  // The focus is mirrored in the visible search box, and the mirror is
  // remembered so an unrelated typed search is never overwritten.
  for (const key of Object.keys(tableStates)) { tableStates[key].focus = null; tableStates[key].focusMirror = false; }
  // A cluster-scoped kind is never identified by a namespace.
  const clusterScopedKinds = new Set(["Profile", "Namespace", "Node", "PersistentVolume", "StorageClass"]);
  // The only kinds with the narrow storage document workflow. StorageClasses
  // keep their observed details and never grow Edit/Delete actions.
  const storageActionKinds = new Set(["PersistentVolume", "PersistentVolumeClaim"]);
  // The kind a legacy deep link implies on a page that lists exactly one.
  // A page that lists several kinds treats a kind-less link as a name filter.
  const pageResourceKinds = { pods: "Pod", nodes: "Node", profiles: "Profile", namespaces: "Namespace" };
  const perPage = 50;
  // Local receipt time is the freshness clock. Server observedAt is display-only
  // so clock skew never disables Refresh or forces a scan on every navigation.
  const monotonicNow = () => {
    try { return typeof performance !== "undefined" && typeof performance.now === "function" ? performance.now() : Date.now(); }
    catch { return Date.now(); }
  };

  // ---- Notifications -------------------------------------------------------
  // Dismissal identifiers only. Message text, identities, tokens and inventory
  // are never persisted. The scope is a random non-secret server session value.
  const dismissed = loadDismissed();
  const visibleNotifications = new Map();
  const notificationTimers = new Map();
  let notificationSequence = 0;
  let notificationsExpanded = false;
  let notificationToggle = null;
  const noticeLabels = { info: "Information", success: "Success", warning: "Warning", error: "Error" };

  function renderNotificationStack() {
    const root = $("#notifications");
    const notices = [...visibleNotifications.values()].sort((a, b) =>
      Number(b.dataset.level === "error") - Number(a.dataset.level === "error") ||
      Number(b.dataset.recency) - Number(a.dataset.recency));
    if (!notificationToggle) {
      notificationToggle = el("button", undefined, "notification-toggle");
      notificationToggle.type = "button";
      notificationToggle.addEventListener("click", () => {
        notificationsExpanded = !notificationsExpanded;
        renderNotificationStack();
      });
    }
    if (notices.length <= 1) notificationsExpanded = false;
    const focused = document.activeElement;
    const restoreFocus = root.contains(focused);
    notificationToggle.hidden = notices.length <= 1;
    notificationToggle.textContent = notificationsExpanded
      ? `Collapse notifications (${notices.length})`
      : `Show ${notices.length} notifications (${notices.filter(node => node.dataset.level === "error").length} errors)`;
    notificationToggle.setAttribute("aria-expanded", String(notificationsExpanded));
    root.classList.toggle("is-expanded", notificationsExpanded);
    notices.forEach((node, index) => { node.hidden = !notificationsExpanded && index > 0; });
    root.replaceChildren(notificationToggle, ...notices);
    if (restoreFocus) {
      const target = !focused.hidden && !focused.closest("[hidden]") ? focused
        : notices[0]?.querySelector("button") || $("#refresh");
      target?.focus();
    }
  }

  function loadDismissed() {
    const remembered = new Set();
    if (!logic.shouldRememberDismissals(notificationScope)) return remembered;
    try {
      const raw = sessionStorage.getItem(logic.dismissalStorageKey(notificationScope));
      const parsed = JSON.parse(raw || "[]");
      if (Array.isArray(parsed)) parsed.filter(value => typeof value === "string" && value.length < 80).slice(-200).forEach(value => remembered.add(value));
    } catch { /* Private mode or disabled storage: explicit dismissal is still honoured in memory. */ }
    return remembered;
  }
  function persistDismissed() {
    if (!logic.shouldRememberDismissals(notificationScope)) return;
    try { sessionStorage.setItem(logic.dismissalStorageKey(notificationScope), JSON.stringify([...dismissed].slice(-200))); } catch { /* Storage is optional. */ }
  }
  function persistableCode(code) {
    return code === "inventory" || /^policy:/.test(String(code || ""));
  }
  function removeNotification(id, remember = false) {
    const node = visibleNotifications.get(id);
    const hadFocus = !!(node && document.activeElement && node.contains(document.activeElement));
    if (node) node.remove();
    visibleNotifications.delete(id);
    const timer = notificationTimers.get(id);
    if (timer) clearTimeout(timer);
    notificationTimers.delete(id);
    renderNotificationStack();
    if (remember && logic.shouldRememberDismissals(notificationScope)) { dismissed.add(id); persistDismissed(); }
    if (hadFocus) {
      const open = openDrawers();
      const drawerButton = open.length ? open[open.length - 1].querySelector("button[data-close-drawer]") : null;
      const target = drawerButton || (!notificationToggle.hidden ? notificationToggle : null) ||
        [...visibleNotifications.values()].find(node => !node.hidden)?.querySelector("button") || $("#refresh");
      if (target) target.focus();
    }
  }
  function notify(level, message, code = "general", options = {}) {
    const text = String(message || "").trim();
    if (!text) return;
    const persist = options.persist === undefined ? persistableCode(code) : !!options.persist;
    const id = logic.notificationID(level, code, text);
    if (persist && dismissed.has(id)) return;
    const existing = visibleNotifications.get(id);
    if (existing) {
      existing.dataset.recency = String(++notificationSequence);
      renderNotificationStack();
      return;
    }
    const article = el("article", undefined, "notification " + level);
    article.dataset.notificationId = id;
    article.dataset.persist = persist ? "1" : "0";
    article.dataset.level = level;
    article.dataset.recency = String(++notificationSequence);
    article.setAttribute("role", level === "error" || level === "warning" ? "alert" : "status");
    const body = el("div", undefined, "notification-body");
    body.append(el("strong", noticeLabels[level] || "Notice"), el("p", text));
    const close = el("button", "Dismiss", "dismiss");
    close.type = "button";
    close.setAttribute("aria-label", "Dismiss " + text.slice(0, 90));
    close.addEventListener("click", () => removeNotification(id, persist));
    article.append(body, close);
    visibleNotifications.set(id, article);
    renderNotificationStack();
    if (level === "success" || level === "info") notificationTimers.set(id, setTimeout(() => removeNotification(id), 10000));
  }

  // ---- API and shared helpers ---------------------------------------------
  async function api(path, body, key) {
    const headers = { Accept: "application/json" };
    if (body !== undefined) { headers["Content-Type"] = "application/json"; headers["X-CSRF-Token"] = csrf; }
    if (key) headers["Idempotency-Key"] = key;
    const response = await fetch("/api/v1/" + path, { method: body === undefined ? "GET" : "POST", headers, credentials: "same-origin", redirect: "error", body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(30000) });
    let data; try { data = await response.json(); } catch { throw new Error("The server returned an unexpected response. Your session may have expired; sign in again."); }
    if (!response.ok) {
      const detail = data.error, message = typeof detail === "string" ? detail : detail?.message;
      const error = new Error(message || "The request could not be completed.");
      if (response.status === 429 && /^\d+$/.test(response.headers.get("Retry-After") || "")) error.message += ` Retry in ${response.headers.get("Retry-After")} seconds.`;
      error.code = detail?.code; error.retryable = detail?.retryable; throw error;
    }
    if (path === "operations" || path === "audit") {
      const days = Number(response.headers.get("X-History-Retention-Days"));
      if (Number.isInteger(days) && days >= 1 && days <= 90) historyRetention[path] = days;
      const limit = Number(response.headers.get("X-History-Limit"));
      if (Number.isInteger(limit) && limit >= 1 && limit <= 100000) historyLimit[path] = limit;
      const windowName = String(response.headers.get("X-History-Window") || "").trim();
      if (/^[a-z0-9][a-z0-9-]{0,63}$/.test(windowName)) historyWindow[path] = windowName;
    }
    return data;
  }
  function badge(state) {
    const text = String(state || "Not reported");
    const rank = logic.severityValue(text);
    const cls = rank === 0 ? "badge good" : rank === 3 ? "badge bad" : rank >= 1 ? "badge warn" : "badge";
    return el("span", text, cls);
  }
  function formatTime(value) {
    const stamp = logic.timeValue(value);
    return stamp === null ? "Not reported" : new Date(stamp).toLocaleString();
  }
  function fillSelect(select, options, placeholder) {
    const old = select.value; select.replaceChildren(); if (placeholder !== undefined) select.append(new Option(placeholder, ""));
    [...new Set(options)].sort(logic.naturalCompare).forEach(value => select.append(new Option(value, value)));
    if ([...select.options].some(option => option.value === old)) select.value = old;
  }
  function scopeNamespaces() {
    const names = logic.namespaceOptions(inventory?.resources || []);
    // History remains filterable even when inventory is unavailable.
    for (const tab of ["operations", "audit"]) {
      for (const row of historyCache[tab].rows || []) {
        const target = logic.parseTarget(row.target);
        const name = target.namespace || (target.kind === "profile" ? target.name : "");
        if (name) names.push(name);
      }
    }
    if (namespaceContext && !names.includes(namespaceContext)) names.push(namespaceContext);
    return [...new Set(names)].sort(logic.naturalCompare);
  }
  function applyPodFilters(search) {
    const next = logic.queryPodFilters(search);
    if (next.node === podFilters.node && next.phase === podFilters.phase) return;
    podFilters = next;
    rowCache = {};
  }
  function clearPodFilterScope() {
    const url = new URL(location.href);
    const hadFilter = !!podFilters.node || !!podFilters.phase ||
      url.searchParams.has("node") || url.searchParams.has("phase");
    podFilters = { node: "", phase: "" };
    url.searchParams.delete("node");
    url.searchParams.delete("phase");
    if (hadFilter) window.history.replaceState(null, "", url.pathname + url.search);
    rowCache = {};
    return hadFilter;
  }
  function desired(resource) { return resource.desired || resource.details?.requested || "Not reported"; }
  function capacity(resource) { return resource.details?.capacity || "Not reported"; }
  function notebookCondition(resource) { return resource.details?.["condition.Ready"] || capacity(resource); }
  function podResourceDetail(resource, key, fallback) {
    const details = resource.details || {};
    if (details[key] !== undefined && details[key] !== "") return details[key];
    if (details[fallback] !== undefined && details[fallback] !== "") return details[fallback];
    return "Not reported";
  }
  function podReady(resource) {
    const details = resource.details || {};
    if (details.containers) return details.containers;
    if (details.ready === "true" || details.ready === true) return "Ready";
    if (details.ready === "false" || details.ready === false) return "Not ready";
    return details.ready || "Not reported";
  }
  function podRestartCount(resource) {
    const details = resource.details || {};
    if (details.restarts !== undefined && details.restarts !== "") return details.restarts;
    const keys = Object.keys(details).filter(key => key.startsWith("restarts."));
    if (!keys.length) return "Not reported";
    const total = keys.reduce((sum, key) => sum + (Number(details[key]) || 0), 0);
    return String(total);
  }
  function resourceSort(resource) {
    const owner = resource.owner || resource.details?.owner || "Not reported";
    return {
      name: resource.name || "", kind: resource.kind || "", type: resource.kind || "", namespace: resource.namespace || "",
      state: resource.state || "Not reported", desired: desired(resource), requested: resource.details?.requested || desired(resource),
      capacity: capacity(resource), condition: notebookCondition(resource), storageClass: resource.details?.storageClass || "",
      owner, ready: podReady(resource), restarts: podRestartCount(resource),
      node: resource.details?.node || "",
      cpuRequest: podResourceDetail(resource, "cpuRequest", "requested.cpu"), memoryRequest: podResourceDetail(resource, "memoryRequest", "requested.memory"),
      cpuLimit: podResourceDetail(resource, "cpuLimit", "limit.cpu"), memoryLimit: podResourceDetail(resource, "memoryLimit", "limit.memory"), time: ""
    };
  }
  // The configuration precondition is an internal request guard that travels
  // with a Profile update. Its raw hash is never rendered and never
  // searchable: it is read from the observation only to be sent back.
  function visibleDetails(resource) {
    return logic.visibleResourceDetails(resource);
  }
  function resourceSearch(resource) {
    const details = Object.entries(visibleDetails(resource)).map(([key, value]) => `${key} ${value}`).join(" ");
    return [resource.name, resource.kind, resource.namespace, resource.owner, resource.state, desired(resource), capacity(resource), details].filter(Boolean).join(" ");
  }
  // Projected resource details are read-only observations. They open the right
  // resource details drawer instead of an inline disclosure, so a long
  // key/value list never inflates a table row.
  function detailEntries(resource) {
    return Object.entries(visibleDetails(resource)).map(([key, value]) => `${key}: ${value}`);
  }
  // The identity of one observed resource: kind, namespace and name. Two
  // resources that share a name in different kinds or namespaces are
  // different resources and are never the same focus.
  function resourceKey(resource) {
    return [resource?.kind || "", resource?.namespace || "", resource?.name || ""].join("|");
  }
  // Names are the visible handle for the kinds whose full observation belongs
  // in the right-side details drawer. The separate Details control is removed
  // for these tables (Profiles opens its consolidated summary instead).
  const nameDetailsKinds = new Set(["Notebook", "Pod", "Node", "PersistentVolume", "PersistentVolumeClaim", "StorageClass"]);
  const inlineDetailsKinds = new Set(["Profile", ...nameDetailsKinds]);
  function detailsButton(resource) {
    if (inlineDetailsKinds.has(resource.kind)) return null;
    if (!detailEntries(resource).length) return null;
    const button = el("button", "Details", "detail-button");
    button.type = "button";
    button.dataset.openDrawer = "resource-details";
    button.dataset.resourceKey = resourceKey(resource);
    button.setAttribute("aria-label", `Details for ${resource.kind || "resource"} ${resource.name || ""}`.trim());
    // The clicked control is the opener: a pointer click does not always move
    // the focus, so document.activeElement is not a reliable substitute.
    button.addEventListener("click", event => openResourceDetails(resource, event?.currentTarget));
    return button;
  }
  function resourceNameNode(resource, options = {}) {
    const node = el("div", undefined, "resource-name");
    if (resource.kind === "Profile") {
      const open = el("button", resource.name || "Unnamed", "link-button");
      open.type = "button";
      open.addEventListener("click", event => openProfileSummary(resource.name, event?.currentTarget));
      node.append(open);
    } else if (options.openDetails || nameDetailsKinds.has(resource.kind)) {
      // The observed name is the visible handle for a resource whose details
      // belong in the right-side drawer. Storage claim and volume names keep
      // the same behaviour they already had.
      const open = el("button", resource.name || "Unnamed", "link-button");
      open.type = "button";
      open.dataset.openDrawer = "resource-details";
      open.dataset.resourceKey = resourceKey(resource);
      open.setAttribute("aria-label", `Details for ${resource.kind || "resource"} ${resource.name || ""}`.trim());
      open.addEventListener("click", event => openResourceDetails(resource, event?.currentTarget));
      node.append(open);
    } else node.append(el("span", resource.name || "Unnamed"));
    node.append(el("small", [resource.kind, resource.namespace, resource.owner].filter(Boolean).join(" · ")));
    const details = detailsButton(resource);
    if (details) node.append(details);
    return node;
  }
  function actorCell(record) {
    return el("span", logic.actorLabel(record), "actor-label");
  }
  function namespaceNameNode(resource) {
    const node = el("div", undefined, "resource-name");
    const open = el("button", resource.name || "Unnamed", "link-button");
    open.type = "button";
    open.addEventListener("click", () => openNamespaceSummary(resource.name));
    node.append(open);
    node.append(el("small", [resource.kind, resource.namespace].filter(Boolean).join(" · ")));
    return node;
  }
  function operationReceiptDetails(operation) {
    const receipts = logic.mutationReceipts(operation);
    if (!receipts.length) return null;
    const details = el("details", undefined, "receipt-details");
    const known = receipts.map(receipt => logic.receiptOutcomeLabel(receipt.outcome)).join(", ");
    details.append(el("summary", `${receipts.length} recorded mutation receipt${receipts.length === 1 ? "" : "s"} · ${known}`));
    const body = el("div", undefined, "receipt-body");
    body.append(el("p", "Mutation receipts record accepted API writes or verified observations, separately from planned changes. They can describe a partial operation and do not prove mount or admission readiness.", "hint"));
    if (String(operation.state || "") === "needs-review") {
      body.append(el("p", "The current operation stage remains needs-review. These receipts are known completed facts only; they do not prove that the whole operation succeeded.", "warning"));
    }
    simpleTable(body, ["Recorded outcome", "Kind", "Namespace", "Name", "UID", "Resource version"], receipts.map(receipt => [
      logic.receiptOutcomeLabel(receipt.outcome), receipt.kind, receipt.namespace || "—", receipt.name, receipt.uid || "—", receipt.resourceVersion || "—"
    ]));
    details.append(body);
    return details;
  }
  function operationStateCell(operation) {
    const wrap = el("div", undefined, "state-cell");
    wrap.append(badge(operation.state));
    // The slot is always rendered at a fixed size so a failure indicator can
    // never change row height or shift neighbouring columns.
    const slot = el("span", undefined, "state-error-slot");
    const error = String(operation.error || "").trim();
    const failure = error || (operation.historyExpired ? "Detailed operation history expired; compact retry record retained." : "");
    if (failure) {
      const tipID = `err-tip-${operation.id || ++errorTipSeq}`;
      const button = el("button", undefined, "error-dot");
      button.type = "button";
      button.setAttribute("aria-label", "Failure details: " + failure);
      button.setAttribute("aria-describedby", tipID);
      const tooltip = el("span", failure, "error-tooltip");
      tooltip.id = tipID;
      tooltip.setAttribute("role", "tooltip");
      button.addEventListener("click", event => {
        event?.stopPropagation?.();
        button.classList.remove("dismissed");
        $$(".error-dot.open").forEach(dot => { if (dot !== button) dot.classList.remove("open"); });
        button.classList.toggle("open");
      });
      // Escape dismissal must survive hover and keyboard focus; clear the
      // suppression when the pointer leaves or the dot is focused/clicked again.
      ["mouseleave", "focus"].forEach(type => button.addEventListener(type, () => button.classList.remove("dismissed")));
      slot.append(button, tooltip);
    }
    wrap.append(slot);
    const receipts = operationReceiptDetails(operation);
    if (receipts) wrap.append(receipts);
    return wrap;
  }
  function nodeCordonButton(resource) {
    const cordoned = resource.details?.unschedulable === "true";
    const verb = cordoned ? "Uncordon" : "Cordon";
    const button = el("button", verb, "secondary small");
    button.type = "button";
    button.disabled = !nodeCordonFeature;
    button.setAttribute("aria-label", `${verb} node ${resource.name || ""}`);
    if (!writes) button.title = "Changes are disabled by server policy.";
    else if (meta("node-cordon") !== "true") button.title = "Node cordon and uncordon are disabled by server policy.";
    else if (cordoned) button.title = "Prepare an uncordon plan: allow new Pods to be scheduled on this node again.";
    else button.title = "Prepare a cordon plan: stop new Pods from being scheduled on this node without evicting running Pods.";
    button.addEventListener("click", () => openNodeCordon(resource, cordoned ? "uncordon" : "cordon"));
    return button;
  }
  function idCell(entries) {
    const node = el("div", undefined, "id-cell");
    entries.filter(Boolean).forEach(([label, value]) => node.append(el("small", `${label}: ${value}`)));
    return node;
  }
  function podRestartSupport(resource) {
    const owner = String(resource.owner || resource.details?.owner || "").trim();
    // Absent evidence is left to the authoritative server revalidation. Clearly
    // unsupported observed controller owners are disabled with an explanation.
    return { ok: !owner || owner.startsWith("StatefulSet/"), owner };
  }
  function restartButton(resource) {
    const button = el("button", "Restart", "secondary small");
    const support = podRestartSupport(resource);
    button.type = "button";
    button.setAttribute("aria-label", `Restart Pod ${resource.namespace || ""}/${resource.name || ""}${support.ok ? "" : ", unavailable for observed owner " + support.owner}`);
    button.disabled = !podRestartFeature || !support.ok;
    if (!writes) button.title = "Changes are disabled by server policy.";
    else if (!notebookWrites) button.title = "Pod restarts require the confirmed Notebook controller contract.";
    else if (!meta("pod-restarts").startsWith("true")) button.title = "Pod restarts are disabled by server policy.";
    else if (!support.ok) button.title = `Observed owner ${support.owner} is not a StatefulSet. Restart is unavailable for this Pod; the server remains authoritative.`;
    else button.title = "Prepare a restart plan. The server validates that this Pod is a supported Notebook StatefulSet member.";
    button.addEventListener("click", () => openPodRestart(resource, support));
    if (support.ok) return button;
    const explanation = el("div", undefined, "restart-unavailable");
    explanation.append(button, el("small", "Observed owner is not a StatefulSet; restart unavailable."));
    return explanation;
  }

  // ---- Table definitions ---------------------------------------------------
  const tableDefs = {
    overview: {
      label: "overview resources", empty: "No matching resources. Choose another namespace or refresh inventory.",
      columns: [{ key: "name", label: "Resource" }, { key: "desired", label: "Desired / requested", type: "mixed" }, { key: "state", label: "Observed", type: "severity" }, { key: "capacity", label: "Capacity / condition", type: "quantity" }]
    },
    profiles: {
      label: "profile resources", empty: "No actual Profiles are observed in this scope. Namespaces have their own page and are not listed here.",
      columns: [{ key: "name", label: "Resource" }, { key: "type", label: "Type" }, { key: "namespace", label: "Namespace" }, { key: "state", label: "Observed", type: "severity" }, { key: "desired", label: "Desired / requested", type: "mixed" }]
    },
    namespaces: {
      label: "namespace resources", empty: "No matching Namespaces. Choose another namespace or refresh inventory.",
      columns: [{ key: "name", label: "Resource" }, { key: "state", label: "Status", type: "severity" }, { key: "age", label: "Age", type: "time" }]
    },
    storage: {
      label: "storage resources", empty: "No matching storage resources. Choose another namespace or refresh inventory.",
      columns: [{ key: "name", label: "Resource" }, { key: "type", label: "Resource Type" }, { key: "requested", label: "Requested", type: "quantity" }, { key: "state", label: "Observed", type: "severity" }, { key: "capacity", label: "Capacity", type: "quantity" }, { key: "storageClass", label: "Storage class" }]
    },
    notebooks: {
      label: "Notebook resources", empty: "No matching Notebook custom resources. Choose another namespace or refresh inventory. Pods and StatefulSets have their own views.",
      columns: [{ key: "name", label: "Resource" }, { key: "desired", label: "Desired / requested", type: "mixed" }, { key: "state", label: "Observed", type: "severity" }, { key: "condition", label: "Capacity / condition", type: "text" }, { key: "owner", label: "Owner" }]
    },
    pods: {
      label: "Pods", empty: "No matching Pods. Choose another namespace or refresh inventory.",
      columns: [
        { key: "name", label: "Pod" }, { key: "ready", label: "Ready", type: "ready" }, { key: "restarts", label: "Restarts", type: "number" },
        { key: "state", label: "State", type: "severity" }, { key: "node", label: "Node" }, { key: "owner", label: "Owner" }, { key: "actions", label: "Restart", sortable: false }
      ]
    },
    nodes: {
      label: "nodes", empty: "No matching Nodes. Refresh inventory to observe cluster nodes.",
      columns: [
        { key: "name", label: "Name" }, { key: "state", label: "Status", type: "severity" }, { key: "roles", label: "Roles" },
        { key: "age", label: "Age", type: "time" }, { key: "version", label: "Version" },
        { key: "internalIP", label: "Internal-IP" }, { key: "externalIP", label: "External-IP" },
        { key: "actions", label: "Cordon / Uncordon", sortable: false }
      ]
    },
    historyOperations: {
      label: "operation history", empty: "No operation records are loaded in this recent window.",
      columns: [{ key: "time", label: "Time", type: "time" }, { key: "actor", label: "Actor" }, { key: "kind", label: "Kind" }, { key: "namespace", label: "Namespace" }, { key: "name", label: "Name" }, { key: "action", label: "Action" }, { key: "state", label: "State", type: "severity" }]
    },
    historyAudit: {
      label: "audit history", empty: "No audit records are loaded in this recent window.",
      columns: [{ key: "time", label: "Time", type: "time" }, { key: "actor", label: "Actor" }, { key: "kind", label: "Kind" }, { key: "namespace", label: "Namespace" }, { key: "name", label: "Name" }, { key: "action", label: "Action" }, { key: "result", label: "Result", type: "severity" }, { key: "reason", label: "Reason" }, { key: "id", label: "IDs / stages" }]
    }
  };

  // Cells are materialized only for the current page, never for every hidden
  // row, so a large table does not create a DOM node per cluster resource.
  function resourceRow(resource, cellFactory) {
    return { namespace: resource.namespace || "", search: resourceSearch(resource), sort: resourceSort(resource), cellFactory };
  }
  function auditStageTable(group) {
    const distinctReason = group.stages.some(stage => {
      const reason = String(stage.reason ?? "").trim();
      return reason && reason !== group.reason;
    });
    const headings = ["Time", "Actor", "Action", "Result"];
    if (distinctReason) headings.push("Reason");
    headings.push("Audit ID");
    const table = el("table", undefined, "stage-table"), head = el("thead"), headRow = el("tr"), body = el("tbody");
    headings.forEach(label => headRow.append(el("th", label)));
    head.append(headRow); table.append(head);
    group.stages.forEach(stage => {
      const row = el("tr");
      const values = [formatTime(stage.time), actorCell(stage), stage.action || "—", badge(stage.result)];
      if (distinctReason) {
        const reason = String(stage.reason ?? "").trim();
        values.push(reason && reason !== group.reason ? reason : "—");
      }
      values.push(stage.id || "—");
      values.forEach(value => {
        const cell = el("td"); if (value instanceof Node) cell.append(value); else cell.textContent = value; row.append(cell);
      });
      body.append(row);
    });
    table.append(body);
    return table;
  }
  function buildRows(stateKey) {
    const resources = inventory?.resources || [];
    const scoped = logic.scopedResources(resources, namespaceContext);
    if (stateKey === "overview") {
      // StatefulSets stay visible as aggregate workload observations in the
      // Overview table even though the Notebooks page lists Notebook CRs only.
      return scoped.filter(resource => ["Notebook", "StatefulSet", "PersistentVolumeClaim", "PersistentVolume", "Pod", "Node", "ResourceQuota", "Event"].includes(resource.kind)).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), desired(resource), badge(resource.state), capacity(resource)]));
    }
    if (stateKey === "profiles") {
      return logic.profileResources(resources, namespaceContext).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), resource.kind || "—", resource.namespace || "—", badge(resource.state), desired(resource)]));
    }
    if (stateKey === "namespaces") {
      return resources.filter(resource => resource.kind === "Namespace").map(resource => ({
        namespace: resource.name || "",
        search: [resource.name, resource.kind, resource.state, resource.details?.createdAt].filter(Boolean).join(" "),
        sort: { name: resource.name || "", type: "Namespace", namespace: "", state: resource.state || "Not reported", age: resource.details?.createdAt || "" },
        cellFactory: () => [namespaceNameNode(resource), badge(resource.state), logic.formatAge(resource.details?.createdAt)]
      }));
    }
    if (stateKey === "storage") {
      return scoped.filter(resource => ["PersistentVolume", "PersistentVolumeClaim", "StorageClass"].includes(resource.kind)).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource, { openDetails: ["PersistentVolume", "PersistentVolumeClaim"].includes(resource.kind) }), resource.kind || "—", resource.details?.requested || "—", badge(resource.state), capacity(resource), resource.details?.storageClass || "—"]));
    }
    if (stateKey === "notebooks") {
      // The Notebooks page lists only kubeflow.org Notebook custom resources.
      // A Pod or StatefulSet with the same name is a different observed
      // resource and must never appear as a Notebook row.
      return logic.notebookResources(scoped).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), desired(resource), badge(resource.state), notebookCondition(resource), resource.owner || "Not reported"]));
    }
    if (stateKey === "pods") {
      // Node and phase filters are applied before row materialization. The
      // matcher is shared with the Node scoped count, so the count and the
      // destination cannot drift apart.
      return scoped.filter(resource => logic.podMatchesFilters(resource, podFilters)).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), podReady(resource), podRestartCount(resource), badge(resource.state), resource.details?.node || "—", resource.owner || resource.details?.owner || "Not reported", restartButton(resource)]));
    }
    if (stateKey === "nodes") {
      return resources.filter(resource => resource.kind === "Node").map(resource => {
        const details = resource.details || {};
        return {
          namespace: "",
          search: [resource.name, resource.state, details.roles, details.version, details.internalIP, details.externalIP, details.osImage, details.kernelVersion, details.containerRuntime, details.controlPlane === "true" ? "control-plane" : ""].filter(Boolean).join(" "),
          sort: {
            name: resource.name || "", type: "Node", namespace: "", state: resource.state || "Not reported", roles: details.roles || "",
            age: details.createdAt || "", version: details.version || "", internalIP: details.internalIP || "",
            externalIP: details.externalIP || "", osImage: details.osImage || "", kernelVersion: details.kernelVersion || "",
            containerRuntime: details.containerRuntime || ""
          },
          cellFactory: () => [
            resourceNameNode(resource), badge(resource.state), details.roles || "—", logic.formatAge(details.createdAt),
            details.version || "—", details.internalIP || "—", details.externalIP || "—", nodeCordonButton(resource)
          ]
        };
      });
    }
    if (stateKey === "historyOperations") {
      return (historyCache.operations.rows || []).map(operation => {
        const target = logic.parseTarget(operation.target);
        return {
          namespace: target.namespace || (target.kind === "profile" ? target.name : ""),
          search: [operation.id, operation.planID, target.kind, target.namespace, target.name, operation.action, operation.state, operation.step, operation.error, logic.actorLabel(operation),
            logic.mutationReceipts(operation).map(receipt => [logic.receiptOutcomeLabel(receipt.outcome), receipt.kind, receipt.namespace, receipt.name, receipt.uid].filter(Boolean).join(" ")).join(" ")].filter(Boolean).join(" "),
          sort: { time: operation.updatedAt || operation.createdAt, actor: logic.actorLabel(operation), kind: target.kind, namespace: target.namespace, name: target.name, action: operation.action, state: operation.state },
          cellFactory: () => [formatTime(operation.updatedAt || operation.createdAt), actorCell(operation), target.kind || "—", target.namespace || "—", target.name || "—", operation.action || "—", operationStateCell(operation)]
        };
      });
    }
    if (stateKey === "historyAudit") {
      return logic.groupAudit(historyCache.audit.rows || []).map(group => ({
        namespace: group.namespace || (group.kind === "profile" ? group.name : ""),
        search: [group.operationID, group.actorLabel, group.kind, group.namespace, group.name, group.action, group.result, group.reason, group.stages.map(stage => stage.id).join(" ")].filter(Boolean).join(" "),
        sort: { time: group.time, actor: group.actorLabel, kind: group.kind, namespace: group.namespace, name: group.name, action: group.action, result: group.result, reason: group.reason, id: group.operationID || group.id },
        cellFactory: () => {
          const detail = el("details", undefined, "stage-details");
          detail.append(el("summary", `${group.stages.length} stage${group.stages.length === 1 ? "" : "s"} · details`));
          let built = false;
          detail.addEventListener("toggle", () => {
            if (!detail.open || built) return;
            built = true;
            detail.append(auditStageTable(group));
          });
          const ids = group.operationID
            ? idCell([["Operation", group.operationID]])
            : idCell([["Audit ID", group.stages[0]?.id || "—"]]);
          const idWrap = el("div", undefined, "id-stack"); idWrap.append(ids, detail);
          return [formatTime(group.time), actorCell(group), group.kind || "—", group.namespace || "—", group.name || "—", group.action || "—", badge(group.result), group.reason || "—", idWrap];
        }
      }));
    }
    return [];
  }
  function cacheKey(stateKey) {
    if (stateKey === "historyOperations") return stateKey + "|" + historyCache.operations.at;
    if (stateKey === "historyAudit") return stateKey + "|" + historyCache.audit.at;
    const filters = stateKey === "pods" ? "|" + JSON.stringify(podFilters) : "";
    return stateKey + "|" + inventoryAt + "|" + namespaceContext + filters;
  }
  function tableRows(stateKey) {
    const key = cacheKey(stateKey);
    if (!rowCache[key]) rowCache[key] = buildRows(stateKey);
    return rowCache[key];
  }
  function historyLoaded(stateKey) {
    return stateKey === "historyAudit" ? historyCache.audit.rows !== null : historyCache.operations.rows !== null;
  }
  function historyWindowLabel(tab) {
    const value = historyWindow[tab];
    if (value === "recent-plus-unresolved") return "recent operations plus unresolved operations";
    if (value === "recent-audit-events") return "recent audit events";
    return value ? "server-reported " + value.replace(/-/g, " ") : tab === "audit" ? "recent audit events" : "recent operations plus unresolved operations";
  }
  function historyNote(stateKey) {
    const tab = stateKey === "historyAudit" ? "audit" : stateKey === "historyOperations" ? "operations" : null;
    if (!tab) return "";
    const limitNumber = tab === "audit" ? Math.min(historyLimit.audit || 1000, 1000) : (historyLimit.operations || 1000);
    const limit = limitNumber.toLocaleString();
    if (tab === "operations") return `Loaded window only: up to ${limit} ${historyWindowLabel(tab)}. Search and sorting apply only to this loaded window, not to all retained history.`;
    const days = historyRetention[tab] || 90;
    return `Loaded window only: up to ${limit} ${historyWindowLabel(tab)}. Retention is at most ${days} days and can be lower under configured row/byte pressure. This is not a complete historical export.`;
  }
  function invalidateHistoryCaches() {
    for (const tab of ["operations", "audit"]) {
      historyCache[tab].rows = null; historyCache[tab].at = 0;
      historyError[tab] = false; historyForce[tab] = true; historyEpoch[tab]++;
    }
    rowCache = {};
  }

  // ---- Rendering -----------------------------------------------------------
  // Cluster-scoped tables ignore the namespace filter. Their rows have no
  // namespace, and one page-scope selection must not hide other items.
  function clusterWideTable(stateKey) {
    return stateKey === "profiles" || stateKey === "namespaces" || stateKey === "nodes";
  }
  // A focused row is the observed resource with the very same kind, namespace
  // and name. A cluster-scoped kind never matches a namespaced row, and a
  // namespaced kind never matches a row of another namespace.
  function focusMatches(row, focus) {
    if (!focus) return true;
    if (String(row.sort?.name || "") !== focus.name) return false;
    if (focus.kind && String(row.sort?.type || "") !== focus.kind) return false;
    if (focus.namespace !== null && focus.namespace !== undefined && String(row.sort?.namespace ?? "") !== focus.namespace) return false;
    return true;
  }
  function buildControls(stateKey) {
    const def = tableDefs[stateKey], state = tableStates[stateKey];
    const controls = el("div", undefined, "table-controls");
    const searchLabel = el("label", undefined, "control-field");
    searchLabel.append(document.createTextNode("Search"));
    const search = el("input");
    search.type = "search"; search.value = state.search; search.placeholder = "Filter loaded rows";
    search.dataset.search = "1";
    search.setAttribute("aria-label", "Search " + def.label);
    // Editing the visible search invalidates a deep-linked resource focus, so
    // the URL never claims a filter the table no longer applies. A typed
    // search is not a focus mirror and is never cleared by a later focus.
    search.addEventListener("input", () => { const live = tableStates[stateKey]; live.search = search.value; live.focus = null; live.focusMirror = false; live.page = 1; dropResourceScope(); renderTable(stateKey); });
    searchLabel.append(search);
    const clear = el("button", "Clear filters", "secondary small");
    clear.type = "button";
    clear.dataset.clearFilters = "1";
    clear.setAttribute("aria-label", "Clear all filters for " + def.label);
    clear.addEventListener("click", () => clearFilters(stateKey));
    const count = el("span", undefined, "result-count");
    count.dataset.count = "1";
    count.setAttribute("role", "status");
    count.setAttribute("aria-live", "polite");
    if (clusterWideTable(stateKey)) {
      controls.append(searchLabel, clear, count);
      return controls;
    }
    const scopeLabel = el("label", undefined, "control-field");
    scopeLabel.append(document.createTextNode("Namespace"));
    const select = el("select");
    select.dataset.namespace = "1";
    select.setAttribute("aria-label", "Namespace for " + def.label);
    select.addEventListener("change", () => setNamespace(select.value));
    scopeLabel.append(select);
    if (stateKey === "pods") {
      const filters = el("div", undefined, "active-filters");
      filters.dataset.podFilters = "1";
      filters.setAttribute("role", "group");
      filters.setAttribute("aria-label", "Active Pod filters");
      filters.hidden = true;
      controls.append(searchLabel, scopeLabel, filters, clear, count);
      return controls;
    }
    controls.append(searchLabel, scopeLabel, clear, count);
    return controls;
  }
  // The visible URL filters are rebuilt on every render. They are plain text
  // built with textContent, so a node name is never interpreted as markup.
  function syncPodFilterChips(controls) {
    const filters = controls.querySelector("[data-pod-filters]");
    if (!filters) return;
    filters.replaceChildren();
    const entries = [];
    if (podFilters.node) entries.push(["Node", podFilters.node]);
    if (podFilters.phase) entries.push(["State", podFilters.phase]);
    filters.hidden = entries.length === 0;
    entries.forEach(([label, value]) => {
      const chip = el("span", undefined, "filter-chip");
      chip.append(el("span", label, "filter-chip-label"), el("strong", value));
      filters.append(chip);
    });
  }
  // The clear control is always reachable, but it is only actionable when a
  // search text, namespace scope, resource focus or typed Pod filter is applied.
  function syncClearFilters(controls, stateKey) {
    const clear = controls.querySelector("[data-clear-filters]");
    if (!clear) return;
    const state = tableStates[stateKey];
    const podFilterActive = stateKey === "pods" && (!!podFilters.node || !!podFilters.phase);
    const active = !!String(state.search || "").trim() || !!namespaceContext || !!state.focus || podFilterActive;
    clear.disabled = !active;
    clear.setAttribute("aria-disabled", active ? "false" : "true");
    clear.title = active
      ? "Clear the search text, the namespace scope, the Pod node and state filters and the resource focus, and return to the first page."
      : "No search text, namespace scope, Pod node or state filter, or resource focus is applied.";
  }
  // A focus is dropped on its own. The mirrored search is cleared with it, but
  // a search the administrator typed is kept.
  function clearResourceFocus(state) {
    if (!state || !state.focus) return false;
    state.focus = null;
    if (state.focusMirror) { state.search = ""; state.focusMirror = false; }
    return true;
  }
  // Reset search, namespace scope and pagination to their defaults. The
  // namespace scope is shared by the whole console, so it is cleared from the
  // URL as well and a deep-linked resource focus is dropped with it.
  function clearFilters(stateKey) {
    const state = tableStates[stateKey];
    const container = $(`[data-table="${stateKey}"]`);
    state.search = "";
    state.focus = null;
    state.focusMirror = false;
    state.page = 1;
    const search = container?.querySelector("[data-search]");
    if (search) search.value = "";
    // A node/state filter is URL state. Clear it and its visible chips before
    // the namespace branch rewrites the rest of the URL.
    if (stateKey === "pods") clearPodFilterScope();
    if (namespaceContext) setNamespace("");
    else { dropResourceScope(); renderTable(stateKey); }
    const focus = container?.querySelector("[data-search]") || container?.querySelector("[data-clear-filters]");
    if (focus) focus.focus();
  }
  function renderTable(stateKey) {
    const container = $(`[data-table="${stateKey}"]`);
    if (!container) return;
    const def = tableDefs[stateKey], state = tableStates[stateKey];
    let controls = container.querySelector(".table-controls");
    if (!controls) { controls = buildControls(stateKey); container.append(controls); }
    const previousOptions = controls.dataset.options || "";
    const options = scopeNamespaces();
    const signature = options.join("\u0000");
    const select = controls.querySelector("[data-namespace]");
    if (select) {
      if (signature !== previousOptions || !select.options.length) { fillSelect(select, options, "All permitted namespaces"); controls.dataset.options = signature; }
      select.value = options.includes(namespaceContext) ? namespaceContext : "";
    }
    const search = controls.querySelector("[data-search]");
    if (search && document.activeElement !== search) search.value = state.search;
    syncPodFilterChips(controls);
    syncClearFilters(controls, stateKey);
    container.querySelectorAll(".table-scroll, .pager, .table-note, .empty-state").forEach(node => node.remove());
    const count = controls.querySelector("[data-count]");
    if (!inventory && stateKey !== "historyAudit" && stateKey !== "historyOperations") {
      if (count) count.textContent = inventoryError ? "Inventory unavailable" : "Loading observations…";
      container.append(el("div", inventoryError ? "Inventory is currently unavailable. Use Refresh to try again." : "Loading the latest inventory observation. Sidebar navigation does not repeat this cluster read within its freshness window.", "empty-state"));
      return;
    }
    if (!historyLoaded(stateKey) && (stateKey === "historyAudit" || stateKey === "historyOperations")) {
      const tab = stateKey === "historyAudit" ? "audit" : "operations";
      if (count) count.textContent = historyError[tab] ? "History unavailable" : "Loading history…";
      container.append(el("div", historyError[tab] ? "History is currently unavailable. Use Refresh to try again." : "Loading history from the server. History does not wait for an inventory scan.", "empty-state"));
      return;
    }
    const rows = tableRows(stateKey);
    const scoped = clusterWideTable(stateKey) ? rows : rows.filter(row => !namespaceContext || !row.namespace || row.namespace === namespaceContext);
    const focused = state.focus ? scoped.filter(row => focusMatches(row, state.focus)) : scoped;
    const filtered = focused.filter(row => logic.searchMatch(row.search, state.search));
    const sorted = logic.sortRows(filtered, state.sortKey, state.sortDir, (def.columns.find(column => column.key === state.sortKey) || {}).type || "text");
    const view = logic.paginate(sorted, state.page, perPage);
    state.page = view.page;
    const loadedEvents = (historyCache.audit.rows || []).length;
    if (count) {
      if (stateKey === "historyAudit") {
        count.textContent = `${view.total} operation group${view.total === 1 ? "" : "s"}${state.search ? ` matching “${state.search}”` : ""} · ${loadedEvents.toLocaleString()} loaded audit event${loadedEvents === 1 ? "" : "s"} · page ${view.page} of ${view.pages}`;
      } else {
        count.textContent = `${view.total} ${view.total === 1 ? "record" : "records"}${state.search ? ` matching “${state.search}”` : ""} · page ${view.page} of ${view.pages}`;
      }
    }
    if (!view.items.length) {
      // A focused resource that is not observed must not look like an empty
      // scope; the message names the filter that produced the empty view.
      const focus = state.focus;
      const podScope = stateKey === "pods"
        ? [podFilters.node ? `node “${podFilters.node}”` : "", podFilters.phase ? `${podFilters.phase} state` : ""].filter(Boolean).join(" and ")
        : "";
      const empty = focus
        ? (focus.kind
          ? `No observed ${focus.kind} named “${focus.name}” matches this scope. Clear the filters to see every loaded row.`
          : `No observed resource named “${focus.name}” matches this scope. This deep link filters by name only; clear the filters to see every loaded row.`)
        : (podScope
          ? `No observed Pods match ${podScope} in this scope. Clear the filters to see every loaded row.`
          : def.empty);
      container.append(el("div", empty, "empty-state"));
    } else {
      const scroll = el("div", undefined, "table-scroll");
      scroll.tabIndex = 0;
      scroll.setAttribute("role", "region");
      scroll.setAttribute("aria-label", def.label + " table, scrollable");
      const table = el("table"), head = el("thead"), headRow = el("tr"), body = el("tbody");
      def.columns.forEach(column => {
        const th = el("th");
        th.scope = "col";
        if (column.sortable === false) { th.textContent = column.label; }
        else {
          const button = el("button", undefined, "sort-button");
          button.type = "button";
          button.dataset.sort = column.key;
          button.append(document.createTextNode(column.label));
          if (state.sortKey === column.key) {
            th.setAttribute("aria-sort", state.sortDir === "asc" ? "ascending" : "descending");
            button.append(el("span", state.sortDir === "asc" ? "▲" : "▼", "sort-indicator"));
          } else button.append(el("span", "↕", "sort-indicator"));
          button.setAttribute("aria-label", column.label + (state.sortKey === column.key ? `, sorted ${state.sortDir === "asc" ? "ascending" : "descending"}` : ", sortable"));
          button.addEventListener("click", () => {
            Object.assign(tableStates[stateKey], logic.nextSort(tableStates[stateKey], column.key));
            renderTable(stateKey);
            const restored = container.querySelector(`[data-sort="${column.key}"]`);
            if (restored) restored.focus();
          });
          th.append(button);
        }
        headRow.append(th);
      });
      head.append(headRow); table.append(head);
      view.items.forEach(item => {
        const row = el("tr");
        const cells = typeof item.cellFactory === "function" ? item.cellFactory() : item.cells;
        cells.forEach(value => {
          const cell = el("td");
          if (value instanceof Node) cell.append(value); else cell.textContent = value ?? "—";
          row.append(cell);
        });
        body.append(row);
      });
      table.append(body); scroll.append(table); container.append(scroll);
      if (view.pages > 1) {
        const pager = el("nav", undefined, "pager");
        pager.setAttribute("aria-label", "Pagination for " + def.label);
        const focusPager = direction => {
          const same = container.querySelector(`[data-pager="${direction}"]:not([disabled])`);
          const other = container.querySelector(`[data-pager="${direction === "previous" ? "next" : "previous"}"]:not([disabled])`);
          const search = container.querySelector("[data-search]");
          const target = same || other || search;
          if (target) target.focus();
        };
        const previous = el("button", "Previous", "secondary small"); previous.type = "button"; previous.dataset.pager = "previous"; previous.disabled = view.page <= 1;
        previous.addEventListener("click", () => { Object.assign(tableStates[stateKey], logic.withPage(tableStates[stateKey], view.page - 1)); renderTable(stateKey); focusPager("previous"); });
        const status = el("span", stateKey === "historyAudit"
          ? `Page ${view.page} of ${view.pages} · ${view.total} groups · ${loadedEvents.toLocaleString()} loaded events`
          : `Page ${view.page} of ${view.pages} · ${view.total} records`);
        const next = el("button", "Next", "secondary small"); next.type = "button"; next.dataset.pager = "next"; next.disabled = view.page >= view.pages;
        next.addEventListener("click", () => { Object.assign(tableStates[stateKey], logic.withPage(tableStates[stateKey], view.page + 1)); renderTable(stateKey); focusPager("next"); });
        pager.append(previous, status, next); container.append(pager);
      }
    }
    const note = historyNote(stateKey);
    if (note) container.append(el("p", note, "table-note"));
  }
  function renderWorkspaceHealth() {
    const summaryRoot = $("#workspace-health-summary");
    const select = $("#overview-namespace");
    if (select) {
      const options = scopeNamespaces(), signature = options.join("\u0000");
      if (signature !== select.dataset.options || !select.options.length) {
        fillSelect(select, options, "All permitted namespaces");
        select.dataset.options = signature;
      }
      select.value = options.includes(namespaceContext) ? namespaceContext : "";
    }
    if (!summaryRoot) return;
    const summary = logic.workspaceHealthSummary(inventory?.resources || [], namespaceContext);
    const cards = [["Nodes", "Nodes"], ["Profiles", "Profiles"], ["Notebooks", "Notebooks"], ["Pods", "Pods"], ["PersistentVolumeClaims", "PVCs"], ["PersistentVolumes", "PVs"], ["Events", "Events"]];
    summaryRoot.replaceChildren(...cards.map(([key, label]) => {
      const data = summary[key] || { total: 0, states: [] };
      const card = el("article", undefined, "health-card");
      if (key === "Nodes") card.classList.add("health-card-nodes");
      const head = el("div", undefined, "health-card-head");
      head.append(el("h3", label), el("strong", String(data.total)));
      const states = el("ul", undefined, "health-states");
      data.states.forEach(state => {
        const item = el("li");
        item.append(el("span", state.label), el("span", String(state.count), "health-count " + state.tone));
        states.append(item);
      });
      card.append(head, states);
      return card;
    }));
  }
  function syncOverviewTable() {
    const table = $("#overview-table"), button = $("#toggle-overview-table");
    if (table) table.hidden = !overviewTableExpanded;
    if (button) {
      button.textContent = overviewTableExpanded ? "Hide detailed resource table" : "Show detailed resource table";
      button.setAttribute("aria-expanded", overviewTableExpanded ? "true" : "false");
    }
  }
  function renderPage() {
    if (page === "overview") { renderWorkspaceHealth(); syncOverviewTable(); if (overviewTableExpanded) renderTable("overview"); }
    else if (page === "profiles") renderTable("profiles");
    else if (page === "namespaces") renderTable("namespaces");
    else if (page === "storage") renderTable("storage");
    else if (page === "notebooks") renderTable("notebooks");
    else if (page === "pods") renderTable("pods");
    else if (page === "nodes") renderTable("nodes");
    else if (page === "history") renderHistory();
  }
  function renderHistory() {
    $$("[data-history-tab]").forEach(button => {
      const active = button.dataset.historyTab === historyTab;
      button.setAttribute("aria-selected", active ? "true" : "false");
      button.className = active ? "tab active" : "tab";
      button.tabIndex = active ? 0 : -1;
    });
    $("#history-operations-panel").hidden = historyTab !== "operations";
    $("#history-audit-panel").hidden = historyTab !== "audit";
    renderTable(historyTab === "audit" ? "historyAudit" : "historyOperations");
  }
  function updateTimestamp() {
    const out = $("#observed-at");
    if (!inventory) { out.textContent = "Loading observations…"; return; }
    const observed = new Date(inventory.observedAt);
    const label = Number.isFinite(observed.getTime()) ? observed.toLocaleString() : "Unknown timestamp";
    // Freshness is the local receipt age, never the server clock.
    const age = monotonicNow() - inventoryAt;
    const stale = !Number.isFinite(age) || age > 60000;
    out.textContent = `${stale ? "STALE · " : ""}Observed ${label}`;
    out.classList.toggle("stale", stale);
  }

  // ---- Data loading --------------------------------------------------------
  async function loadInventory() {
    const data = await api("inventory");
    inventory = data; inventoryAt = monotonicNow(); rowCache = {};
    storageTemplates = approvedStorageTemplates(data.storageTemplates);
    updateTimestamp(); selectors(); renderPage();
    // The open details drawer follows the observed resource. A refresh never
    // rewrites a form the administrator is working in.
    syncResourceDetailsDrawer();
    // The Notebook management drawer follows the exact bound target. A
    // disappeared or replaced Notebook is reported instead of being silently
    // retargeted to another Notebook.
    syncNotebookManagement();
    // A refreshed observation can make a client-side validation message
    // stale. A server answer is never cleared by an unrelated refresh.
    syncStorageOperation();
    for (const form of [$("#profile-form"), $("#profile-delete-form")]) {
      if (!form) continue;
      const region = form.querySelector(".form-error");
      if (region && region.dataset.source === "validation") clearFormError(form);
    }
    (data.warnings || []).forEach(warning => notify("warning", warning, "inventory"));
  }
  function ensureHistory(tab, force = false) {
    // Each tab owns its request promise so switching History tabs never waits
    // for a slow inventory refresh and concurrent activations share one read.
    if (historyInflight[tab]) return historyInflight[tab];
    const cache = historyCache[tab];
    const fresh = cache.rows !== null && !historyError[tab] && !historyForce[tab] && !logic.needsRefresh(new Date(cache.at).toISOString(), Date.now(), 30000);
    if (!force && fresh) return Promise.resolve();
    const epoch = historyEpoch[tab];
    const request = (async () => {
      try {
        const data = await api(tab === "audit" ? "audit" : "operations");
        const limit = tab === "audit" ? Math.min(historyLimit.audit || 1000, 1000) : (historyLimit.operations || 1000);
        const rows = Array.isArray(data) ? data : [];
        if (historyEpoch[tab] !== epoch) {
          // A mutation invalidated history while this read was in flight.
          cache.rows = null; cache.at = 0; historyForce[tab] = true;
        } else {
          // Audit details are capped defensively; operations are already bounded
          // by the server including any separately bounded unresolved records.
          cache.rows = tab === "audit" ? rows.slice(0, limit) : rows;
          cache.at = Date.now();
          historyError[tab] = false;
          historyForce[tab] = false;
          rowCache = {};
        }
      } catch (error) {
        historyError[tab] = true;
        throw error;
      } finally {
        historyInflight[tab] = null;
        const invalidated = historyEpoch[tab] !== epoch;
        if (page === "history" && historyTab === tab) {
          renderHistory();
          if (invalidated) ensureHistory(tab).catch(() => {});
        }
      }
    })();
    historyInflight[tab] = request;
    return request;
  }
  function ensureActiveHistory() {
    if (page !== "history") return;
    const tab = historyTab;
    ensureHistory(tab).catch(() => {});
  }
  async function refresh(force = false) {
    if (loading || document.hidden) return;
    loading = true;
    const button = $("#refresh"); button.disabled = true;
    const jobs = [];
    const staleInventory = force || !inventory || monotonicNow() - inventoryAt >= 30000;
    if (staleInventory) jobs.push(loadInventory().then(() => { inventoryError = false; }, error => { inventoryError = true; renderPage(); throw error; }));
    if (page === "history") {
      const tab = historyTab;
      jobs.push(ensureHistory(tab, force).catch(error => { historyError[tab] = true; if (page === "history" && historyTab === tab) renderHistory(); throw error; }));
    }
    const results = await Promise.allSettled(jobs);
    for (const result of results) {
      if (result.status === "rejected") {
        const message = result.reason?.message || "The request could not be completed.";
        notify("error", message + (inventory ? " Previous observations remain visible and may be stale." : ""));
      }
    }
    loading = false; button.disabled = false;
  }

  // ---- Navigation ----------------------------------------------------------
  // A deep link may focus one observed resource inside a scoped page. The
  // resource identity is the canonical triple kind, namespace and name; the
  // name alone is ambiguous and is never treated as an identity.
  function resourceScope(search) {
    try {
      const url = new URL(String(search || ""), location.href);
      return {
        name: (url.searchParams.get("resource") || "").trim(),
        kind: (url.searchParams.get("kind") || "").trim(),
        namespace: (url.searchParams.get("namespace") || "").trim()
      };
    } catch { return { name: "", kind: "", namespace: "" }; }
  }
  function applyResourceScope(search) {
    const key = pageTableKeys[page];
    if (!key || !tableStates[key]) return null;
    const state = tableStates[key];
    const scope = resourceScope(search);
    if (!scope.name) {
      // A deep link that no longer names a resource must not leave a cached
      // focus behind. A search the administrator typed is kept.
      clearResourceFocus(state);
      return null;
    }
    // A link without a kind is a legacy link. It keeps the kind of a page that
    // lists exactly one kind, and it is otherwise a name filter.
    const kind = scope.kind || pageResourceKinds[page] || "";
    const focus = {
      name: scope.name,
      kind,
      // A namespaced kind is identified by its namespace. A cluster-scoped
      // kind is never identified by one, and a legacy link without a
      // namespace does not constrain the namespace at all.
      namespace: kind ? (clusterScopedKinds.has(kind) ? "" : (scope.namespace || null)) : null
    };
    state.focus = focus;
    state.focusMirror = true;
    state.search = scope.name;
    state.page = 1;
    // A resource URL on the Overview page must land on the detailed table, not
    // on a collapsed dashboard, however the page was reached.
    if (page === "overview") overviewTableExpanded = true;
    return focus;
  }
  // The resource identity is dropped from the URL. Every other query
  // parameter is preserved.
  function dropResourceScope() {
    const url = new URL(location.href);
    if (!url.searchParams.has("resource") && !url.searchParams.has("kind")) return false;
    url.searchParams.delete("resource");
    url.searchParams.delete("kind");
    window.history.replaceState(null, "", url.pathname + url.search);
    return true;
  }
  function activatePage() {
    const current = logic.routeFor(location.pathname, location.search);
    page = current.page;
    if (current.tab) historyTab = current.tab;
    applyPodFilters(location.search);
    applyResourceScope(location.search);
    $("#page-title").textContent = pageTitles[page]?.[0] || "Overview";
    $("#page-eyebrow").textContent = page === "overview" ? "CLUSTER AT A GLANCE" : "CLUSTER ADMINISTRATION";
    let subtitle = $("#page-subtitle");
    if (page === "overview") {
      subtitle?.remove();
    } else {
      if (!subtitle) {
        subtitle = el("p");
        subtitle.id = "page-subtitle";
        $("#page-title").parentElement.append(subtitle);
      }
      subtitle.textContent = pageTitles[page]?.[1] || "";
    }
    document.title = `${pageTitles[page]?.[0] || "Overview"} · Kubeflow Administration`;
    $$(".page").forEach(section => { section.hidden = section.id !== page; });
    $$("nav a").forEach(link => {
      const active = link.dataset.page === page;
      link.classList.toggle("active", active);
      if (active) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current");
    });
    renderPage(); updateTimestamp();
    ensureActiveHistory();
  }
  function cancelPlanForNavigation() {
    // Never clear a plan/key while an operation submission is in flight; the
    // same idempotency key is required for a safe retry.
    if (submitInflight) return;
    if ($("#plan-dialog").open) $("#plan-dialog").close();
    currentPlan = null; submitKey = null;
  }
  function navigate() { cancelPlanForNavigation(); activatePage(); refresh(false); }
  function setNamespace(value) {
    namespaceContext = value;
    rowCache = {};
    const url = new URL(location.href);
    if (value) url.searchParams.set("namespace", value); else url.searchParams.delete("namespace");
    // A namespace change invalidates a deep-linked resource focus: the URL and
    // the visible filter are dropped together so they never disagree.
    url.searchParams.delete("resource");
    url.searchParams.delete("kind");
    window.history.replaceState(null, "", url.pathname + url.search);
    const key = pageTableKeys[page];
    if (key && tableStates[key]) tableStates[key].page = 1;
    // The namespace scope is shared: a resource focus of any table belongs to
    // the scope it was taken in. It is dropped even when the URL no longer
    // names the resource. A search the administrator typed is kept.
    for (const stateKey of Object.keys(tableStates)) clearResourceFocus(tableStates[stateKey]);
    selectors(true);
    renderPage();
  }
  $$("nav a").forEach(link => link.addEventListener("click", event => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const target = new URL(link.href, location.href);
    const routeInfo = logic.routeFor(target.pathname, target.search);
    if (target.origin !== location.origin || !pageTitles[routeInfo.page]) return;
    event.preventDefault();
    const next = new URL(target.pathname, location.origin);
    if (namespaceContext) next.searchParams.set("namespace", namespaceContext);
    window.history.pushState(null, "", next.pathname + next.search);
    navigate();
  }));
  window.addEventListener("popstate", () => {
    namespaceContext = logic.queryNamespace(location.search);
    closeDrawers(false);
    selectors(true);
    navigate();
    // The drawer that was open is closed by the navigation. The focus moves
    // to the destination heading so no keyboard user is left on a hidden
    // element.
    const heading = $("#page-title");
    if (heading) heading.focus();
  });
  $$("[data-history-tab]").forEach(button => {
    button.addEventListener("click", () => {
      cancelPlanForNavigation();
      historyTab = button.dataset.historyTab === "audit" ? "audit" : "operations";
      window.history.pushState(null, "", logic.historyHref(historyTab, namespaceContext));
      renderHistory();
      const tab = historyTab;
      ensureHistory(tab).catch(error => { historyError[tab] = true; if (historyTab === tab) renderHistory(); notify("error", error.message); });
    });
    button.addEventListener("keydown", event => {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      event.preventDefault();
      const next = historyTab === "operations" ? "audit" : "operations";
      const target = $(`[data-history-tab="${next}"]`);
      if (target) { target.click(); target.focus(); }
    });
  });
  document.addEventListener("click", event => {
    const link = event.target.closest?.("a[data-scoped-link]");
    if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const target = new URL(link.href, location.href);
    if (target.origin !== location.origin) return;
    event.preventDefault();
    // A Node scoped Pods link is a fresh view, not the previous Pods search.
    // The URL filters are authoritative and the visible search starts empty so
    // unrelated state cannot hide rows behind the typed node view.
    if (link.dataset.podNodeLink === "1") {
      const pods = tableStates.pods;
      pods.search = "";
      pods.focus = null;
      pods.focusMirror = false;
      pods.page = 1;
    }
    // A scoped summary link to Overview should land on the full table rather
    // than a collapsed dashboard, so Events and quota rows are immediately visible.
    if (target.pathname === "/" && link.closest?.("#namespace-summary-drawer")) overviewTableExpanded = true;
    closeDrawers(false);
    namespaceContext = logic.queryNamespace(target.search);
    window.history.pushState(null, "", target.pathname + target.search);
    selectors(true);
    navigate();
    // The link lived inside a drawer that is now closed. Move focus to the
    // destination page heading so keyboard and screen reader users are not
    // left on a hidden element.
    const heading = $("#page-title");
    if (heading) heading.focus();
  });

  // ---- Drawers -------------------------------------------------------------
  function setBackgroundInert(value) {
    const main = $("main"), sidebar = $(".sidebar");
    if (main) main.inert = value;
    if (sidebar) sidebar.inert = value;
  }
  function showFormError(form, message, source = "server") {
    let region = form.querySelector(".form-error");
    if (!region) { region = el("p", undefined, "form-error"); region.setAttribute("role", "alert"); form.append(region); }
    region.textContent = String(message || "The request could not be completed.");
    region.dataset.source = source;
    region.hidden = false;
  }
  function clearFormError(form) {
    const region = form.querySelector(".form-error");
    if (region) { region.textContent = ""; region.hidden = true; delete region.dataset.source; }
  }
  function clearFormErrors() { $$(".form-error").forEach(region => { region.textContent = ""; region.hidden = true; delete region.dataset.source; }); }
  // A form may refuse its own submission before any request is made. The
  // message stays in the drawer and is never sent to the server.
  function preSubmitError(form) {
    if (form.id === "profile-form") return profileSubmitError();
    if (form.id === "notebook-form") {
      // A Notebook management form opened for one observed resource never
      // submits against a target that disappeared, was replaced or is no
      // longer the selected resource.
      return notebookTargetError();
    }
    if (form.id === "profile-delete-form") {
      const name = String($("#profile-delete-name")?.value || "");
      const reason = String($("#profile-delete-reason")?.value || "");
      const confirmName = String($("#profile-delete-confirm")?.value || "");
      return logic.profileDeleteValidation(name, { reason, confirmName }).message;
    }
    if (form.id === "storage-form") return storageCreateError();
    if (form.id === "storage-edit-form" || form.id === "storage-delete-form") return storageSubmitError(form);
    return "";
  }
  function openDrawers() { return $$(".drawer").filter(drawer => !drawer.hidden); }
  function openDrawer(drawer, opener) {
    if (!drawer) return;
    closeDrawers(false);
    // The control that opened the drawer is remembered. A pointer click does
    // not always move the focus, so the active element is only a fallback.
    drawerOpener = opener && opener !== document.body ? opener : document.activeElement;
    $("#drawer-scrim").hidden = false;
    drawer.hidden = false;
    document.body.classList.add("drawer-open");
    setBackgroundInert(true);
    const first = drawer.querySelector("input:not([type='hidden']):not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled]), a[href]");
    if (first) first.focus();
  }
  // A control may only take the focus back when it is really there: it must be
  // connected, visible, enabled and not buried inside a hidden ancestor. A
  // drawer that was closed keeps its DOM, so its controls stay connected while
  // they can no longer be focused.
  function focusEligible(node) {
    if (!node || node === document.body) return false;
    if (node.isConnected === false) return false;
    if (node.hidden) return false;
    if (node.disabled) return false;
    if (typeof node.closest === "function" && node.closest("[hidden]")) return false;
    if (typeof node.getClientRects === "function" && node.getClientRects().length === 0) return false;
    if (typeof node.focus !== "function") return false;
    return true;
  }
  // The control that may take the focus back is the visible one of the very
  // same observed resource. The first matching control in the document may
  // belong to a hidden table or to another resource.
  function resourceDetailsOpener(key) {
    if (!key) return null;
    return $$(`[data-resource-key]`).find(node => node.dataset.resourceKey === key && focusEligible(node)) || null;
  }
  function restoreDrawerFocus(drawer) {
    let target = focusEligible(drawerOpener) ? drawerOpener : null;
    if (!target && drawer?.id === "resource-details-drawer" && drawerResource) target = resourceDetailsOpener(drawerResource.key);
    if (!target && drawer?.id === "notebook-drawer" && notebookManagementFromDetails) {
      // A management drawer opened from the details drawer returns to the
      // visible control of the very same Notebook row, or to the page heading.
      // It never focuses a control inside a closed details drawer or an
      // unrelated page-level management button.
      target = notebookReturnKey ? resourceDetailsOpener(notebookReturnKey) : null;
      if (!target) target = $("#page-title");
    }
    if (!target && (drawer?.id === "storage-edit-drawer" || drawer?.id === "storage-delete-drawer")) {
      // Storage actions are opened from the observed details. The Edit/Delete
      // control lives inside that now closed drawer, so the focus returns to
      // the visible name control of the exact same storage resource.
      target = storageOperation?.returnKey ? resourceDetailsOpener(storageOperation.returnKey) : null;
    }
    if (!target) {
      const kind = String(drawer?.id || "").replace(/-drawer$/, "");
      // The management control of the drawer, when it is visible. Another
      // drawer never borrows the details control.
      if (kind && kind !== "resource-details") target = $$(`[data-open-drawer="${kind}"]`).find(node => focusEligible(node)) || null;
    }
    if (!target) {
      target = $("#page-title");
      if (target && !target.hasAttribute("tabindex")) target.setAttribute("tabindex", "-1");
    }
    if (target && typeof target.focus === "function") target.focus();
    drawerOpener = null;
  }
  function closeDrawers(restore = true) {
    const open = openDrawers();
    if (!open.length) return;
    const last = open[open.length - 1];
    open.forEach(drawer => { drawer.hidden = true; });
    $("#drawer-scrim").hidden = true;
    document.body.classList.remove("drawer-open");
    setBackgroundInert(false);
    clearFormErrors();
    if (restore) restoreDrawerFocus(last);
    else drawerOpener = null;
    // The closed details drawer no longer follows a resource.
    if (last?.id === "resource-details-drawer") drawerResource = null;
    if (last?.id === "notebook-drawer") {
      notebookManagementFromDetails = false;
      notebookReturnKey = "";
      notebookTarget = null;
      notebookTargetWarning("");
    }
    if (last?.id === "storage-edit-drawer" || last?.id === "storage-delete-drawer") {
      storageOperation = null;
      storageEpoch++;
    }
  }
  function focusable(drawer) {
    return $$("a[href], button:not([disabled]), input:not([type='hidden']):not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])", drawer)
      .filter(node => !node.hidden && !node.closest("[hidden]") && (typeof node.getClientRects !== "function" || node.getClientRects().length > 0));
  }
  function openResourceDetails(resource, opener) {
    // A Profile has one consolidated summary. Every context that would open
    // generic resource details for a Profile hands over to that summary, so
    // the same owner and quota are never presented twice.
    if ((resource?.kind || "") === "Profile") {
      openProfileSummary(resource.name || "", opener);
      return;
    }
    const drawer = $("#resource-details-drawer");
    drawerResource = {
      key: resourceKey(resource),
      kind: resource.kind || "",
      namespace: resource.namespace || "",
      name: resource.name || "",
      uid: resource.uid || "",
      last: resource
    };
    renderResourceDetails(resource, {});
    openDrawer(drawer, opener);
  }
  // A Node's scoped view counts only observed Pod resources whose canonical
  // phase is Running and whose projected node is exactly this Node. It is a
  // snapshot count, never a live atomic proof, and the same typed filters drive
  // the destination table. The href omits namespace so the link shows every
  // namespace, and URLSearchParams keeps an unusual node name safely encoded.
  function nodePodScopedView(resource) {
    const name = String(resource.name || "");
    const running = logic.runningPodsOnNode(inventory?.resources || [], name);
    const count = running.length;
    const section = el("section", undefined, "node-pod-scope");
    section.dataset.nodePodScope = "1";
    section.append(el("h3", "Scoped view"));
    const link = el("a", undefined, "summary-link node-pod-scope-link");
    link.href = logic.podNodeHref(name);
    link.dataset.scopedLink = "1";
    link.dataset.podNodeLink = "1";
    link.setAttribute("aria-label", `${count} Running Pod${count === 1 ? "" : "s"} on node ${name}. Open the Pods page filtered to this node and Running state.`);
    link.append(el("strong", String(count)), el("span", `Running Pod${count === 1 ? "" : "s"} on this node`));
    section.append(link, el("p", "Counted from the latest inventory observation across all namespaces; this is not a live atomic proof. The link applies exactly this node and Running state to the Pods page.", "hint"));
    return section;
  }
  // The body of the drawer is rebuilt from one observation. It never claims to
  // be the latest observation while it shows an older one.
  function renderResourceDetails(resource, options = {}) {
    const name = resource.name || "Unnamed";
    $("#resource-details-title").textContent = name;
    const body = $("#resource-details-body"); body.replaceChildren();
    if (options.replaced) {
      body.append(el("p", "This resource was replaced by a new observation while the drawer was open. The values below are the new observation, not the one that was open.", "warning"));
    }
    if (options.gone) {
      body.append(el("p", "This resource is no longer present in the latest observation. The values below are the last observation of it.", "warning"));
    }
    body.append(el("p", options.gone
      ? `Observed ${resource.kind || "resource"} ${name}${resource.namespace ? ` in namespace ${resource.namespace}` : ""}.`
      : `Observed ${resource.kind || "resource"} ${name}${resource.namespace ? ` in namespace ${resource.namespace}` : ""}. Every value below comes from this console's latest inventory observation.`));
    const facts = el("dl", undefined, "detail-list");
    [
      ["Kind", resource.kind || "Not reported"],
      ["Namespace", resource.namespace || "Cluster scoped"],
      ["Owner", resource.owner || resource.details?.owner || "Not reported"],
      ["Observed state", resource.state || "Not reported"]
    ].forEach(([label, value]) => facts.append(el("dt", label), el("dd", value)));
    body.append(facts);
    // The scoped Pod view sits above the raw projected details of a Node.
    if ((resource.kind || "") === "Node") body.append(nodePodScopedView(resource));
    // The projected details keep their original labels and values. They are
    // written as text, never as markup.
    const entries = detailEntries(resource);
    if (entries.length) {
      body.append(el("h3", "Observed details"));
      const rows = el("div", undefined, "detail-rows");
      entries.forEach(entry => rows.append(el("div", entry, "detail-row")));
      body.append(rows);
    } else {
      body.append(el("p", "No additional projected details were observed for this resource.", "hint"));
    }
    if ((resource.kind || "") === "Notebook") {
      const actions = el("div", undefined, "drawer-actions");
      const manage = el("button", "Manage Notebook", "secondary");
      manage.type = "button";
      manage.setAttribute("aria-label", `Manage Notebook ${resource.namespace || ""}/${name}`.trim());
      const policyReason = notebookManagementReason();
      const goneReason = options.gone ? "This Notebook is no longer observed. Refresh inventory before managing it." : "";
      const unavailable = goneReason || policyReason;
      manage.disabled = !!unavailable;
      manage.title = unavailable || "Open Notebook management for this exact observed Notebook.";
      // The same drawer, prefill and policy gates as the page-level Notebook
      // action are reused. The selected action is never silently switched.
      manage.addEventListener("click", () => openNotebookManagement(resource));
      actions.append(manage);
      body.append(actions);
      if (unavailable) body.append(el("p", "Manage Notebook is unavailable: " + unavailable, "hint"));
    }
    if (storageActionKinds.has(resource.kind || "")) {
      const actions = el("div", undefined, "drawer-actions");
      const edit = el("button", "Edit", "secondary");
      edit.type = "button";
      edit.setAttribute("aria-label", `Edit ${resource.kind} ${resource.namespace ? resource.namespace + "/" : ""}${name}`);
      const remove = el("button", "Delete", "danger");
      remove.type = "button";
      remove.setAttribute("aria-label", `Delete ${resource.kind} ${resource.namespace ? resource.namespace + "/" : ""}${name}`);
      const editReason = options.gone ? "This resource is no longer observed. Refresh inventory and open it again." : (!resource.uid ? "This resource has no stable observed UID. Refresh inventory before opening a change." : storageFeatureReason("edit"));
      const deleteReason = options.gone ? "This resource is no longer observed. Refresh inventory and open it again." : (!resource.uid ? "This resource has no stable observed UID. Refresh inventory before opening a change." : storageFeatureReason("delete"));
      edit.disabled = !!editReason;
      remove.disabled = !!deleteReason;
      edit.title = editReason || "Open the restricted YAML editor for this exact observed resource.";
      remove.title = deleteReason || "Prepare a deletion plan for this exact observed resource.";
      edit.addEventListener("click", event => openStorageAction(resource, "edit", event?.currentTarget));
      remove.addEventListener("click", event => openStorageAction(resource, "delete", event?.currentTarget));
      actions.append(edit, remove);
      body.append(actions);
      if (editReason) body.append(el("p", "Edit is unavailable: " + editReason, "hint"));
      if (deleteReason) body.append(el("p", "Delete is unavailable: " + deleteReason, "hint"));
    }
  }
  // The observed resource behind the open drawer, if any.
  function observedResource(identity) {
    if (!identity) return null;
    const resources = inventory?.resources || [];
    return resources.find(resource =>
      (resource.kind || "") === identity.kind &&
      (resource.namespace || "") === identity.namespace &&
      (resource.name || "") === identity.name) || null;
  }
  // A refreshed observation is never shown as the latest one while the drawer
  // still holds an older body. The drawer follows the resource: it is updated
  // in place, and a resource that is gone is reported instead of being hidden.
  function syncResourceDetailsDrawer() {
    const drawer = $("#resource-details-drawer");
    if (!drawer || drawer.hidden || !drawerResource) return;
    const found = observedResource(drawerResource);
    if (!found) {
      notify("warning", `The observed ${drawerResource.kind} ${drawerResource.name} is no longer present in the latest observation. The details drawer shows its last observation.`, "inventory");
      renderResourceDetails(drawerResource.last || {}, { gone: true });
      return;
    }
    const uid = found.uid || "";
    const replaced = !!drawerResource.uid && !!uid && drawerResource.uid !== uid;
    const active = document.activeElement;
    const hadFocus = !!(active && drawer.contains(active));
    renderResourceDetails(found, { replaced });
    drawerResource.uid = uid;
    drawerResource.last = found;
    // The refresh never steals the focus from the open drawer.
    if (hadFocus && (!active || active.isConnected === false)) {
      const close = drawer.querySelector("button[data-close-drawer]") || drawer.querySelector("button");
      if (close) close.focus();
    }
  }
  function openProfileSummary(name, opener) {
    const drawer = $("#profile-summary-drawer");
    $("#profile-summary-title").textContent = name;
    const body = $("#profile-summary-body"); body.replaceChildren();
    body.append(el("p", `Observed resources in namespace ${name}, associated with Profile ${name} by name convention. Counts come only from this console's live observations.`));
    const counts = logic.profileSummary(inventory?.resources || [], name);
    if (counts.length) {
      const list = el("ul", undefined, "summary-counts");
      counts.forEach(entry => {
        const item = el("li");
        const link = el("a", undefined, "summary-link");
        link.href = logic.hrefFor(entry.href, name);
        link.dataset.scopedLink = "1";
        link.append(el("strong", String(entry.count)), el("span", entry.label));
        item.append(link); list.append(item);
      });
      body.append(el("h3", "Scoped views"), list);
    } else {
      body.append(el("p", "No Notebook, Pod or persistent volume claim resources are currently observed in this profile namespace. Zero counts are omitted rather than inferred.", "hint"));
    }
    body.append(el("p", "Kubeflow pipeline runs are not integrated with this console. No pipeline count is shown and none is inferred from Pods.", "hint"));
    body.append(profileConfiguration(name));
    // The management actions follow the observed configuration.
    const actions = el("div", undefined, "drawer-actions");
    const edit = el("button", "Edit profile", "secondary");
    edit.type = "button";
    edit.setAttribute("aria-label", "Edit the quota of profile " + name);
    edit.addEventListener("click", () => openProfileUpdate(name));
    actions.append(edit);
    const profile = observedProfile(name);
    if (profileDeletionAllowed(profile)) {
      const remove = el("button", "Delete profile", "danger");
      remove.type = "button";
      remove.setAttribute("aria-label", "Delete profile " + name);
      remove.addEventListener("click", () => openProfileDelete(profile));
      actions.append(remove);
    }
    body.append(actions);
    openDrawer(drawer, opener);
  }
  // Individual resources inside a namespace summary link to the scoped page
  // that lists them, focused on the selected resource through the shared
  // resource identity parameters. The identity is the canonical triple kind,
  // namespace and name, so a claim never selects its volume. Kinds without a
  // listing page stay plain text.
  const resourceScopeTargets = {
    Notebook: "/notebooks", StatefulSet: "/", Pod: "/pods",
    PersistentVolumeClaim: "/storage", ResourceQuota: "/", Event: "/"
  };
  function resourceScopeCell(resource) {
    const name = resource.name || "Unnamed";
    const target = resourceScopeTargets[resource.kind];
    if (!target || !resource.name) return el("span", name);
    const url = new URL(target, location.origin);
    if (resource.namespace) url.searchParams.set("namespace", resource.namespace);
    url.searchParams.set("kind", resource.kind);
    url.searchParams.set("resource", resource.name);
    const link = el("a", name, "summary-resource");
    link.href = url.pathname + url.search;
    link.dataset.scopedLink = "1";
    link.setAttribute("aria-label", `Open ${resource.kind} ${name}${resource.namespace ? " in namespace " + resource.namespace : ""}`);
    return link;
  }
  function openNamespaceSummary(name) {
    const drawer = $("#namespace-summary-drawer");
    $("#namespace-summary-title").textContent = name;
    const body = $("#namespace-summary-body"); body.replaceChildren();
    body.append(el("p", `Observed resources in namespace ${name}. Counts come only from this console's live observations.`));
    const groups = logic.namespaceResources(inventory?.resources || [], name);
    if (!groups.length) {
      body.append(el("p", "No resources are currently observed in this namespace. An empty summary does not imply the namespace has no workloads; they may be outside this console's observation scope.", "hint"));
    } else {
      const list = el("ul", undefined, "summary-counts");
      groups.forEach(group => {
        const item = el("li");
        if (group.href) {
          const link = el("a", undefined, "summary-link");
          link.href = logic.hrefFor(group.href, name);
          link.dataset.scopedLink = "1";
          link.append(el("strong", String(group.count)), el("span", group.label));
          item.append(link);
        } else {
          const row = el("div", undefined, "summary-link static");
          row.append(el("strong", String(group.count)), el("span", group.label));
          item.append(row);
        }
        list.append(item);
      });
      body.append(el("h3", "Scoped views"), list);
      groups.forEach(group => {
        body.append(el("h3", `${group.label} (${group.count})`));
        simpleTable(body, ["Name", "State"], group.items.slice(0, 50).map(resource => [resourceScopeCell(resource), badge(resource.state)]));
        if (group.items.length > 50) body.append(el("p", `Showing first 50 of ${group.items.length} resources.`, "hint"));
      });
    }
    openDrawer(drawer);
  }
  function openNodeCordon(resource, verb = "cordon") {
    if (!nodeCordonFeature) return;
    const form = $("#node-cordon-form");
    const name = resource.name || "";
    const uncordon = verb === "uncordon";
    const kind = uncordon ? "node.uncordon" : "node.cordon";
    form.elements.kind.value = kind;
    form.elements.name.value = name;
    form.elements.reason.value = "";
    if (form.elements.force) form.elements.force.checked = false;
    $("#node-cordon-title").textContent = `${uncordon ? "Uncordon" : "Cordon"} ${name}`;
    $("#node-cordon-description").textContent = uncordon
      ? `Uncordon node ${name}? This sets spec.unschedulable=false (same as kubectl uncordon) and removes SchedulingDisabled so new Pods can be scheduled onto this node again.`
      : `Cordon node ${name}? This sets spec.unschedulable=true (same as kubectl cordon) and updates the node status to include SchedulingDisabled alongside its current readiness state. New Pods will not be scheduled onto this node; existing running Pods are not evicted.`;
    const controlPlane = resource.details?.controlPlane === "true";
    const notReady = String(resource.state || "").startsWith("NotReady");
    const cordon = kind === "node.cordon";
    let warningText = "";
    if (cordon && controlPlane && notReady) {
      warningText = "This node is both control-plane and NotReady. The server requires Force to cordon it.";
    } else if (cordon && controlPlane) {
      warningText = "This node carries a control-plane role. Cordoning it can affect control-plane availability; the server requires Force to cordon a control-plane node.";
    } else if (cordon && notReady) {
      warningText = "This node is currently NotReady. The server requires Force to cordon a NotReady node.";
    }
    const warning = $("#node-cordon-warning");
    warning.hidden = !warningText;
    warning.textContent = warningText;
    openDrawer($("#node-cordon-drawer"));
  }
  function openPodRestart(resource, support = podRestartSupport(resource)) {
    if (!podRestartFeature || !support.ok) return;
    const form = $("#pod-restart-form");
    form.elements.namespace.value = resource.namespace || "";
    form.elements.name.value = resource.name || "";
    form.elements.reason.value = "";
    const owner = resource.owner || resource.details?.owner;
    $("#pod-restart-target").textContent = `Target pod:${resource.namespace || "?"}/${resource.name || "?"}${owner ? ` · observed owner ${owner}` : ""}`;
    openDrawer($("#pod-restart-drawer"));
  }
  $$("[data-open-drawer]").forEach(button => button.addEventListener("click", () => {
    // The Profile management drawer always starts from a clean create form.
    if (button.dataset.openDrawer === "profile") openProfileCreate();
    // The page-level Notebook action starts from a clean context and binds the
    // currently selected observed Notebook instead of bypassing the UID guard.
    if (button.dataset.openDrawer === "notebook") {
      openNotebookPageManagement(button);
      return;
    }
    openDrawer($(`#${button.dataset.openDrawer}-drawer`));
  }));
  $$("[data-close-drawer]").forEach(button => button.addEventListener("click", () => closeDrawers(true)));
  $("#drawer-scrim").addEventListener("click", () => closeDrawers(true));
  document.addEventListener("keydown", event => {
    const open = openDrawers();
    if (open.length) {
      if (event.key === "Escape") { event.preventDefault(); closeDrawers(true); return; }
      if (event.key === "Tab") {
        const drawer = open[open.length - 1], nodes = focusable(drawer);
        if (!nodes.length) return;
        const first = nodes[0], last = nodes[nodes.length - 1];
        if (event.shiftKey && (document.activeElement === first || !drawer.contains(document.activeElement))) { event.preventDefault(); last.focus(); }
        else if (!event.shiftKey && (document.activeElement === last || !drawer.contains(document.activeElement))) { event.preventDefault(); first.focus(); }
        return;
      }
    }
    if (event.key === "Escape") {
      const dots = $$(".error-dot.open");
      const focused = document.activeElement?.classList?.contains("error-dot") ? document.activeElement : null;
      if (dots.length || focused) {
        event.preventDefault();
        dots.forEach(dot => { dot.classList.remove("open"); dot.classList.add("dismissed"); });
        if (focused) {
          focused.classList.add("dismissed");
          focused.blur();
        }
        return;
      }
    }
    if (event.key === "/" && !event.metaKey && !event.ctrlKey && !event.altKey) {
      const target = event.target;
      if (target && (["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName) || target.isContentEditable)) return;
      if ($("dialog[open]") || open.length) return;
      const search = $(`[data-table="${page === "history" ? (historyTab === "audit" ? "historyAudit" : "historyOperations") : page}"] [data-search]`);
      if (!search) return;
      event.preventDefault(); search.focus(); search.select();
    }
  });
  // Enter inside a drawer must not implicitly submit a half-edited change.
  // Selects and explicit submit buttons keep their normal keyboard behaviour.
  $$(".drawer form").forEach(form => form.addEventListener("keydown", event => {
    if (event.key === "Enter" && event.target.tagName === "INPUT" && event.target.type !== "submit") event.preventDefault();
  }));

  // ---- Static provisioning template registry ------------------------------
  // The server embeds only the approved registration-template fields. The
  // browser never invents a template, a class, a backend prefix or a capacity.
  function approvedStorageTemplates(value) {
    if (!Array.isArray(value)) return [];
    return value.filter(template => template && template.name && template.storageClass).map(template => ({
      name: String(template.name || ""),
      kind: logic.templateKind(template),
      storageClass: String(template.storageClass || ""),
      pvCapacity: String(template.pvCapacity || ""),
      pvAccessModes: Array.isArray(template.pvAccessModes) ? template.pvAccessModes.map(String) : [],
      pvcRequest: String(template.pvcRequest || ""),
      pvcAccessModes: Array.isArray(template.pvcAccessModes) ? template.pvcAccessModes.map(String) : [],
      reclaimPolicy: String(template.reclaimPolicy || "Retain"),
      volumeMode: String(template.volumeMode || "Filesystem"),
      nfsServer: String(template.nfsServer || ""),
      nfsRoots: Array.isArray(template.nfsRoots) ? template.nfsRoots.map(String) : [],
      nfsMountOptions: Array.isArray(template.nfsMountOptions) ? template.nfsMountOptions.map(String) : [],
      requireDirectoryUIDGID: template.requireDirectoryUIDGID === true,
      beegfsDriver: String(template.beegfsDriver || ""),
      beegfsPrefix: String(template.beegfsPrefix || ""),
      podDefaultName: String(template.podDefaultName || ""),
      podDefaultMountPathPrefix: String(template.podDefaultMountPathPrefix || "")
    }));
  }
  function storageTemplateByName(name) {
    return storageTemplates.find(template => template.name === String(name || "")) || null;
  }
  function selectedStorageTemplate() {
    const select = $("#storage-registration-template");
    return select ? storageTemplateByName(select.value) : null;
  }
  function present(value) { return String(value ?? "").trim() !== ""; }
  function selectedObservedPV() {
    const name = String($("#bound-pv-selector")?.value || "");
    if (!name) return null;
    return (inventory?.resources || []).find(resource => resource.kind === "PersistentVolume" && resource.name === name) || null;
  }
  function relatedPodDefaults(resource) {
    if (!resource || resource.kind !== "PersistentVolumeClaim") return [];
    const namespace = String(resource.namespace || "");
    const claim = String(resource.name || "");
    return (inventory?.resources || []).filter(candidate => {
      if (candidate.kind !== "PodDefault" || (candidate.namespace || "") !== namespace) return false;
      const claims = String(candidate.details?.claims || "").split(",").map(value => value.trim()).filter(Boolean);
      return claims.includes(claim);
    });
  }
  function storagePodDefaultCollision(name, namespace) {
    const value = String(name || "").trim();
    const scope = String(namespace ?? storageFormValue('#storage-form [name="namespace"]') ?? "");
    if (!value) return null;
    // PodDefaults are namespace-local. A same-named object in another
    // namespace is not a conflict for this claim.
    const observed = (inventory?.resources || []).filter(resource =>
      resource.kind === "PodDefault" && resource.name === value && (resource.namespace || "") === scope);
    return observed[0] || null;
  }
  function fillStorageClassOptions(classes) {
    const select = $("#storage-class");
    if (!select) return;
    const wanted = [...new Set(classes.filter(Boolean))];
    if (!wanted.length) return;
    const current = String(select.value || "");
    select.replaceChildren(new Option("Select an allowed class", ""));
    wanted.forEach(name => select.append(new Option(name, name)));
    if (wanted.includes(current)) select.value = current;
  }
  function fillStorageTemplateSelect(select, templates) {
    if (!select) return;
    const current = String(select.value || "");
    select.replaceChildren(new Option("Select an applicable template", ""));
    templates.forEach(template => select.append(new Option(`${template.name} · ${logic.templateKind(template) === "beegfs" ? "BeeGFS" : logic.templateKind(template).toUpperCase()} · ${template.storageClass}`, template.name)));
    if (templates.some(template => template.name === current)) select.value = current;
  }
  function renderStorageTemplateFacts(template, kind) {
    const facts = $("#storage-template-facts"), list = $("#storage-template-facts-list");
    if (!facts || !list) return;
    if (!template) { facts.hidden = true; list.replaceChildren(); return; }
    const rows = [];
    const add = (label, value) => { if (present(value)) rows.push([label, String(value)]); };
    add("Template", template.name);
    add("Backend", logic.templateKind(template) === "beegfs" ? "BeeGFS" : "NFS");
    add("Storage class", template.storageClass);
    add("PV capacity (read-only)", template.pvCapacity);
    add("PV access modes (read-only)", template.pvAccessModes.join(", "));
    add("Volume mode (read-only)", template.volumeMode);
    add("Reclaim policy (read-only)", template.reclaimPolicy);
    add("NFS server (read-only)", template.nfsServer);
    add("Approved NFS roots (read-only)", template.nfsRoots.join(", "));
    add("NFS mount options (read-only)", template.nfsMountOptions.join(", "));
    add("BeeGFS driver (read-only)", template.beegfsDriver);
    add("BeeGFS fixed prefix (read-only)", template.beegfsPrefix);
    add("PVC request (read-only)", template.pvcRequest);
    add("PVC access modes (read-only)", template.pvcAccessModes.join(", "));
    list.replaceChildren();
    rows.forEach(([label, value]) => list.append(el("dt", label), el("dd", value)));
    facts.hidden = false;
  }
  function storageTemplateOptionsFor(kind) {
    if (!logic.storageTemplateKinds.has(kind)) return [];
    return logic.templatesForKind(storageTemplates, kind);
  }
  // A dependent field must never travel with another action or template. A
  // new action clears the PV, path, UID/GID and PodDefault values; changing
  // only the template keeps the typed name/reason but clears the target.
  function clearStorageTargets() {
    const bound = $("#bound-pv-selector"); if (bound) { bound.replaceChildren(new Option("Select an Available PV", "")); bound.value = ""; }
    ["#storage-nfs-path", "#storage-beegfs-path", "#storage-directory-uid", "#storage-directory-gid"].forEach(selector => { const input = $(selector); if (input) input.value = ""; });
    const enable = $("#storage-pod-default-enable"); if (enable) enable.checked = false;
    const fields = $("#storage-pod-default-fields"); if (fields) fields.hidden = true;
    const name = $("#storage-pod-default-name"); if (name) name.value = "";
    const mount = $("#storage-pod-default-mount-path"); if (mount) mount.value = "";
    const overwrite = $("#storage-pod-default-overwrite"); if (overwrite) overwrite.checked = false;
    storagePodDefaultFeedback();
  }
  function storagePodDefaultFeedback() {
    const warning = $("#storage-pod-default-warning");
    const observedHint = $("#storage-pod-default-observed");
    const enable = $("#storage-pod-default-enable");
    if (!warning || !observedHint || !enable) return;
    const active = !enable.disabled && enable.checked;
    const collision = storagePodDefaultCollision($("#storage-pod-default-name")?.value || "", storageFormValue('#storage-form [name="namespace"]'));
    const visible = active && !!collision;
    warning.hidden = !visible;
    if (visible) {
      warning.textContent = `An observed PodDefault named ${collision.name}${collision.namespace ? " in namespace " + collision.namespace : ""} already exists. Creating this plan overwrites its dedicated spec only after the server revalidates its UID/resourceVersion. Its current selector and all other injected PVC mounts and settings are replaced. External management such as Helm or GitOps may reconcile it again; this console does not claim exclusive ownership. Secret-bearing values are never shown here.`;
    }
    const hint = active && !collision
      ? "No PodDefault with this name is in the current observation. The server still revalidates the exact name and reports a collision at plan time."
      : "";
    observedHint.hidden = !hint;
    observedHint.textContent = hint;
  }
  function syncStoragePodDefaultSuggestion() {
    const enable = $("#storage-pod-default-enable");
    const container = $("#storage-pod-default");
    if (!enable || !container || container.hidden || !enable.checked) return;
    const template = selectedStorageTemplate();
    const claim = storageNameValue();
    const suggestion = logic.podDefaultSuggestion(template, claim);
    const name = $("#storage-pod-default-name"), mount = $("#storage-pod-default-mount-path");
    if (name && !name.value) name.value = suggestion.name;
    if (mount && !mount.value) mount.value = suggestion.mountPath;
  }
  function syncStoragePodDefaultFields() {
    const enable = $("#storage-pod-default-enable");
    const container = $("#storage-pod-default");
    const fields = $("#storage-pod-default-fields");
    if (!enable || !container || !fields) return;
    const kind = $("#storage-kind")?.value || "";
    const template = selectedStorageTemplate();
    const offered = podDefaultsFeature && (kind === "pvc.create" || kind === "pvc.create.bound");
    container.hidden = !offered;
    enable.disabled = !offered;
    if (!offered) enable.checked = false;
    fields.hidden = !offered || !enable.checked;
    $$("input", fields).forEach(input => { input.disabled = !offered || !enable.checked; });
    if (offered && enable.checked) syncStoragePodDefaultSuggestion();
    const overwrite = $("#storage-pod-default-overwrite");
    if (overwrite) overwrite.disabled = !offered || !enable.checked;
    storagePodDefaultFeedback();
  }

  // ---- Forms and plan workflow --------------------------------------------
  function selectors(applyFilter = false) {
    const resources = inventory?.resources || [];
    $$("[data-namespaces]").forEach(select => {
      fillSelect(select, scopeNamespaces(), "Select a namespace");
      if (namespaceContext && (applyFilter || !select.value)) select.value = namespaceContext;
    });
    if (applyFilter) { $("#mount-rows").replaceChildren(); $("#replace-mounts").checked = false; }
    // The Profile selector is refreshed from the observation, but the current
    // selection and every prefilled value are left untouched: an inventory
    // refresh never edits what the administrator typed or selected.
    const updateSelect = $("#profile-update-name");
    if (updateSelect) {
      const names = observedProfiles().map(resource => resource.name).sort(logic.naturalCompare);
      const current = String(updateSelect.value || "");
      updateSelect.replaceChildren(new Option("Select a profile", ""));
      for (const name of names) updateSelect.append(new Option(name, name));
      if (names.includes(current)) updateSelect.value = current;
    }
    $("#claim-names").replaceChildren(...resources.filter(resource => resource.kind === "PersistentVolumeClaim" && (!namespaceContext || resource.namespace === namespaceContext)).map(resource => new Option(resource.name, resource.name)));
    fillSelect($("#pv-selector"), resources.filter(resource => resource.kind === "PersistentVolume" && resource.state === "Available" && !resource.details?.claim && resource.details?.reclaimPolicy === "Retain").map(resource => resource.name), "Select an available Retain PV");
    // The registration-template and compatible-PV selectors are refreshed from
    // the same observation. A refresh never discards a still-valid selection.
    storageFields();
    notebookOptions();
  }
  function notebookOptions() {
    const namespace = $('#notebook-form [name="namespace"]').value;
    fillSelect($("#notebook-selector"), (inventory?.resources || []).filter(resource => resource.kind === "Notebook" && resource.namespace === namespace).map(resource => resource.name), "Select a notebook");
    containerOptions();
  }
  function containerOptions() {
    const namespace = $('#notebook-form [name="namespace"]').value, name = $("#notebook-selector").value;
    const notebook = (inventory?.resources || []).find(resource => resource.kind === "Notebook" && resource.namespace === namespace && resource.name === name);
    const names = logic.containerNames(notebook);
    fillSelect($("#container-selector"), names, "Select the exact named container");
    if (names.length === 1) $("#container-selector").value = names[0];
  }
  function storageFields() {
    const kind = $("#storage-kind").value;
    $$('[data-storage]').forEach(label => { const visible = label.dataset.storage.split(" ").includes(kind); label.hidden = !visible; $$("input,select", label).forEach(input => { input.disabled = !visible; }); });
    const legacy = $("#storage-template").selectedOptions?.[0];
    const registrationSelect = $("#storage-registration-template");
    const eligible = storageTemplateOptionsFor(kind);
    if (registrationSelect) {
      const previous = String(registrationSelect.value || "");
      fillStorageTemplateSelect(registrationSelect, eligible);
      if (!eligible.some(template => template.name === previous)) registrationSelect.value = "";
      registrationSelect.required = logic.storageTemplateKinds.has(kind);
    }
    const currentTemplate = selectedStorageTemplate();
    // The approved class list is narrowed to classes with an eligible
    // registration template for the active registration action. Legacy flows
    // keep their complete allowed list.
    if (allowedStorageClasses === null) allowedStorageClasses = $$("#storage-class option").map(option => String(option.value ?? "")).filter(Boolean);
    fillStorageClassOptions(logic.storageTemplateKinds.has(kind) ? eligible.map(template => template.storageClass) : allowedStorageClasses);
    const templateClass = currentTemplate?.storageClass || legacy?.dataset.class || "";
    if (templateClass) $("#storage-class").value = templateClass;
    // A registration template fixes the class. The select remains visible for
    // review but is not an editable override; the server would refuse any
    // different class anyway.
    if (logic.storageTemplateKinds.has(kind) && currentTemplate) $("#storage-class").disabled = true;
    $('#storage-form [name="size"]').required = ["pvc.create", "pv.nfs.create", "pv.reserve"].includes(kind);
    $("#pv-selector").required = kind === "pv.reserve";
    $("#pv-selector").disabled = kind !== "pv.reserve";
    $("#storage-template").disabled = kind !== "pv.nfs.create";
    $("#bound-pv-selector").required = kind === "pvc.create.bound";
    $("#storage-class").required = ["pvc.create", "pvc.create.bound", "pv.create.static", "pv.reserve"].includes(kind);
    const nfs = !!currentTemplate && logic.templateKind(currentTemplate) === "nfs";
    const beegfs = !!currentTemplate && logic.templateKind(currentTemplate) === "beegfs";
    // A new PV enters its backend path. A bound PVC never accepts a path: the
    // selected PV already carries its fixed backend path, so the fields stay
    // disabled and are excluded from the payload.
    const staticPath = kind === "pv.create.static";
    $("#storage-nfs-path").required = staticPath && nfs;
    $("#storage-nfs-path").disabled = !(staticPath && nfs);
    $("#storage-beegfs-path").required = staticPath && beegfs;
    $("#storage-beegfs-path").disabled = !(staticPath && beegfs);
    // Only the path input for the selected backend kind is rendered. The
    // generic data-storage visibility above cannot know the template kind.
    const nfsField = $("#storage-nfs-path-field"); if (nfsField) nfsField.hidden = !(staticPath && nfs);
    const nfsHint = $("#storage-nfs-path-hint"); if (nfsHint) nfsHint.hidden = !(staticPath && nfs);
    const beegfsField = $("#storage-beegfs-path-field"); if (beegfsField) beegfsField.hidden = !(staticPath && beegfs);
    const beegfsHint = $("#storage-beegfs-path-hint"); if (beegfsHint) beegfsHint.hidden = !(staticPath && beegfs);
    ["#storage-directory-uid", "#storage-directory-gid"].forEach(selector => { const input = $(selector); if (input) { input.required = kind === "pvc.create.bound" && nfs; input.disabled = !(kind === "pvc.create.bound" && nfs); } });
    renderStorageTemplateFacts(currentTemplate, kind);
    syncStorageBoundPVs(kind, currentTemplate);
    syncStoragePodDefaultFields();
    return { kind, template: currentTemplate, legacy };
  }
  // The selected PV is narrowed by the same observable facts the server
  // revalidates. Missing observation metadata does not hide a PV: the server
  // performs the authoritative compatibility check.
  function syncStorageBoundPVs(kind, template) {
    const select = $("#bound-pv-selector");
    if (!select) return;
    if (kind !== "pvc.create.bound") return;
    const classValue = String($("#storage-class")?.value || "");
    const compatible = logic.compatiblePVs(inventory?.resources || [], template, classValue);
    const current = String(select.value || "");
    select.replaceChildren(new Option(compatible.length ? "Select an Available PV" : "No compatible Available PV", ""));
    compatible.forEach(resource => {
      const details = resource.details || {};
      const suffix = [details.storageClass, details.capacity].filter(Boolean).join(" · ");
      select.append(new Option(suffix ? `${resource.name} · ${suffix}` : resource.name, resource.name));
    });
    if (compatible.some(resource => resource.name === current)) select.value = current;
    else select.value = "";
  }
  function storageTemplateChanged() {
    clearStorageTargets();
    storageFields();
  }

  // ---- Template-driven plan binding ---------------------------------------
  // The registration actions reuse the typed plan workflow. These helpers
  // collect only the fields the action defines, resolve the selected observed
  // PV identity at submission time and perform browser-side early validation.
  // The server remains authoritative for every rule.
  function storageFormValue(selector) { return String($(selector)?.value ?? ""); }
  // Two distinct controls exist for the new claim name and the new PV name.
  // Only the active action owns one of them; never read the hidden duplicate.
  function storageNameInput() {
    const kind = storageFormValue("#storage-kind");
    const id = kind === "pv.create.static" ? "#storage-pv-name" : "#storage-claim-name";
    return $(id) || $('#storage-form [name="name"]:not([disabled])') || $('#storage-form [name="name"]');
  }
  function storageNameValue() { return String(storageNameInput()?.value ?? ""); }
  function storageBoundSelection() {
    const template = selectedStorageTemplate();
    const pv = selectedObservedPV();
    return {
      kind: storageFormValue("#storage-kind"),
      name: storageNameValue(),
      namespace: storageFormValue('#storage-form [name="namespace"]'),
      class: storageFormValue("#storage-class"),
      template,
      pv,
      resourceVersion: String(pv?.resourceVersion || pv?.details?.resourceVersion || ""),
      nfsPath: storageFormValue("#storage-nfs-path"),
      beeGFSPath: storageFormValue("#storage-beegfs-path"),
      directoryUID: storageFormValue("#storage-directory-uid"),
      directoryGID: storageFormValue("#storage-directory-gid")
    };
  }
  function storagePodDefaultRequest(bound) {
    if (!podDefaultsFeature) return null;
    if (bound.kind !== "pvc.create" && bound.kind !== "pvc.create.bound") return null;
    const enable = $("#storage-pod-default-enable");
    if (!enable?.checked) return null;
    return {
      name: storageFormValue("#storage-pod-default-name").trim(),
      mountPath: storageFormValue("#storage-pod-default-mount-path"),
      overwrite: !!$("#storage-pod-default-overwrite")?.checked,
      collision: !!storagePodDefaultCollision(storageFormValue("#storage-pod-default-name"), bound.namespace)
    };
  }
  function storageCreateError() {
    const bound = storageBoundSelection();
    if (logic.storageTemplateKinds.has(bound.kind)) {
      if (!bound.template) return "Select an approved registration template.";
      if (!storageTemplateOptionsFor(bound.kind).some(template => template.name === bound.template.name)) {
        return "The selected template is not eligible for this action. Select a template again.";
      }
      if (bound.class && bound.class !== bound.template.storageClass) {
        return `The selected template requires StorageClass ${bound.template.storageClass}. Select that class or change the template.`;
      }
      if (!bound.class) return "Select the approved StorageClass for this template.";
    }
    if (bound.kind === "pvc.create.bound") {
      if (!bound.pv) return "Select the observed Available PersistentVolume to bind.";
      if (!logic.compatiblePV(bound.pv, bound.template, bound.class)) {
        return "The selected PersistentVolume is not an Available, unclaimed volume compatible with this template and class. Refresh the observation and select it again.";
      }
      if (!String(bound.pv.uid || "")) return "The selected PersistentVolume has no stable observed UID. Refresh inventory and select it again.";
      if (!bound.resourceVersion) return "The selected PersistentVolume has no observed resourceVersion. Refresh inventory and select it again.";
    }
    const error = logic.storageActionError(bound.kind, {
      name: bound.name,
      namespace: bound.namespace,
      pvName: bound.pv?.name || "",
      template: bound.template,
      // Paths are only entered for a new PV. A bound claim must never send a
      // path field: the selected volume already carries its fixed backend path.
      nfsPath: bound.kind === "pv.create.static" ? bound.nfsPath : "",
      beeGFSPath: bound.kind === "pv.create.static" ? bound.beeGFSPath : "",
      directoryUID: bound.directoryUID,
      directoryGID: bound.directoryGID,
      podDefault: storagePodDefaultRequest(bound)
    });
    if (error) return error;
    if (bound.kind === "pvc.create.bound" && bound.template && logic.templateKind(bound.template) === "nfs") {
      if (!bound.template.requireDirectoryUIDGID) return "The selected NFS template is not configured for the required directory UID/GID.";
    }
    return "";
  }
  // The exact action fields are assembled here, never from unchecked hidden
  // controls or controls of another action. Existing dynamic/legacy actions
  // keep their existing fields and never acquire a path from a registration
  // template.
  function storageCreateContext() {
    const bound = storageBoundSelection();
    const podDefault = storagePodDefaultRequest(bound);
    return {
      kind: bound.kind, name: bound.name, namespace: bound.namespace,
      template: bound.template?.name || "", class: bound.class,
      pv: bound.pv?.name || "", pvUID: String(bound.pv?.uid || ""),
      nfsPath: bound.nfsPath, beeGFSPath: bound.beeGFSPath,
      directoryUID: bound.directoryUID, directoryGID: bound.directoryGID,
      podDefault: podDefault ? [podDefault.name, podDefault.mountPath, podDefault.overwrite] : null
    };
  }
  function storageCreateContextCurrent(context) {
    if (!context) return true;
    const current = storageCreateContext();
    return current.kind === context.kind && current.name === context.name &&
      current.namespace === context.namespace && current.template === context.template &&
      current.class === context.class && current.pv === context.pv && current.pvUID === context.pvUID &&
      current.nfsPath === context.nfsPath && current.beeGFSPath === context.beeGFSPath &&
      current.directoryUID === context.directoryUID && current.directoryGID === context.directoryGID &&
      JSON.stringify(current.podDefault) === JSON.stringify(context.podDefault);
  }
  function enforceStorageAction(action) {
    if (!action) return action;
    if (action.kind === "pvc.delete") {
      if ($("#storage-delete-reclaim-ack")?.checked) action.confirmPVReclaim = true;
      else delete action.confirmPVReclaim;
      if ($("#storage-delete-pod-default-ack")?.checked) action.confirmPodDefaultCleanup = true;
      else delete action.confirmPodDefaultCleanup;
      return action;
    }
    if (action.kind === "pvc.create.dynamic" || action.kind === "pvc.create") {
      const podDefault = action.podDefault || storagePodDefaultRequest(storageBoundSelection());
      const keep = ["kind", "namespace", "name", "reason", "storageClass", "size", "accessMode"];
      for (const key of Object.keys(action)) if (!keep.includes(key)) delete action[key];
      if (podDefault) action.podDefault = { name: podDefault.name, mountPath: podDefault.mountPath, overwrite: podDefault.overwrite };
      return action;
    }
    if (!logic.storageTemplateKinds.has(action.kind)) return action;
    const bound = storageBoundSelection();
    const podDefault = action.podDefault || storagePodDefaultRequest(bound);
    const keep = ["kind", "reason"];
    for (const key of Object.keys(action)) {
      if (!keep.includes(key)) delete action[key];
    }
    action.name = bound.name.trim();
    action.reason = String(action.reason || "").trim();
    action.template = bound.template?.name || "";
    action.storageClass = bound.class;
    if (action.kind === "pv.create.static") {
      if (bound.template && logic.templateKind(bound.template) === "nfs") action.nfsPath = bound.nfsPath;
      if (bound.template && logic.templateKind(bound.template) === "beegfs") action.beeGFSPath = bound.beeGFSPath;
    } else if (action.kind === "pvc.create.bound") {
      action.namespace = bound.namespace;
      action.pvName = bound.pv?.name || "";
      action.targetUID = String(bound.pv?.uid || "");
      action.targetResourceVersion = bound.resourceVersion;
      if (bound.template && logic.templateKind(bound.template) === "nfs") {
        action.directoryUID = bound.directoryUID.trim();
        action.directoryGID = bound.directoryGID.trim();
      }
      if (podDefault) action.podDefault = { name: podDefault.name, mountPath: podDefault.mountPath, overwrite: podDefault.overwrite };
    }
    return action;
  }
  function notebookFields() {
    const edit = $("#notebook-kind").value === "notebook.edit"; $("#notebook-edit").hidden = !edit;
    $$("input,select,button", $("#notebook-edit")).forEach(input => { input.disabled = !edit; });
    $("#container-selector").required = edit;
    mountFields();
  }
  // ---- Notebook management target binding ---------------------------------
  function observedNotebook(namespace, name) {
    return (inventory?.resources || []).find(resource =>
      resource.kind === "Notebook" && (resource.namespace || "") === String(namespace || "") && (resource.name || "") === String(name || "")) || null;
  }
  function notebookTargetWarning(text) {
    const warning = $("#notebook-target-warning");
    if (!warning) return;
    warning.textContent = String(text || "");
    warning.hidden = !text;
  }
  // Selecting a different observed Notebook replaces the bound target. An
  // unknown target is refused instead of being submitted against the wrong
  // Notebook.
  function bindNotebookSelection() {
    const namespace = String($('#notebook-form [name="namespace"]')?.value || "");
    const name = String($("#notebook-selector")?.value || "");
    if (!namespace || !name) { notebookTarget = null; notebookTargetWarning(""); return; }
    const found = observedNotebook(namespace, name);
    notebookTarget = found ? { namespace, name, uid: found.uid || "" } : null;
    notebookTargetWarning(found ? "" : `The Notebook ${namespace}/${name} is not part of the current observation. Refresh inventory and select it again.`);
  }
  // A previous target's edit values are never carried into another Notebook.
  // Blank resource and image fields keep their existing "preserve" meaning; a
  // changed target always drops old mounts, the replace flag and restart.
  // A container change preserves the chosen container itself.
  function clearNotebookEditState(options = {}) {
    const preserveContainer = !!(options && options.preserveContainer === true);
    const edit = $("#notebook-edit");
    if (!edit) return;
    const container = $("#container-selector");
    const keptContainer = preserveContainer && container ? container.value : "";
    $$("input,select", edit).forEach(input => {
      if (input === container && preserveContainer) return;
      if (input.type === "checkbox") { input.checked = false; return; }
      input.value = "";
    });
    if (preserveContainer && container) container.value = keptContainer;
    const rows = $("#mount-rows"); if (rows) rows.replaceChildren();
    const replace = $("#replace-mounts"); if (replace) replace.checked = false;
    mountFields();
  }
  function notebookReasonInput() { return $('#notebook-form [name="reason"]'); }
  // A new management context never inherits the previous target's edit values,
  // reason or a client-side error. The selected action is not changed.
  function resetNotebookManagementContext() {
    clearNotebookEditState();
    const reason = notebookReasonInput();
    if (reason) reason.value = "";
    clearFormError($("#notebook-form"));
    notebookTargetWarning("");
  }
  function clearNotebookTargetChangeState() {
    clearNotebookEditState();
    clearFormError($("#notebook-form"));
  }
  function notebookManagementAllowed() { return writes && notebookWrites; }
  // Prefill the existing Notebook management form from one exact observed
  // resource: its namespace, its Notebook name and its observed container.
  // The lifecycle/resource/mount helpers of the form are reused as they are.
  function prefillNotebookTarget(resource) {
    const namespace = String(resource?.namespace || "");
    const name = String(resource?.name || "");
    if (!namespace || !name) return false;
    const namespaceSelect = $('#notebook-form [name="namespace"]');
    const notebookSelect = $("#notebook-selector");
    if (!namespaceSelect || !notebookSelect) return false;
    const namespaces = scopeNamespaces();
    if (!namespaces.includes(namespace)) namespaces.push(namespace);
    fillSelect(namespaceSelect, namespaces, "Select a namespace");
    namespaceSelect.value = namespace;
    const names = (inventory?.resources || []).filter(candidate => candidate.kind === "Notebook" && (candidate.namespace || "") === namespace).map(candidate => candidate.name);
    fillSelect(notebookSelect, names, "Select a notebook");
    if (!names.includes(name)) return false;
    notebookSelect.value = name;
    clearNotebookEditState();
    containerOptions();
    const containers = logic.containerNames(resource);
    const containerSelect = $("#container-selector");
    if (containerSelect) containerSelect.value = containers.length === 1 ? containers[0] : "";
    return true;
  }
  function openNotebookManagement(resource) {
    // The visible control is already disabled by policy, but the opener stays
    // defensive so no code path can open a management form it cannot submit.
    if (!notebookManagementAllowed()) return false;
    const found = observedNotebook(resource?.namespace, resource?.name);
    if (!found || found.kind !== "Notebook") {
      notify("error", `The Notebook ${resource?.namespace || ""}/${resource?.name || ""} is not observed any more. Refresh inventory and open it again.`, "action", { persist: false });
      return false;
    }
    // This is a new operation context: an old reason, error and edit values
    // from another Notebook are never reused. The selected action is kept.
    resetNotebookManagementContext();
    if (!prefillNotebookTarget(found)) {
      notify("error", "The selected Notebook is not part of the current observation. Refresh inventory and open it again.", "action", { persist: false });
      return false;
    }
    notebookTarget = { namespace: found.namespace || "", name: found.name || "", uid: found.uid || "" };
    notebookManagementFromDetails = true;
    notebookReturnKey = resourceKey(found);
    notebookFields();
    // The exact visible row control of this Notebook is the preferred return
    // target. The page heading is the fallback when the row is gone.
    const rowControl = resourceDetailsOpener(notebookReturnKey);
    openDrawer($("#notebook-drawer"), rowControl || undefined);
    return true;
  }
  // The page-level Manage notebook control is deliberately unbound on open:
  // it starts from a clean management context, then binds the currently
  // selected observed Notebook so a stale UID can never be submitted.
  function openNotebookPageManagement(opener) {
    if (!notebookManagementAllowed()) return false;
    resetNotebookManagementContext();
    containerOptions();
    notebookFields();
    bindNotebookSelection();
    openDrawer($("#notebook-drawer"), opener);
    return true;
  }
  function syncNotebookManagement() {
    if (!notebookTarget) { notebookTargetWarning(""); return ""; }
    const found = observedNotebook(notebookTarget.namespace, notebookTarget.name);
    let message = "";
    if (!found) message = `The Notebook ${notebookTarget.namespace}/${notebookTarget.name} is no longer observed. Refresh inventory and open it again.`;
    else if (notebookTarget.uid && found.uid && found.uid !== notebookTarget.uid) message = `The Notebook ${notebookTarget.namespace}/${notebookTarget.name} was replaced by a new observation. Reopen it before submitting a change.`;
    notebookTargetWarning(message);
    return message;
  }
  function notebookTargetError() {
    if (!notebookTarget) return "";
    const found = observedNotebook(notebookTarget.namespace, notebookTarget.name);
    if (!found) return `The Notebook ${notebookTarget.namespace}/${notebookTarget.name} is no longer observed. Refresh inventory and select it again; no change was sent.`;
    if (notebookTarget.uid && found.uid && notebookTarget.uid !== found.uid) {
      return `The Notebook ${notebookTarget.namespace}/${notebookTarget.name} was replaced by a new observation. Reopen it to review the current configuration; no change was sent.`;
    }
    const namespace = String($('#notebook-form [name="namespace"]')?.value || "");
    const name = String($("#notebook-selector")?.value || "");
    if (namespace !== notebookTarget.namespace || name !== notebookTarget.name) {
      const selected = observedNotebook(namespace, name);
      if (!selected) return "Select an observed Notebook; the current selection is not part of the latest observation.";
      notebookTarget = { namespace, name, uid: selected.uid || "" };
      notebookTargetWarning("");
    }
    return "";
  }
  // ---- Notebook policy presentation ---------------------------------------
  // The page-level and details actions share one reason. It names the exact
  // configuration gates rather than hiding the action without explanation.
  function notebookManagementReason() {
    const missing = [];
    if (!writes) missing.push("features.writesEnabled is false");
    if (!notebookWrites) missing.push("notebooks.controllerContractConfirmed is false");
    if (!missing.length) return "";
    return `Notebook management is disabled by server policy because ${missing.join(" and ")}. Enable both only after the installed Notebook controller contract is actually verified; this console never enables them automatically.`;
  }
  function syncNotebookPagePolicy() {
    const reason = notebookManagementReason();
    const button = $("#notebook-page-manage");
    if (button) {
      button.disabled = !!reason;
      button.title = reason || "Open Notebook management for an observed Notebook.";
      button.setAttribute("aria-disabled", reason ? "true" : "false");
    }
    const hint = $("#notebook-page-policy");
    if (hint) {
      hint.textContent = reason;
      hint.hidden = !reason;
    }
  }

  // ---- Storage YAML edit and deletion -------------------------------------
  // Both actions are only ever prepared through the existing typed plan
  // workflow. The browser never sends a direct Kubernetes mutation and never
  // stores the resource projection in local or session storage.
  function storageFeatureReason(mode) {
    if (!writes) return "Changes are disabled by server policy (features.writesEnabled is false).";
    if (mode === "edit" && !storageEditingFeature) return "Storage YAML editing is disabled by server policy (features.storageEditing is false).";
    if (mode === "delete" && !storageDeletionFeature) return "Storage deletion is disabled by server policy (features.storageDeletion is false).";
    return "";
  }
  function storageLabel(operation) {
    return `${operation.kind} ${operation.namespace ? operation.namespace + "/" : ""}${operation.name}`;
  }
  function storageDrawer(mode) { return $(`#storage-${mode}-drawer`); }
  function storageDocumentQuery(operation) {
    return "storage-document?kind=" + encodeURIComponent(operation.kind) +
      "&name=" + encodeURIComponent(operation.name) +
      (operation.namespace ? "&namespace=" + encodeURIComponent(operation.namespace) : "");
  }
  function storagePolicyHint(resource, mode) {
    if (mode === "edit" && resource.kind === "PersistentVolumeClaim") {
      return "A PersistentVolumeClaim size change may only increase the request, and only when features.pvcExpansion is enabled. Shrink and no-op changes are refused by the server. metadata.labels follows the server projection warnings; this console never infers a label change.";
    }
    if (mode === "edit") {
      return "Only an Available, unclaimed and unreserved PersistentVolume in an approved StorageClass can be edited. The server revalidates the live object and its class. metadata.labels follows the server projection warnings; this console never infers a label change.";
    }
    if (resource.kind === "PersistentVolumeClaim") {
      return "Deletion is allowed only when no observed Pod or Notebook references this claim. The server revalidates every observed reference.";
    }
    return "Deletion is allowed only for an Available, unclaimed and unreserved PersistentVolume in an approved StorageClass. The server revalidates the live object.";
  }
  function observedReclaimPolicy(resource) {
    if (resource.kind === "PersistentVolume") return String(resource.details?.reclaimPolicy || "");
    if (resource.kind !== "PersistentVolumeClaim") return "";
    const volume = String(resource.details?.volume || "");
    const claim = `${resource.namespace || ""}/${resource.name || ""}`;
    const pv = (inventory?.resources || []).find(candidate => candidate.kind === "PersistentVolume" &&
      (candidate.name === volume || String(candidate.details?.claim || "") === claim));
    return String(pv?.details?.reclaimPolicy || resource.details?.reclaimPolicy || "");
  }
  function storageDeleteWarning(resource) {
    const policy = observedReclaimPolicy(resource);
    let text = `Deleting the ${storageLabel(resource)} object is not a filesystem wipe.`;
    if (policy) text += ` The observed reclaim policy is ${policy}.`;
    if (policy === "Delete") text += " A Delete policy can let the storage controller remove the backing volume, which can be irreversible.";
    else if (policy === "Retain") text += " A Retain policy may leave the backing data behind for a separate operator decision; it is not proof that the data is gone.";
    else text += " The reclaim policy may remove or retain the backing data; this console never wipes backend data itself.";
    text += " The console never removes a finalizer and never forces cleanup. If the result is needs-review, do not retry: inspect History and ask the operator.";
    return text;
  }
  function syncStorageSubmitButtons() {
    for (const [mode, feature] of [["edit", storageEditingFeature], ["delete", storageDeletionFeature]]) {
      const form = $(`#storage-${mode}-form`);
      const button = form ? $('button[type="submit"]', form) : null;
      if (!button) continue;
      const operation = storageOperation;
      button.disabled = !feature || !operation || operation.mode !== mode || !operation.doc || !!operation.planning;
    }
  }
  function storageDocumentFailure(message) {
    const operation = storageOperation;
    if (!operation) return;
    operation.doc = null;
    const drawer = storageDrawer(operation.mode);
    if (drawer && !drawer.hidden) {
      const loading = $(`#storage-${operation.mode}-loading`);
      if (loading) loading.hidden = true;
      const form = $(`#storage-${operation.mode}-form`);
      if (form) showFormError(form, message);
    }
    syncStorageSubmitButtons();
  }
  // A document read is used only when it still belongs to the current drawer
  // epoch and to the exact selected kind, namespace, name and UID. A late
  // answer for a closed or replaced target is discarded, never rendered.
  async function loadStorageDocument(epoch) {
    const operation = storageOperation;
    if (!operation || operation.epoch !== epoch) return;
    try {
      const doc = await api(storageDocumentQuery(operation));
      if (!storageOperation || storageOperation.epoch !== epoch || storageOperation.mode !== operation.mode) return;
      const found = observedResource(operation);
      if (!found) {
        storageDocumentFailure(`The ${storageLabel(operation)} is no longer observed. Close this change and open the current resource again.`);
        return;
      }
      if (String(found.uid || "") !== operation.uid) {
        storageDocumentFailure(!found.uid
          ? `The ${storageLabel(operation)} no longer has a stable observed UID. Close this change and open the current resource again.`
          : `The ${storageLabel(operation)} was replaced by a new observation. Close this change and open the current resource again.`);
        return;
      }
      const kind = String(doc?.kind || ""), namespace = String(doc?.namespace || ""), name = String(doc?.name || "");
      if (kind !== operation.kind || namespace !== operation.namespace || name !== operation.name) {
        storageDocumentFailure("The server returned a document for a different storage resource. Close this change and open the selected resource again.");
        return;
      }
      if (!doc?.uid || String(doc.uid) !== operation.uid) {
        storageDocumentFailure("The selected storage resource UID changed while the document was loading. Refresh inventory and open the current resource again.");
        return;
      }
      if (!doc.resourceVersion) {
        storageDocumentFailure("The server returned no resource version for this storage resource. Refresh and open it again.");
        return;
      }
      if (typeof doc.yaml !== "string" || !doc.yaml.trim()) {
        storageDocumentFailure("The server returned no restricted YAML projection for this storage resource.");
        return;
      }
      operation.doc = doc;
      operation.warnings = Array.isArray(doc.warnings) ? doc.warnings.map(String) : [];
      const targetUID = $("#storage-" + operation.mode + "-target-uid");
      const targetRV = $("#storage-" + operation.mode + "-target-rv");
      if (targetUID) targetUID.value = String(doc.uid);
      if (targetRV) targetRV.value = String(doc.resourceVersion);
      const targetCaption = $(`#storage-${operation.mode}-target`);
      if (targetCaption) targetCaption.textContent = `Target: ${storageLabel(operation)} · UID ${doc.uid} · resourceVersion ${doc.resourceVersion}. The document was fetched from the server for this exact target; the plan is unavailable until this read has settled.`;
      const loading = $(`#storage-${operation.mode}-loading`);
      if (loading) loading.hidden = true;
      if (operation.mode === "edit") {
        const textarea = $("#storage-edit-yaml");
        if (textarea) {
          // The projection is a string value, never markup. An arbitrary
          // server projection can never become executable DOM here.
          textarea.value = String(doc.yaml);
          textarea.disabled = false;
        }
        const warnings = $("#storage-edit-warnings");
        if (warnings) {
          warnings.textContent = operation.warnings.join(" ");
          warnings.hidden = !operation.warnings.length;
        }
      }
      clearFormError($(`#storage-${operation.mode}-form`));
      syncStorageSubmitButtons();
    } catch (error) {
      if (!storageOperation || storageOperation.epoch !== epoch) return;
      storageDocumentFailure(error?.message || "The storage document could not be loaded. Refresh inventory and try again.");
    }
  }
  function openStorageAction(resource, mode, opener) {
    if (!storageActionKinds.has(resource?.kind || "")) return false;
    const reason = storageFeatureReason(mode);
    if (reason) return false;
    const found = observedResource({ kind: resource.kind || "", namespace: resource.namespace || "", name: resource.name || "" });
    if (!found) {
      notify("error", `The observed ${resource.kind} ${resource.name || ""} is no longer present. Refresh inventory and open it again.`, "action", { persist: false });
      return false;
    }
    if (!found.uid) {
      notify("error", "This storage resource has no stable observed UID. Refresh inventory and open it again.", "action", { persist: false });
      return false;
    }
    const drawer = storageDrawer(mode);
    const form = $(`#storage-${mode}-form`);
    if (!drawer || !form) return false;
    const actionKind = logic.storageActionKind(found.kind, mode);
    const kind = $("#storage-" + mode + "-kind"); if (kind) kind.value = actionKind;
    const name = $("#storage-" + mode + "-name"); if (name) name.value = found.name || "";
    const namespace = $("#storage-" + mode + "-namespace"); if (namespace) namespace.value = found.namespace || "";
    const targetUID = $("#storage-" + mode + "-target-uid"); if (targetUID) targetUID.value = found.uid || "";
    const targetRV = $("#storage-" + mode + "-target-rv"); if (targetRV) targetRV.value = "";
    const target = $("#storage-" + mode + "-target");
    if (target) target.textContent = `Target: ${storageLabel(found)} · selected observation UID ${found.uid} · the current server projection is fetched before any plan can be prepared.`;
    const loading = $(`#storage-${mode}-loading`); if (loading) loading.hidden = false;
    const reasonInput = $("#storage-" + mode + "-reason"); if (reasonInput) reasonInput.value = "";
    clearFormError(form);
    if (mode === "edit") {
      const textarea = $("#storage-edit-yaml"); if (textarea) { textarea.value = ""; textarea.disabled = true; }
      const warnings = $("#storage-edit-warnings"); if (warnings) { warnings.textContent = ""; warnings.hidden = true; }
      const policy = $("#storage-edit-policy"); if (policy) policy.textContent = storagePolicyHint(found, mode);
    } else {
      const title = $("#storage-delete-title"); if (title) title.textContent = `Delete ${found.name}`;
      if (!$("#storage-delete-reclaim-option")) {
        const option = $("#storage-reclaim-option")?.content?.cloneNode(true);
        if (option) form.prepend(option);
      }
      const reclaimOption = $("#storage-delete-reclaim-option"); if (reclaimOption) reclaimOption.hidden = found.kind !== "PersistentVolumeClaim";
      const reclaim = $("#storage-delete-reclaim-ack"); if (reclaim) reclaim.checked = false;
      const confirm = $("#storage-delete-confirm"); if (confirm) confirm.value = "";
      const ack = $("#storage-delete-ack"); if (ack) ack.checked = false;
      const podDefaultAck = $("#storage-delete-pod-default-ack"); if (podDefaultAck) podDefaultAck.checked = false;
      // Related PodDefault cleanup only applies to claims. The server lists
      // the exact reviewed dependencies in the plan; this section states the
      // boundary and requires the separate acknowledgement up front.
      const cleanup = $("#storage-delete-pod-default");
      const cleanupLabel = $("#storage-delete-pod-default-ack-label");
      const cleanupHint = $("#storage-delete-pod-default-hint");
      const claim = found.kind === "PersistentVolumeClaim";
      const related = relatedPodDefaults(found);
      if (cleanup) {
        cleanup.hidden = !claim;
        if (claim && related.length) {
          const shared = related.filter(item => item.details?.shared === "true" || item.details?.externalManaged === "true");
          const names = related.map(item => `${item.namespace}/${item.name}`).join(", ");
          const primary = cleanup.querySelector("p");
          if (primary) primary.textContent = `${related.length} related PodDefault${related.length === 1 ? "" : "s"} observed: ${names}. They are removed before the claim, with their own UID/resourceVersion preconditions. Any shared or externally managed object loses its other injected mounts and settings from future matching Pods; existing Pod mounts are never removed. The reviewed cleanup is bounded to 32 objects.`;
          const extra = cleanup.querySelectorAll("p")[1];
          if (extra) extra.hidden = !shared.length;
        }
      }
      if (cleanupLabel) cleanupLabel.hidden = !claim;
      if (cleanupHint) cleanupHint.hidden = !claim;
      if (podDefaultAck) podDefaultAck.disabled = !claim || !writes;
      if (podDefaultAck) podDefaultAck.checked = false;
      const warning = $("#storage-delete-warning"); if (warning) { warning.textContent = storageDeleteWarning(found); warning.hidden = false; }
    }
    // Opening a new drawer is a new operation context. openDrawer may close a
    // previous storage drawer and clear its operation; the new binding is
    // installed afterwards, with a fresh epoch that discards every old read.
    openDrawer(drawer, opener);
    storageEpoch++;
    storageOperation = {
      mode, kind: found.kind, namespace: found.namespace || "", name: found.name || "",
      uid: String(found.uid || ""), returnKey: resourceKey(found), epoch: storageEpoch, doc: null, warnings: [], planning: false
    };
    syncStorageSubmitButtons();
    loadStorageDocument(storageEpoch).catch(() => {});
    return true;
  }
  function storageSubmitError(form) {
    const mode = form?.id === "storage-delete-form" ? "delete" : "edit";
    const operation = storageOperation;
    if (!operation || operation.mode !== mode) return "Open a storage resource before preparing a change.";
    if (!operation.doc) return "Wait for the current restricted resource document to load before preparing a change.";
    const found = observedResource(operation);
    if (!found) return `The ${storageLabel(operation)} is no longer observed. Close this change and open the current resource again.`;
    if (String(found.uid || "") !== operation.uid) {
      return !found.uid
        ? `The ${storageLabel(operation)} no longer has a stable observed UID. Close this change and open the current resource again; no change was sent.`
        : `The ${storageLabel(operation)} was replaced by a new observation. Close this change and open the current resource again; no change was sent.`;
    }
    const doc = operation.doc;
    if (String(doc.uid || "") !== operation.uid || String(doc.kind || "") !== operation.kind ||
      String(doc.namespace || "") !== operation.namespace || String(doc.name || "") !== operation.name) {
      return "The loaded document no longer matches the selected storage resource. Close this change and open it again; no change was sent.";
    }
    const formUID = String($("#storage-" + mode + "-target-uid")?.value || "");
    const formRV = String($("#storage-" + mode + "-target-rv")?.value || "");
    if (formUID !== operation.uid || formRV !== String(doc.resourceVersion || "")) {
      return "The target UID or resourceVersion field no longer matches the loaded document. Close this change and open it again; no change was sent.";
    }
    if (mode === "edit") {
      if (!storageEditingFeature) return storageFeatureReason("edit");
      const yaml = String($("#storage-edit-yaml")?.value || "");
      if (!yaml.trim()) return "The restricted YAML projection is required.";
      if (yaml.length > 32768) return "The restricted YAML projection is limited to 32 KiB.";
      if (!String($("#storage-edit-reason")?.value || "").trim()) return "This console requires a reason for its audit trail before preparing a storage edit.";
      return "";
    }
    if (!storageDeletionFeature) return storageFeatureReason("delete");
    const reason = String($("#storage-delete-reason")?.value || "").trim();
    const confirm = String($("#storage-delete-confirm")?.value || "");
    const acknowledged = !!$("#storage-delete-ack")?.checked;
    if (!reason) return "A reason is required to delete a storage resource.";
    if (!confirm) return `Type "${operation.name}" to confirm the deletion.`;
    if (confirm !== operation.name) return `The confirmation does not match "${operation.name}". Type it exactly.`;
    if (!acknowledged) return "Acknowledge that deleting this storage object can lead to data loss before preparing the deletion plan.";
    if (operation.kind === "PersistentVolumeClaim") {
      const related = relatedPodDefaults({ kind: operation.kind, namespace: operation.namespace, name: operation.name });
      if (related.length && !$("#storage-delete-pod-default-ack")?.checked) {
        return "Acknowledge that the observed related PodDefaults listed in this drawer are deleted with this claim before preparing the deletion plan.";
      }
    }
    return "";
  }
  // A storage plan request is bound to the exact drawer epoch, mode and
  // observed resource identity. A response for a closed or switched context
  // is discarded before it can close another drawer, inject an old error or
  // reopen an old plan.
  function storagePlanContext(form) {
    const operation = storageOperation;
    if (!operation || !form || (form.id !== "storage-edit-form" && form.id !== "storage-delete-form")) return null;
    return {
      mode: operation.mode, epoch: operation.epoch, kind: operation.kind,
      namespace: operation.namespace, name: operation.name, uid: operation.uid,
      resourceVersion: String(operation.doc?.resourceVersion || "")
    };
  }
  function storagePlanContextCurrent(context) {
    if (!context) return true;
    const operation = storageOperation;
    if (!operation) return false;
    return operation.epoch === context.epoch && operation.mode === context.mode &&
      operation.kind === context.kind && operation.namespace === context.namespace &&
      operation.name === context.name && operation.uid === context.uid &&
      !!operation.doc && String(operation.doc.uid || "") === context.uid &&
      String(operation.doc.kind || "") === context.kind && String(operation.doc.namespace || "") === context.namespace &&
      String(operation.doc.name || "") === context.name &&
      String(operation.doc.resourceVersion || "") === context.resourceVersion;
  }
  // A refresh never rewrites the open form. A disappeared or replaced target
  // is reported and its plan is refused until the administrator opens the
  // current resource again and a fresh document is fetched.
  function syncStorageOperation() {
    const operation = storageOperation;
    if (!operation) return "";
    const found = observedResource(operation);
    let message = "";
    if (!found) message = `The ${storageLabel(operation)} is no longer observed. Close this change and open the current resource again.`;
    else if (!found.uid) message = `The ${storageLabel(operation)} no longer has a stable observed UID. Close this change and open the current resource again.`;
    else if (String(found.uid) !== operation.uid) {
      message = `The ${storageLabel(operation)} was replaced by a new observation. Close this change and open the current resource again.`;
    }
    if (!message) return "";
    operation.doc = null;
    const drawer = storageDrawer(operation.mode);
    if (drawer && !drawer.hidden) {
      const loading = $(`#storage-${operation.mode}-loading`); if (loading) loading.hidden = true;
      showFormError($(`#storage-${operation.mode}-form`), message);
    }
    syncStorageSubmitButtons();
    return message;
  }
  // ---- Profile management ---------------------------------------------------
  // The owner of an existing Profile is read-only: the installed Profile
  // controller cannot transfer it. An update therefore submits the exact
  // observed owner and the observed configuration precondition unchanged, and
  // changes only the quota values.
  let profileSnapshot = null;
  function observedProfiles() {
    return (inventory?.resources || []).filter(resource => resource.kind === "Profile" && resource.name);
  }
  function observedProfile(name) {
    return observedProfiles().find(resource => resource.name === name) || null;
  }
  function profileQuotaInputs() { return $$('[data-profile-quota]'); }
  function profileExtraQuotaInputs() { return $$('[data-profile-quota-extra]'); }
  // The device keys the deployment approves. The server owns the same list; the
  // browser only uses it to label an observed key.
  function presetQuotaKeys() { return $$('[data-profile-quota-device]').map(input => input.dataset.profileQuotaDevice); }
  // The observed quota keys that have no static field of their own and that
  // this console may author: the cpu and memory aliases, and a device key the
  // Profile already declares.
  function observedQuotaFields(quota, managed) {
    const presets = presetQuotaKeys();
    const fields = [];
    for (const key of Object.keys(quota).sort()) {
      if (managed.has(key)) continue;
      const kind = logic.profileQuotaKind(key, presets);
      if (kind === "unmanaged") continue;
      fields.push({ key, kind });
    }
    return fields;
  }
  // Render one editable field per observed key. The label names the observed
  // key, so an operator always edits the key the Profile really declares.
  function renderObservedQuotaFields(fields, quota) {
    const host = $("#profile-observed-quota-fields");
    if (!host) return;
    host.replaceChildren();
    if (!fields.length) return;
    host.append(el("p", "These quota keys are declared by this Profile. They are edited under their own names:", "hint"));
    for (const field of fields) {
      const label = el("label", "Quota · " + field.key);
      const input = el("input");
      input.name = "quota." + field.key;
      input.dataset.profileQuota = field.key;
      if (field.kind === "device") { input.type = "number"; input.min = "0"; input.step = "1"; }
      const value = quota[field.key];
      input.value = value === undefined ? "" : String(value);
      // An existing key may not be omitted: the server rejects a removal.
      input.required = true;
      label.append(input);
      host.append(label);
    }
    // An existing bare extended device key is declared but normally not
    // enforced. Editing its own field never renames it or activates
    // enforcement; a migration needs explicit authorization.
    if (fields.some(field => field.kind === "device")) {
      const warning = logic.profileDeviceQuotaWarning(quota);
      if (warning) {
        const note = el("p", warning, "warning");
        note.setAttribute("role", "note");
        host.append(note);
      }
    }
  }
  // Every mode change starts from an empty Profile state: an owner, a quota
  // or a digest of a previous selection never bleeds into the next one.
  function clearProfileState() {
    const owner = $("#profile-update-owner"); if (owner) owner.value = "";
    const digest = $("#profile-update-digest"); if (digest) digest.value = "";
    const extra = $("#profile-extra-quota-fields"); if (extra) extra.replaceChildren();
    const observed = $("#profile-observed-quota-fields"); if (observed) observed.replaceChildren();
    const summary = $("#profile-update-extra"); if (summary) summary.replaceChildren();
    for (const input of profileQuotaInputs()) { input.value = ""; input.required = false; }
    profileSnapshot = null;
  }
  function profileFields() {
    const kind = $("#profile-kind")?.value || "profile.create";
    const update = kind === "profile.update";
    const createFields = $("#profile-create-fields"), updateFields = $("#profile-update-fields");
    if (createFields) createFields.hidden = update;
    if (updateFields) updateFields.hidden = !update;
    const createName = $("#profile-create-name"), updateName = $("#profile-update-name");
    const createOwner = $("#profile-create-owner"), updateOwner = $("#profile-update-owner"), digest = $("#profile-update-digest");
    if (createName) { createName.disabled = update; createName.required = !update; }
    if (updateName) { updateName.disabled = !update; updateName.required = update; }
    if (createOwner) { createOwner.disabled = update; createOwner.required = !update; }
    if (updateOwner) { updateOwner.disabled = !update; updateOwner.readOnly = true; }
    if (digest) digest.disabled = !update;
    clearProfileState();
    if (update) applyProfileSelection();
    else validateProfileName();
  }
  // Load the selected Profile: the observed owner, the observed quota and the
  // observed configuration precondition. The values are read once, at
  // selection time, and the request is checked against them before it is sent.
  function applyProfileSelection() {
    const select = $("#profile-update-name");
    const name = select ? String(select.value || "") : "";
    clearProfileState();
    if (!name) return;
    const profile = observedProfile(name);
    if (!profile) return;
    const values = logic.profileFormValues(profile);
    const quota = logic.profileQuotaValues(profile);
    const owner = $("#profile-update-owner");
    if (owner) owner.value = values.owner || "";
    const digest = $("#profile-update-digest");
    if (digest) digest.value = values.baseConfigDigest || "";
    const managed = new Set();
    for (const input of profileQuotaInputs()) {
      const key = input.dataset.profileQuota;
      managed.add(key);
      const value = quota[key];
      input.value = value === undefined ? "" : value;
      // An existing managed key may not be omitted: the server rejects a removal.
      input.required = value !== undefined && value !== "";
    }
    // An existing Profile may declare a quota key this console has no static
    // field for: the cpu and memory aliases, or a device key the Profile
    // already declares. Such a key is edited under its own name. It is never
    // renamed to a canonical requests.* key and no second key is added beside
    // it.
    const observed = observedQuotaFields(quota, managed);
    renderObservedQuotaFields(observed, quota);
    for (const field of observed) managed.add(field.key);
    // Observed quota keys this console does not manage are returned unchanged.
    const extra = Object.entries(quota).filter(([key]) => !managed.has(key));
    const host = $("#profile-extra-quota-fields");
    if (host) {
      host.replaceChildren();
      if (extra.length) {
        const list = el("div", undefined, "quota-readonly");
        list.append(el("p", "Observed quota keys this console does not manage are returned unchanged and are not editable:", "hint"));
        for (const [key, value] of extra) {
          const row = el("div", undefined, "quota-readonly-row");
          row.append(el("code", key), el("span", value));
          const hidden = el("input");
          hidden.type = "hidden"; hidden.name = "quota." + key; hidden.value = value;
          hidden.dataset.profileQuotaExtra = key;
          row.append(hidden);
          list.append(row);
        }
        host.append(list);
      }
    }
    profileSnapshot = { name, owner: values.owner || "", digest: values.baseConfigDigest || "", quota, uid: profile.uid || "" };
  }
  // The typed name is validated against every observed Profile and namespace.
  // A replacement is only offered as a suggestion and is never applied
  // automatically.
  function validateProfileName() {
    const input = $("#profile-create-name");
    const message = $("#profile-name-message");
    const suggestion = $("#profile-name-suggestion");
    const suggestionText = $("#profile-name-suggestion-text");
    if (!input) return null;
    const value = String(input.value || "");
    const result = logic.profileNameValidation(value, inventory?.resources || []);
    input.title = result.valid ? "" : result.message;
    input.setCustomValidity?.(result.valid ? "" : result.message);
    if (message) message.textContent = value && !result.valid ? result.message : "";
    if (suggestion && suggestionText) {
      if (result.suggestion) {
        suggestion.hidden = false;
        suggestionText.textContent = `Suggested name: ${result.suggestion}`;
        input.setAttribute("aria-describedby", "profile-name-hint profile-name-message profile-name-suggestion-text");
      } else {
        suggestion.hidden = true;
        suggestionText.textContent = "";
        input.setAttribute("aria-describedby", "profile-name-hint profile-name-message");
      }
    }
    return result;
  }
  // A quota update is refused when nothing would change: the server refuses a
  // no-op as well, and the console never sends a pointless request.
  function profileQuotaChanged(observed) {
    const current = {};
    for (const input of profileQuotaInputs()) {
      const value = String(input.value || "").trim();
      if (value) current[input.dataset.profileQuota] = value;
    }
    for (const input of profileExtraQuotaInputs()) current[input.dataset.profileQuotaExtra] = String(input.value || "");
    const keys = new Set([...Object.keys(observed || {}), ...Object.keys(current)]);
    for (const key of keys) if (String((observed || {})[key] ?? "") !== String(current[key] ?? "")) return true;
    return false;
  }
  // Everything that must hold before a Profile request is sent. A read-only
  // field is a convenience, not a guarantee: the server revalidates the same
  // values.
  function profileSubmitError() {
    const kind = $("#profile-kind")?.value || "profile.create";
    if (kind === "profile.update") {
      const snapshot = profileSnapshot;
      if (!snapshot) return "Select the Profile to update.";
      const name = String($("#profile-update-name")?.value || "");
      const owner = String($("#profile-update-owner")?.value || "");
      const digest = String($("#profile-update-digest")?.value || "");
      if (!owner) return "The observed owner is missing. Refresh the inventory and select the Profile again.";
      if (!digest) return "The observed configuration precondition is missing. Refresh the inventory and select the Profile again.";
      if (name !== snapshot.name || owner !== snapshot.owner || digest !== snapshot.digest) {
        return "This form no longer matches the observed Profile. Select the Profile again to load its current owner and quota.";
      }
      // The form was filled from one observation. If the latest observation of
      // the same Profile is a different one, the request would only be
      // refused by the server. The typed values and the precondition are kept
      // exactly as they are; the administrator is asked to reselect.
      const current = observedProfile(snapshot.name);
      const currentDigest = String(current?.details?.configDigest ?? "");
      const currentOwner = String(current?.owner ?? "") || String(current?.details?.owner ?? "");
      const replaced = !!(snapshot.uid && current?.uid && snapshot.uid !== current.uid);
      if (!current || currentDigest !== snapshot.digest || currentOwner !== snapshot.owner || replaced) {
        return "Reselect or reopen the Profile to review the current configuration; your edits were not applied.";
      }
      for (const input of profileQuotaInputs()) {
        if (input.required && !String(input.value || "").trim()) {
          return `The quota key ${input.dataset.profileQuota} is already set on this Profile. Keep its current value: removing a quota key is unsupported.`;
        }
        // A count is a whole number of objects: a fraction of a device is not
        // a quota. Every finite non-negative integer spelling is accepted as
        // typed (1.0, 1e2); the raw value is submitted unchanged and the
        // server validates the authoritative whole Kubernetes quantity.
        const value = String(input.value || "").trim();
        if (value && logic.profileQuotaCounts(input.dataset.profileQuota, presetQuotaKeys()) && !logic.isWholeQuotaCountValue(value)) {
          return `The quota key ${input.dataset.profileQuota} counts whole objects: use a whole number of 0 or more.`;
        }
      }
      if (!profileQuotaChanged(snapshot.quota)) return "Nothing would change: the quota values are identical to the observed Profile quota.";
      return "";
    }
    const name = String($("#profile-create-name")?.value || "");
    const result = logic.profileNameValidation(name, inventory?.resources || []);
    if (!result.valid) return result.message;
    if (!String($("#profile-create-owner")?.value || "").trim()) return "Enter the email address registered with your identity provider.";
    return "";
  }
  function profileDeletionAllowed(profile) {
    if (!profileDeletionFeature) return false;
    if (!profile || !profile.name) return false;
    // A protected Profile is never deletable from this console.
    return String(profile.state || "") !== "Protected";
  }
  function openProfileCreate() {
    const kind = $("#profile-kind");
    if (kind) kind.value = "profile.create";
    profileFields();
  }
  function openProfileUpdate(name) {
    const profile = observedProfile(name);
    if (!profile) {
      notify("error", "That Profile is not observed any more. Refresh the inventory and try again.", "action", { persist: false });
      return;
    }
    const kind = $("#profile-kind");
    if (kind) kind.value = "profile.update";
    profileFields();
    const select = $("#profile-update-name");
    if (select) select.value = name;
    applyProfileSelection();
    openDrawer($("#profile-drawer"));
  }
  function openProfileDelete(profile) {
    if (!profileDeletionAllowed(profile)) return;
    const name = profile?.name || "";
    const title = $("#profile-delete-title");
    if (title) title.textContent = "Delete " + name;
    const target = $("#profile-delete-target");
    if (target) target.textContent = `Deleting the Profile ${name} also deletes the namespace ${name} and everything inside it.`;
    const hidden = $("#profile-delete-name"); if (hidden) hidden.value = name;
    const reason = $("#profile-delete-reason"); if (reason) reason.value = "";
    const confirm = $("#profile-delete-confirm"); if (confirm) confirm.value = "";
    openDrawer($("#profile-delete-drawer"));
  }
  async function copyOwnerEmail(value, button) {
    const text = String(value || "");
    try {
      if (typeof navigator === "undefined" || !navigator.clipboard || !navigator.clipboard.writeText) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(text);
      button.textContent = "Copied";
    } catch {
      button.textContent = "Copy failed";
    }
    setTimeout(() => { button.textContent = "Copy"; }, 2000);
  }
  // The consolidated safe Profile summary, shown under the scoped views. Every
  // projected fact of the Profile is rendered here once: the internal
  // configuration precondition is excluded by the shared safe-details helper
  // and is never rendered or searchable.
  function profileConfiguration(name) {
    const profile = observedProfile(name);
    const summary = logic.profileSummaryDetails(profile || { kind: "Profile", name });
    const section = el("section", undefined, "profile-configuration");
    section.append(el("h3", "Profile configuration"));
    const facts = el("dl", undefined, "detail-list");
    facts.append(el("dt", "Kind"), el("dd", summary.kind || "Profile"));
    facts.append(el("dt", "API version"), el("dd", summary.apiVersion || "Not reported"));
    const ownerValue = el("dd");
    ownerValue.append(el("span", summary.owner || "Not reported"));
    if (summary.owner) {
      const copy = el("button", "Copy", "secondary small");
      copy.type = "button";
      copy.setAttribute("aria-label", "Copy the owner email " + summary.owner);
      copy.addEventListener("click", () => copyOwnerEmail(summary.owner, copy));
      ownerValue.append(copy);
    }
    facts.append(el("dt", "Owner"), ownerValue);
    if (summary.ownerKind) facts.append(el("dt", "Owner kind"), el("dd", summary.ownerKind));
    facts.append(el("dt", "Observed state"), el("dd", summary.state || "Not reported"));
    facts.append(el("dt", "Scope"), el("dd", summary.scope || "Cluster scoped"));
    facts.append(el("dt", "Created"), el("dd", summary.createdAt || "Not reported"));
    facts.append(el("dt", "Resource version"), el("dd", summary.resourceVersion || "Not reported"));
    const quota = summary.quota;
    const quotaValue = el("dd");
    if (Object.keys(quota).length) {
      const list = el("ul", undefined, "quota-list");
      for (const [key, value] of Object.entries(quota)) {
        const item = el("li");
        item.append(el("code", key), el("span", value));
        list.append(item);
      }
      quotaValue.append(list);
    } else quotaValue.textContent = "No resource quota is observed for this Profile.";
    facts.append(el("dt", "resourceQuotaSpec"), quotaValue);
    section.append(facts);
    const warning = logic.profileDeviceQuotaWarning(quota);
    if (warning) {
      const note = el("p", warning, "warning");
      note.setAttribute("role", "note");
      section.append(note);
    }
    // Scopes and plugins are preserved by the server. This console edits only
    // quota values and never claims to rewrite arbitrary Profile YAML.
    section.append(el("p", "Scopes and plugins are preserved by the server. This console edits quota values only; it never rewrites arbitrary Profile YAML.", "hint"));
    return section;
  }
  function mountFields() { const enabled = $("#notebook-kind").value === "notebook.edit" && $("#replace-mounts").checked; $$("#mount-rows input,#mount-rows select").forEach(input => { input.disabled = !enabled; }); }
  // A new static PV has its own name, separate from the new PVC name. The
  // existing-PV selector is intentionally only enabled for reservations.
  const newPVLabel = el("label", "New PV name"); newPVLabel.dataset.storage = "pv.nfs.create";
  const newPVInput = el("input"); newPVInput.name = "pvName"; newPVInput.required = true; newPVInput.placeholder = "team-workspace-pv";
  newPVLabel.append(newPVInput); $("#pv-selector").parentElement.before(newPVLabel);
  function addMount() {
    const row = el("div", undefined, "mount-row");
    const namespace = $('#notebook-form [name="namespace"]').value;
    const name = el("input"); name.dataset.field = "name"; name.required = true; name.placeholder = "volume-name";
    const claim = el("select"); claim.dataset.field = "claim"; claim.required = true;
    fillSelect(claim, (inventory?.resources || []).filter(resource => resource.kind === "PersistentVolumeClaim" && resource.namespace === namespace).map(resource => resource.name), "Select PVC in this namespace");
    const path = el("input"); path.dataset.field = "path"; path.required = true; path.placeholder = "/home/jovyan/data"; path.pattern = "/.*";
    [["Volume name", name], ["Claim", claim], ["Mount path", path]].forEach(([title, input]) => { const label = el("label", title); label.append(input); row.append(label); });
    const readOnly = el("input"); readOnly.type = "checkbox"; readOnly.dataset.field = "readOnly"; const label = el("label", undefined, "check"); label.append(readOnly, document.createTextNode("Read only")); row.append(label);
    const remove = el("button", "Remove mount", "secondary"); remove.type = "button"; remove.addEventListener("click", () => row.remove()); row.append(remove); $("#mount-rows").append(row); $("#replace-mounts").checked = true; mountFields();
  }
  // The Profile form keeps one control for a new name and another for an
  // existing Profile, and one editable owner and one read-only owner. The
  // action field names are restored before the payload is built, so a request
  // only ever carries the typed action fields of the active mode.
  const formFieldNames = {
    profileCreateName: "name", profileTarget: "name",
    profileCreateOwner: "owner", profileOwner: "owner",
    profileBaseConfigDigest: "baseConfigDigest"
  };
  function actionEntries(form) {
    return [...new FormData(form)].map(([name, value]) => [formFieldNames[name] || name, value]);
  }
  function actionFrom(form) {
    const bound = form.id === "storage-form" ? storageBoundSelection() : null;
    const action = logic.buildAction(actionEntries(form), {
      restart: !!$('[name="restart"]', form)?.checked,
      force: !!$('[name="force"]', form)?.checked,
      replaceMounts: form.id === "notebook-form" && $("#replace-mounts").checked,
      mounts: $$(".mount-row").map(row => Object.fromEntries($$("[data-field]", row).map(input => [input.dataset.field, input.type === "checkbox" ? input.checked : input.value]))),
      podDefault: bound ? storagePodDefaultRequest(bound) : null
    });
    if (form.id === "storage-delete-form" && $("#storage-delete-pod-default-ack")?.checked) action.confirmPodDefaultCleanup = true;
    return action;
  }
  function simpleTable(target, headings, rows) {
    const wrap = el("div", undefined, "table-scroll compact"), table = el("table"), head = el("thead"), headRow = el("tr"), body = el("tbody");
    headings.forEach(heading => headRow.append(el("th", heading)));
    head.append(headRow); table.append(head);
    rows.forEach(values => {
      const row = el("tr");
      values.forEach(value => {
        const cell = el("td");
        if (value instanceof Node) cell.append(value); else cell.textContent = value ?? "—";
        row.append(cell);
      });
      body.append(row);
    });
    table.append(body); wrap.append(table); target.append(wrap);
  }
  function preview(plan) {
    const root = $("#plan-preview"); root.replaceChildren();
    const list = el("dl", undefined, "preview-fields");
    logic.previewFields(plan).forEach(([key, value]) => {
      const display = value === null || value === undefined || value === "" ? "—" : typeof value === "object" ? JSON.stringify(value, null, 2) : String(value);
      list.append(el("dt", key), el("dd", display));
    }); root.append(list);
    if (plan.prepared.changes?.length) {
      root.append(el("h3", "Validated before / after"));
      const changes = el("div"); changes.id = "plan-changes"; root.append(changes);
      simpleTable(changes, ["Allowed field", "Before", "After"], plan.prepared.changes.map(change => [change.field, change.before, change.after]));
    }
    (plan.prepared.warnings || []).forEach(warning => root.append(el("div", warning, "warning plan-warning")));
  }
  function syncWriteButtons() {
    $$('[data-action-form] button[type="submit"]').forEach(button => {
      const form = button.closest("form");
      let disabled = !writes;
      if (form?.id === "notebook-form" && !notebookWrites) disabled = true;
      if (form?.id === "pod-restart-form" && !podRestartFeature) disabled = true;
      if (form?.id === "node-cordon-form" && !nodeCordonFeature) disabled = true;
      if (form?.id === "storage-edit-form" && !storageEditingFeature) disabled = true;
      if (form?.id === "storage-delete-form" && !storageDeletionFeature) disabled = true;
      button.disabled = disabled;
    });
    syncStorageSubmitButtons();
  }
  $$('[data-action-form]').forEach(form => form.addEventListener("submit", async event => {
    event.preventDefault();
    const button = $('button[type="submit"]', form);
    if (button.disabled) return;
    button.disabled = true;
    clearFormError(form);
    // A refused submission never reaches the server and never prepares a plan.
    const refused = preSubmitError(form);
    if (refused) {
      showFormError(form, refused, "validation");
      button.disabled = false;
      notify("error", refused, "action", { persist: false });
      return;
    }
    let storageContext = null;
    let storageCreate = null;
    try {
      storageContext = storagePlanContext(form);
      if (form.id === "storage-form") storageCreate = storageCreateContext();
      if (storageContext) {
        storageOperation.planning = true;
        syncStorageSubmitButtons();
      }
      const plan = await api("plans", enforceStorageAction(actionFrom(form)));
      if (storageContext && !storagePlanContextCurrent(storageContext)) return;
      // A late answer for a form whose action, target, template or class was
      // changed while the request was in flight is discarded before it can
      // open a plan for a different review.
      if (storageCreate && !storageCreateContextCurrent(storageCreate)) return;
      if (storageContext) storageOperation.planning = false;
      clearFormError(form);
      currentPlan = plan; submitKey = crypto.randomUUID(); preview(plan);
      $("#confirm-plan").checked = false; $("#submit-plan").disabled = true; $("#submit-status").textContent = "";
      closeDrawers(true);
      $("#plan-dialog").showModal();
    } catch (error) {
      // A response for a closed or switched storage context is discarded. It
      // must never close the new drawer or inject the old target's error into
      // the new form.
      if (storageContext && !storagePlanContextCurrent(storageContext)) return;
      if (storageCreate && !storageCreateContextCurrent(storageCreate)) return;
      if (storageContext && storageOperation && storageOperation.epoch === storageContext.epoch) storageOperation.planning = false;
      // The drawer-scoped error is never suppressed by a dismissed toast and
      // survives until the user retries or closes the drawer.
      if (form.id === "storage-delete-form" && /related PodDefaults|cleanup acknowledgement/i.test(String(error.message || ""))) {
        const podDefaultAck = $("#storage-delete-pod-default-ack");
        if (podDefaultAck) { podDefaultAck.disabled = false; podDefaultAck.checked = false; podDefaultAck.focus(); }
      }
      if (form.id === "storage-form" && /PodDefault.*exists|overwrite acknowledgement/i.test(String(error.message || ""))) {
        const overwrite = $("#storage-pod-default-overwrite");
        if (overwrite) { overwrite.disabled = false; overwrite.checked = false; overwrite.focus(); }
      }
      showFormError(form, error.message);
      notify("error", error.message, "action", { persist: false });
    }
    finally {
      if (storageContext && storageOperation && storageOperation.epoch === storageContext.epoch) storageOperation.planning = false;
      syncWriteButtons();
    }
  }));
  $("#confirm-plan").addEventListener("change", () => { $("#submit-plan").disabled = !$("#confirm-plan").checked || !currentPlan || Date.now() >= new Date(currentPlan.expiresAt).getTime(); });
  $("#cancel-plan").addEventListener("click", () => {
    if (submitInflight) return;
    $("#plan-dialog").close(); currentPlan = null; submitKey = null;
  });
  $("#plan-dialog").addEventListener("cancel", event => {
    if (submitInflight) { event.preventDefault(); return; }
    currentPlan = null; submitKey = null;
  });
  $("#submit-plan").addEventListener("click", async () => {
    if (!currentPlan || !$("#confirm-plan").checked) return;
    if (Date.now() >= new Date(currentPlan.expiresAt).getTime()) { $("#submit-status").textContent = "This plan has expired. Cancel and prepare a new plan."; $("#submit-plan").disabled = true; return; }
    $("#submit-plan").disabled = true; $("#cancel-plan").disabled = true; submitInflight = true;
    try {
      const operation = await api("operations", { planID: currentPlan.id, idempotencyKey: submitKey }, submitKey);
      $("#plan-dialog").close(); currentPlan = null; submitKey = null;
      invalidateHistoryCaches();
      notify("success", `Operation ${operation.id} accepted (${operation.state}). History will reload when opened.`, "action-success", { persist: false });
      await refresh(false);
    } catch (error) {
      // Preserve the same plan/key after an ambiguous network failure: retry is safe.
      $("#submit-status").textContent = error.message + (error.retryable === false ? " Cancel to review the target or prepare a new plan; check History first." : " Check History before preparing another plan. Retrying here reuses the same idempotency key.");
      $("#submit-plan").disabled = error.retryable === false;
    } finally { submitInflight = false; $("#cancel-plan").disabled = false; }
  });

  // ---- Wiring --------------------------------------------------------------
  $("#refresh").addEventListener("click", () => refresh(true));
  $("#overview-namespace").addEventListener("change", () => setNamespace($("#overview-namespace").value));
  $("#toggle-overview-table").addEventListener("click", () => {
    overviewTableExpanded = !overviewTableExpanded;
    syncOverviewTable();
    if (overviewTableExpanded) renderTable("overview");
  });
  $("#storage-kind").addEventListener("change", () => {
    // A new action is a new provisioning context: no template, class, PV,
    // path, UID/GID or PodDefault value from the previous action may travel.
    clearStorageTargets();
    const registration = $("#storage-registration-template");
    if (registration) registration.value = "";
    storageFields();
    clearFormError($("#storage-form"));
  });
  $("#storage-registration-template").addEventListener("change", storageTemplateChanged);
  $("#storage-class").addEventListener("change", () => { syncStorageBoundPVs($("#storage-kind")?.value || "", selectedStorageTemplate()); });
  $("#bound-pv-selector").addEventListener("change", () => {
    const volume = selectedObservedPV();
    // The visible selection is the only target; the plan reads the exact
    // observed UID/resourceVersion at submission time and refuses a changed
    // observation instead of reusing a stale identity.
    if (volume) $("#storage-class").value = volume.details?.storageClass || $("#storage-class").value;
    clearFormError($("#storage-form"));
  });
  $("#storage-pod-default-enable").addEventListener("change", syncStoragePodDefaultFields);
  $("#storage-pod-default-name").addEventListener("input", storagePodDefaultFeedback);
  $("#storage-pod-default-overwrite").addEventListener("change", storagePodDefaultFeedback);
  [...new Set([storageNameInput(), $("#storage-pv-name"), $("#storage-claim-name")].filter(Boolean))].forEach(input => {
    input.addEventListener("input", () => { syncStoragePodDefaultSuggestion(); });
  });
  $('#storage-form [name="namespace"]')?.addEventListener("change", () => {
    // PodDefault collision detection is namespace-local; a namespace change
    // must re-evaluate the warning without discarding the still-compatible PV.
    syncStoragePodDefaultSuggestion();
    syncStoragePodDefaultFields();
    clearFormError($("#storage-form"));
  });
  $("#notebook-kind").addEventListener("change", notebookFields);
  $("#profile-kind").addEventListener("change", profileFields);
  // The typed name is validated as it is typed. The suggestion is only
  // applied when the administrator asks for it.
  $("#profile-create-name")?.addEventListener("input", () => validateProfileName());
  $("#profile-use-suggestion")?.addEventListener("click", () => {
    const input = $("#profile-create-name");
    const result = validateProfileName();
    if (!input || !result || !result.suggestion) return;
    input.value = result.suggestion;
    validateProfileName();
    input.focus();
  });
  $("#profile-update-name")?.addEventListener("change", () => applyProfileSelection());
  $("#storage-template").addEventListener("change", () => { const option = $("#storage-template").selectedOptions[0]; if (option?.dataset.class) $("#storage-class").value = option.dataset.class; storageFields(); });
  $("#pv-selector").addEventListener("change", () => { const volume = (inventory?.resources || []).find(resource => resource.kind === "PersistentVolume" && resource.name === $("#pv-selector").value); if (volume) $("#storage-class").value = volume.details?.storageClass || ""; });
  $('#notebook-form [name="namespace"]').addEventListener("change", () => {
    // A new namespace is a new target: no previous resource, image, GPU, mount
    // or restart edit value may travel with it.
    clearNotebookTargetChangeState();
    notebookOptions();
    notebookFields();
    bindNotebookSelection();
  });
  $("#notebook-selector").addEventListener("change", () => {
    clearNotebookTargetChangeState();
    containerOptions();
    notebookFields();
    bindNotebookSelection();
  });
  $("#container-selector").addEventListener("change", () => {
    // The chosen container itself is kept; only its edit values are cleared.
    clearNotebookEditState({ preserveContainer: true });
    clearFormError($("#notebook-form"));
    notebookFields();
  });
  $("#add-mount").addEventListener("click", addMount);
  $("#replace-mounts").addEventListener("change", mountFields);
  $('#replace-mounts').closest('fieldset').querySelector('.hint').textContent = "Unchecked preserves current mounts. Checked with no rows requests removal of all PVC mounts from this container; the server rejects removing protected workspace mounts.";

  activatePage();
  if (!notebookWrites) notify("warning", "Notebook changes are disabled: the installed controller contract has not been confirmed by the operator.", "policy:notebook-controller");
  if (!meta("pod-restarts").startsWith("true")) notify("warning", "Pod restarts are disabled by server policy. Restart controls are unavailable until the feature and its optional permission are enabled.", "policy:pod-restarts");
  if (meta("node-cordon") !== "true") notify("warning", "Node cordon and uncordon are disabled by server policy. Cordon controls are unavailable until the feature and its optional permission are enabled.", "policy:node-cordon");
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) return;
    ensureActiveHistory();
    refresh(false);
  });
  const quotaKeys = new Set(); $$('[data-gpu-quota]').forEach(label => { if (quotaKeys.has(label.dataset.gpuQuota)) label.remove(); else quotaKeys.add(label.dataset.gpuQuota); });
  storageFields(); notebookFields(); profileFields(); syncWriteButtons(); syncNotebookPagePolicy();
  refresh(false);
  setInterval(updateTimestamp, 10000);
  setInterval(() => refresh(false), 30000);
})();
