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
  const notificationScope = meta("notification-scope");
  const logic = globalThis.DashboardLogic;

  const pageTitles = {
    overview: ["Overview", "A clear view of your Kubeflow workspace."],
    profiles: ["Profiles", "Manage ownership, namespaces and resource quotas."],
    storage: ["Storage", "Provision and grow persistent storage with explicit safeguards."],
    notebooks: ["Notebooks", "Manage workloads and resources with observed-state verification."],
    pods: ["Pods", "Observed workloads, ownership and requested resources."],
    history: ["History", "Operations and audit records in one accountable view."]
  };

  const route = logic.routeFor(location.pathname, location.search);
  let page = route.page;
  let historyTab = route.tab || "operations";
  let namespaceContext = logic.queryNamespace(location.search);
  let inventory = null, inventoryAt = 0, inventoryError = false, loading = false;
  let currentPlan = null, submitKey = null, submitInflight = false;
  let drawerOpener = null;
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
    storage: logic.defaultTableState("name"),
    notebooks: logic.defaultTableState("name"),
    pods: logic.defaultTableState("name"),
    historyOperations: logic.defaultTableState("time", "desc"),
    historyAudit: logic.defaultTableState("time", "desc")
  };
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
  const noticeLabels = { info: "Information", success: "Success", warning: "Warning", error: "Error" };

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
    if (remember && logic.shouldRememberDismissals(notificationScope)) { dismissed.add(id); persistDismissed(); }
    if (hadFocus) {
      const open = openDrawers();
      const drawerButton = open.length ? open[open.length - 1].querySelector("button[data-close-drawer]") : null;
      const target = drawerButton || $("#notifications button") || $("#refresh");
      if (target) target.focus();
    }
  }
  function notify(level, message, code = "general", options = {}) {
    const text = String(message || "").trim();
    if (!text) return;
    const persist = options.persist === undefined ? persistableCode(code) : !!options.persist;
    const id = logic.notificationID(level, code, text);
    if ((persist && dismissed.has(id)) || visibleNotifications.has(id)) return;
    const article = el("article", undefined, "notification " + level);
    article.dataset.notificationId = id;
    article.dataset.persist = persist ? "1" : "0";
    article.setAttribute("role", level === "error" || level === "warning" ? "alert" : "status");
    const body = el("div", undefined, "notification-body");
    body.append(el("strong", noticeLabels[level] || "Notice"), el("p", text));
    const close = el("button", "Dismiss", "dismiss");
    close.type = "button";
    close.setAttribute("aria-label", "Dismiss " + text.slice(0, 90));
    close.addEventListener("click", () => removeNotification(id, persist));
    article.append(body, close);
    $("#notifications").append(article);
    visibleNotifications.set(id, article);
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
    const cls = rank === 0 ? "badge good" : rank === 1 ? "badge warn" : rank >= 2 ? "badge bad" : "badge";
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
      cpuRequest: podResourceDetail(resource, "cpuRequest", "requested.cpu"), memoryRequest: podResourceDetail(resource, "memoryRequest", "requested.memory"),
      cpuLimit: podResourceDetail(resource, "cpuLimit", "limit.cpu"), memoryLimit: podResourceDetail(resource, "memoryLimit", "limit.memory"), time: ""
    };
  }
  function resourceSearch(resource) {
    const details = Object.entries(resource.details || {}).map(([key, value]) => `${key} ${value}`).join(" ");
    return [resource.name, resource.kind, resource.namespace, resource.owner, resource.state, desired(resource), capacity(resource), details].filter(Boolean).join(" ");
  }
  function detailsNode(resource) {
    const details = resource.details || {}, keys = Object.keys(details);
    if (!keys.length) return null;
    const disclosure = el("details"), summary = el("summary", "Details", "detail-button"), content = el("div", undefined, "details");
    content.textContent = keys.sort().map(key => `${key}: ${details[key]}`).join("\n");
    disclosure.append(summary, content);
    return disclosure;
  }
  function resourceNameNode(resource) {
    const node = el("div", undefined, "resource-name");
    if (resource.kind === "Profile") {
      const open = el("button", resource.name || "Unnamed", "link-button");
      open.type = "button";
      open.addEventListener("click", () => openProfileSummary(resource.name));
      node.append(open);
    } else node.append(el("span", resource.name || "Unnamed"));
    node.append(el("small", [resource.kind, resource.namespace, resource.owner].filter(Boolean).join(" · ")));
    const disclosure = detailsNode(resource);
    if (disclosure) node.append(disclosure);
    return node;
  }
  function actorCell(record) {
    const label = logic.actorLabel(record), id = logic.actorID(record);
    const node = el("div", undefined, "actor-cell");
    node.append(el("span", label));
    if (id && id !== label) node.append(el("small", id));
    return node;
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
      label: "profile resources", empty: "No actual Profiles are observed in this scope. Associated namespaces appear only when a Profile with the same name exists.",
      columns: [{ key: "name", label: "Resource" }, { key: "type", label: "Type" }, { key: "namespace", label: "Namespace" }, { key: "state", label: "Observed", type: "severity" }, { key: "desired", label: "Desired / requested", type: "mixed" }]
    },
    storage: {
      label: "storage resources", empty: "No matching storage resources. Choose another namespace or refresh inventory.",
      columns: [{ key: "name", label: "Resource" }, { key: "type", label: "Resource Type" }, { key: "requested", label: "Requested", type: "quantity" }, { key: "state", label: "Observed", type: "severity" }, { key: "capacity", label: "Capacity", type: "quantity" }, { key: "storageClass", label: "Storage class" }]
    },
    notebooks: {
      label: "notebook resources", empty: "No matching Notebooks or workload resources. Choose another namespace or refresh inventory.",
      columns: [{ key: "name", label: "Resource" }, { key: "desired", label: "Desired / requested", type: "mixed" }, { key: "state", label: "Observed", type: "severity" }, { key: "condition", label: "Capacity / condition", type: "text" }, { key: "owner", label: "Owner" }]
    },
    pods: {
      label: "Pods", empty: "No matching Pods. Choose another namespace or refresh inventory.",
      columns: [
        { key: "name", label: "Pod" }, { key: "ready", label: "Ready", type: "ready" }, { key: "restarts", label: "Restarts", type: "number" },
        { key: "cpuRequest", label: "CPU request", type: "quantity" }, { key: "memoryRequest", label: "Memory request", type: "quantity" },
        { key: "cpuLimit", label: "CPU limit", type: "quantity" }, { key: "memoryLimit", label: "Memory limit", type: "quantity" },
        { key: "state", label: "State", type: "severity" }, { key: "owner", label: "Owner" }, { key: "actions", label: "Restart", sortable: false }
      ]
    },
    historyOperations: {
      label: "operation history", empty: "No operation records are loaded in this recent window.",
      columns: [{ key: "time", label: "Time", type: "time" }, { key: "actor", label: "Actor" }, { key: "kind", label: "Kind" }, { key: "namespace", label: "Namespace" }, { key: "name", label: "Name" }, { key: "action", label: "Action" }, { key: "state", label: "State", type: "severity" }, { key: "step", label: "Step" }, { key: "detail", label: "Detail", sortable: false }, { key: "id", label: "IDs" }]
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
      return scoped.filter(resource => ["Notebook", "PersistentVolumeClaim", "Pod", "ResourceQuota", "Event"].includes(resource.kind)).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), desired(resource), badge(resource.state), capacity(resource)]));
    }
    if (stateKey === "profiles") {
      return logic.profileResources(resources, namespaceContext).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), resource.kind || "—", resource.namespace || "—", badge(resource.state), desired(resource)]));
    }
    if (stateKey === "storage") {
      return scoped.filter(resource => ["PersistentVolume", "PersistentVolumeClaim", "StorageClass"].includes(resource.kind)).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), resource.kind || "—", resource.details?.requested || "—", badge(resource.state), capacity(resource), resource.details?.storageClass || "—"]));
    }
    if (stateKey === "notebooks") {
      return scoped.filter(resource => ["Notebook", "Pod", "StatefulSet"].includes(resource.kind)).map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), desired(resource), badge(resource.state), notebookCondition(resource), resource.owner || "Not reported"]));
    }
    if (stateKey === "pods") {
      return scoped.filter(resource => resource.kind === "Pod").map(resource =>
        resourceRow(resource, () => [resourceNameNode(resource), podReady(resource), podRestartCount(resource), podResourceDetail(resource, "cpuRequest", "requested.cpu"), podResourceDetail(resource, "memoryRequest", "requested.memory"), podResourceDetail(resource, "cpuLimit", "limit.cpu"), podResourceDetail(resource, "memoryLimit", "limit.memory"), badge(resource.state), resource.owner || resource.details?.owner || "Not reported", restartButton(resource)]));
    }
    if (stateKey === "historyOperations") {
      return (historyCache.operations.rows || []).map(operation => {
        const target = logic.parseTarget(operation.target);
        return {
          namespace: target.namespace || (target.kind === "profile" ? target.name : ""),
          search: [operation.id, operation.planID, target.kind, target.namespace, target.name, operation.action, operation.state, operation.step, operation.error, logic.actorLabel(operation), logic.actorID(operation)].filter(Boolean).join(" "),
          sort: { time: operation.updatedAt || operation.createdAt, actor: logic.actorLabel(operation), kind: target.kind, namespace: target.namespace, name: target.name, action: operation.action, state: operation.state, step: operation.step, id: operation.id || "" },
          cellFactory: () => {
            const detail = el("div", undefined, "history-detail");
            if (operation.historyExpired) detail.append(badge("History expired"));
            detail.append(el("span", operation.error || (operation.state === "succeeded" ? "Observed convergence verified" : "Not yet verified")));
            return [formatTime(operation.updatedAt || operation.createdAt), actorCell(operation), target.kind || "—", target.namespace || "—", target.name || "—", operation.action || "—", badge(operation.state), operation.step || "—", detail, idCell([["Operation", operation.id], ["Plan", operation.planID]])];
          }
        };
      });
    }
    if (stateKey === "historyAudit") {
      return logic.groupAudit(historyCache.audit.rows || []).map(group => ({
        namespace: group.namespace || (group.kind === "profile" ? group.name : ""),
        search: [group.operationID, group.actorLabel, group.actorID, group.kind, group.namespace, group.name, group.action, group.result, group.reason, group.stages.map(stage => stage.id).join(" ")].filter(Boolean).join(" "),
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
    return stateKey + "|" + inventoryAt + "|" + namespaceContext;
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
  function buildControls(stateKey) {
    const def = tableDefs[stateKey], state = tableStates[stateKey];
    const controls = el("div", undefined, "table-controls");
    const searchLabel = el("label", undefined, "control-field");
    searchLabel.append(document.createTextNode("Search"));
    const search = el("input");
    search.type = "search"; search.value = state.search; search.placeholder = "Filter loaded rows";
    search.dataset.search = "1";
    search.setAttribute("aria-label", "Search " + def.label);
    search.addEventListener("input", () => { const live = tableStates[stateKey]; live.search = search.value; live.page = 1; renderTable(stateKey); });
    searchLabel.append(search);
    const scopeLabel = el("label", undefined, "control-field");
    scopeLabel.append(document.createTextNode("Namespace"));
    const select = el("select");
    select.dataset.namespace = "1";
    select.setAttribute("aria-label", "Namespace for " + def.label);
    select.addEventListener("change", () => setNamespace(select.value));
    scopeLabel.append(select);
    const count = el("span", undefined, "result-count");
    count.dataset.count = "1";
    count.setAttribute("role", "status");
    count.setAttribute("aria-live", "polite");
    controls.append(searchLabel, scopeLabel, count);
    return controls;
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
    const scoped = tableRows(stateKey).filter(row => !namespaceContext || !row.namespace || row.namespace === namespaceContext);
    const filtered = scoped.filter(row => logic.searchMatch(row.search, state.search));
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
      container.append(el("div", def.empty, "empty-state"));
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
  function renderMetrics() {
    const scoped = logic.scopedResources(inventory?.resources || [], namespaceContext);
    const groups = [["Profiles", "Profile", "Workspace ownership"], ["Notebooks", "Notebook", "Desired and observed lifecycle"], ["Volume claims", "PersistentVolumeClaim", "Persistent workspace storage"], ["Pods", "Pod", "Observed workload state"]];
    $("#metrics").replaceChildren(...groups.map(([label, kind, caption]) => {
      const metric = el("div", undefined, "metric");
      metric.append(el("small", label), el("strong", String(scoped.filter(resource => resource.kind === kind).length)), el("span", caption));
      return metric;
    }));
  }
  function renderPage() {
    if (page === "overview") { renderMetrics(); renderTable("overview"); }
    else if (page === "profiles") renderTable("profiles");
    else if (page === "storage") renderTable("storage");
    else if (page === "notebooks") renderTable("notebooks");
    else if (page === "pods") renderTable("pods");
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
    updateTimestamp(); selectors(); renderPage();
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
  function activatePage() {
    const current = logic.routeFor(location.pathname, location.search);
    page = current.page;
    if (current.tab) historyTab = current.tab;
    $("#page-title").textContent = pageTitles[page]?.[0] || "Overview";
    $("#page-subtitle").textContent = pageTitles[page]?.[1] || "";
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
    window.history.replaceState(null, "", url.pathname + url.search);
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
    closeDrawers(false);
    namespaceContext = logic.queryNamespace(target.search);
    window.history.pushState(null, "", target.pathname + target.search);
    selectors(true);
    navigate();
  });

  // ---- Drawers -------------------------------------------------------------
  function setBackgroundInert(value) {
    const main = $("main"), sidebar = $(".sidebar");
    if (main) main.inert = value;
    if (sidebar) sidebar.inert = value;
  }
  function showFormError(form, message) {
    let region = form.querySelector(".form-error");
    if (!region) { region = el("p", undefined, "form-error"); region.setAttribute("role", "alert"); form.append(region); }
    region.textContent = String(message || "The request could not be completed.");
    region.hidden = false;
  }
  function clearFormError(form) {
    const region = form.querySelector(".form-error");
    if (region) { region.textContent = ""; region.hidden = true; }
  }
  function clearFormErrors() { $$(".form-error").forEach(region => { region.textContent = ""; region.hidden = true; }); }
  function openDrawers() { return $$(".drawer").filter(drawer => !drawer.hidden); }
  function openDrawer(drawer) {
    if (!drawer) return;
    closeDrawers(false);
    drawerOpener = document.activeElement;
    $("#drawer-scrim").hidden = false;
    drawer.hidden = false;
    document.body.classList.add("drawer-open");
    setBackgroundInert(true);
    const first = drawer.querySelector("input:not([type='hidden']):not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled]), a[href]");
    if (first) first.focus();
  }
  function restoreDrawerFocus(drawer) {
    let target = drawerOpener;
    if (!target || target === document.body || target.isConnected === false) {
      const kind = String(drawer?.id || "").replace(/-drawer$/, "");
      target = kind ? $(`[data-open-drawer="${kind}"]`) : null;
      if (!target) {
        target = $("#page-title");
        if (target && !target.hasAttribute("tabindex")) target.setAttribute("tabindex", "-1");
      }
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
  }
  function focusable(drawer) {
    return $$("a[href], button:not([disabled]), input:not([type='hidden']):not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])", drawer)
      .filter(node => !node.hidden && !node.closest("[hidden]") && (typeof node.getClientRects !== "function" || node.getClientRects().length > 0));
  }
  function openProfileSummary(name) {
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
    openDrawer(drawer);
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
  $$("[data-open-drawer]").forEach(button => button.addEventListener("click", () => openDrawer($(`#${button.dataset.openDrawer}-drawer`))));
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

  // ---- Forms and plan workflow --------------------------------------------
  function selectors(applyFilter = false) {
    const resources = inventory?.resources || [];
    $$("[data-namespaces]").forEach(select => {
      fillSelect(select, scopeNamespaces(), "Select a namespace");
      if (namespaceContext && (applyFilter || !select.value)) select.value = namespaceContext;
    });
    if (applyFilter) { $("#mount-rows").replaceChildren(); $("#replace-mounts").checked = false; }
    $("#profile-names").replaceChildren(...resources.filter(resource => resource.kind === "Profile").map(resource => new Option(resource.name, resource.name)));
    $("#claim-names").replaceChildren(...resources.filter(resource => resource.kind === "PersistentVolumeClaim" && (!namespaceContext || resource.namespace === namespaceContext)).map(resource => new Option(resource.name, resource.name)));
    fillSelect($("#pv-selector"), resources.filter(resource => resource.kind === "PersistentVolume" && resource.state === "Available" && !resource.details?.claim && resource.details?.reclaimPolicy === "Retain").map(resource => resource.name), "Select an available Retain PV");
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
    const template = $("#storage-template").selectedOptions[0];
    if (kind === "pv.nfs.create" && template?.dataset.type === "csi") { $("#subdirectory-field").hidden = true; $("#subdirectory-field input").disabled = true; }
    $('#storage-form [name="size"]').required = true;
    $("#pv-selector").required = kind === "pv.reserve";
    $("#storage-template").required = kind === "pv.nfs.create";
    $("#subdirectory-field input").required = kind === "pv.nfs.create" && template?.dataset.type !== "csi";
    $("#storage-class").required = kind === "pvc.create";
  }
  function notebookFields() {
    const edit = $("#notebook-kind").value === "notebook.edit"; $("#notebook-edit").hidden = !edit;
    $$("input,select,button", $("#notebook-edit")).forEach(input => { input.disabled = !edit; });
    $("#container-selector").required = edit;
    mountFields();
  }
  function profileFields() { const owner = $('#profile-form [name="owner"]'); owner.disabled = $("#profile-kind").value !== "profile.create"; owner.required = !owner.disabled; }
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
  function actionFrom(form) {
    return logic.buildAction(new FormData(form), {
      restart: !!$('[name="restart"]', form)?.checked,
      replaceMounts: form.id === "notebook-form" && $("#replace-mounts").checked,
      mounts: $$(".mount-row").map(row => Object.fromEntries($$("[data-field]", row).map(input => [input.dataset.field, input.type === "checkbox" ? input.checked : input.value])))
    });
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
      button.disabled = disabled;
    });
  }
  $$('[data-action-form]').forEach(form => form.addEventListener("submit", async event => {
    event.preventDefault();
    const button = $('button[type="submit"]', form);
    if (button.disabled) return;
    button.disabled = true;
    clearFormError(form);
    try {
      const plan = await api("plans", actionFrom(form));
      clearFormError(form);
      currentPlan = plan; submitKey = crypto.randomUUID(); preview(plan);
      $("#confirm-plan").checked = false; $("#submit-plan").disabled = true; $("#submit-status").textContent = "";
      closeDrawers(true);
      $("#plan-dialog").showModal();
    } catch (error) {
      // The drawer-scoped error is never suppressed by a dismissed toast and
      // survives until the user retries or closes the drawer.
      showFormError(form, error.message);
      notify("error", error.message, "action", { persist: false });
    }
    finally { syncWriteButtons(); }
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
  $("#storage-kind").addEventListener("change", storageFields);
  $("#notebook-kind").addEventListener("change", notebookFields);
  $("#profile-kind").addEventListener("change", profileFields);
  $("#storage-template").addEventListener("change", () => { const option = $("#storage-template").selectedOptions[0]; if (option?.dataset.class) $("#storage-class").value = option.dataset.class; storageFields(); });
  $("#pv-selector").addEventListener("change", () => { const volume = (inventory?.resources || []).find(resource => resource.kind === "PersistentVolume" && resource.name === $("#pv-selector").value); if (volume) $("#storage-class").value = volume.details?.storageClass || ""; });
  $('#notebook-form [name="namespace"]').addEventListener("change", () => { notebookOptions(); $("#mount-rows").replaceChildren(); $("#replace-mounts").checked = false; });
  $("#notebook-selector").addEventListener("change", containerOptions);
  $("#add-mount").addEventListener("click", addMount);
  $("#replace-mounts").addEventListener("change", mountFields);
  $('#replace-mounts').closest('fieldset').querySelector('.hint').textContent = "Unchecked preserves current mounts. Checked with no rows requests removal of all PVC mounts from this container; the server rejects removing protected workspace mounts.";

  activatePage();
  if (!notebookWrites) notify("warning", "Notebook changes are disabled: the installed controller contract has not been confirmed by the operator.", "policy:notebook-controller");
  if (!meta("pod-restarts").startsWith("true")) notify("warning", "Pod restarts are disabled by server policy. Restart controls are unavailable until the feature and its optional permission are enabled.", "policy:pod-restarts");
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) return;
    ensureActiveHistory();
    refresh(false);
  });
  const quotaKeys = new Set(); $$('[data-gpu-quota]').forEach(label => { if (quotaKeys.has(label.dataset.gpuQuota)) label.remove(); else quotaKeys.add(label.dataset.gpuQuota); });
  storageFields(); notebookFields(); profileFields(); syncWriteButtons();
  refresh(false);
  setInterval(updateTimestamp, 10000);
  setInterval(() => refresh(false), 30000);
})();
