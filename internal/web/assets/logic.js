"use strict";
// Shared pure presentation/payload logic, executable in local Node tests and in
// the browser. No network access, credentials or Kubernetes manifests live here.
((root) => {
  const kinds = { "profile.quota": "profile.quota.change", "pvc.create": "pvc.create.dynamic", "pv.nfs.create": "storage.create.static", "pv.reserve": "pv.reserve-and-claim" };
  function buildAction(entries, options = {}) {
    const action = {}, quota = {};
    for (const [name, value] of entries) {
      if (value === "" || name === "restart") continue;
      if (name.startsWith("quota.")) quota[name.slice(6)] = value;
      else action[name] = name === "gpuCount" ? Number(value) : value;
    }
    action.kind = kinds[action.kind] || action.kind;
    if (Object.keys(quota).length) action.quota = quota;
    if (action.kind === "profile.quota.change") delete action.owner;
    if (action.kind === "notebook.edit") {
      action.restart = !!options.restart;
      if (options.replaceMounts) action.mounts = options.mounts || [];
    }
    if (action.kind === "pod.restart") {
      action.reason = String(action.reason || "").trim();
      delete action.owner;
    }
    if (action.kind === "node.cordon" || action.kind === "node.uncordon") {
      // Node actions are cluster-scoped and carry an explicit force bit.
      // FormData materializes a checked checkbox as "on"; normalize it.
      const formForce = action.force === true || action.force === "true" || action.force === "on";
      action.reason = String(action.reason || "").trim();
      action.force = !!options.force || formForce;
      delete action.namespace;
      delete action.owner;
    }
    return action;
  }
  function scopedResources(resources, namespace) {
    const volumes = new Set(resources.filter(r => r.kind === "PersistentVolumeClaim" && r.namespace === namespace).map(r => r.details?.volume));
    return resources.filter(r => !namespace || r.namespace === namespace ||
      (["Profile", "Namespace"].includes(r.kind) && r.name === namespace) || r.kind === "StorageClass" ||
      (r.kind === "PersistentVolume" && (volumes.has(r.name) || r.details?.claim?.startsWith(namespace + "/") || (r.state === "Available" && !r.details?.claim))));
  }
  function containerNames(resource) {
    return Object.keys(resource?.details || {}).filter(k => k.startsWith("image.") && k.length > 6).map(k => k.slice(6)).sort();
  }
  const actionLabels = {
    kind: "Change", namespace: "Namespace", name: "Name", reason: "Reason", owner: "Owner identity", quota: "Quota targets",
    storageClass: "Storage class", size: "Requested capacity", accessMode: "Access mode", pvName: "PV name", template: "Approved template", subdirectory: "Relative NFS subdirectory",
    container: "Named container", image: "Image", cpuRequest: "CPU request", cpuLimit: "CPU limit", memoryRequest: "Memory request", memoryLimit: "Memory limit",
    gpuPreset: "GPU preset", gpuCount: "GPU count", mounts: "Replacement PVC mounts", restart: "Restart after edit",
    force: "Force"
  };
  function previewFields(plan) {
    const p = plan.prepared, action = p.action;
    const fields = [["Plan ID", plan.id], ["Actor", actorLabel(plan)], ["Created", plan.createdAt], ["Expires", plan.expiresAt], ["Summary", p.summary], ["Target UID", p.targetUID], ["Resource version", p.resourceVersion]];
    if (p.parentNotebook) fields.push(["Parent Notebook", p.parentNotebook]);
    // Only typed server-owned action fields are a change preview. Prepared.Data
    // is internal revalidation evidence, not a diff and never rendered here.
    for (const [key, label] of Object.entries(actionLabels)) {
      if (Object.hasOwn(action, key)) fields.push([label, action[key]]);
    }
    if (action.kind === "notebook.edit") {
      fields.push(["Unspecified resource fields", "Preserved; blank image, CPU and memory fields do not remove current settings."]);
      if (!Object.hasOwn(action, "gpuCount")) fields.push(["GPU count", "Unchanged (not specified)"]);
      if (!Object.hasOwn(action, "gpuPreset")) fields.push(["GPU placement preset", "Not specified; existing ownership is validated if the GPU count changes."]);
      if (!Object.hasOwn(action, "mounts")) fields.push(["PVC mounts", "Unchanged (replacement not requested)"]);
      else if (action.mounts.length === 0) fields.push(["PVC mount removal", "Remove all PVC mounts from this container; protected mounts cannot be removed."]);
      fields.push(["Edit lifecycle", action.restart ? "Stop before editing; restart only if the notebook was previously running. An already stopped notebook stays stopped." : "Stop before editing and leave the notebook stopped. Start separately when ready."]);
    }
    if (action.kind === "pod.restart") {
      fields.push(["Restart method", "The server revalidates the live Pod UID and owner chain, then requests a disruption-budget-aware eviction. Only supported Notebook StatefulSet-managed Pods are accepted; unmanaged, static, mirror and terminating Pods are rejected."]);
      fields.push(["Force deletion", "Never used automatically. If eviction is rejected before mutation the server reports failure; ambiguous interruption remains needs-review."]);
    }
    if (action.kind === "node.cordon") {
      fields.push(["Cordon effect", "Sets spec.unschedulable=true on the Node (the same effect as kubectl cordon) and adds SchedulingDisabled alongside the current readiness state. New Pods will not be scheduled onto this node; existing running Pods are not evicted."]);
    }
    if (action.kind === "node.uncordon") {
      fields.push(["Uncordon effect", "Sets spec.unschedulable=false on the Node (the same effect as kubectl uncordon) and removes SchedulingDisabled so new Pods can be scheduled onto this node again."]);
    }
    return fields;
  }

  const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });
  const binary = { Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4, Pi: 1024 ** 5, Ei: 1024 ** 6 };
  const decimal = { n: 1e-9, u: 1e-6, m: 1e-3, "": 1, k: 1e3, K: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18 };
  const missingText = new Set(["", "—", "n/a", "na", "not reported", "not available"]);
  function isMissing(value) {
    if (value === null || value === undefined) return true;
    return missingText.has(String(value).trim().toLowerCase());
  }

  // Kubernetes quantities are not decimal text. CPU "500m" is 0.5 cores and
  // "1Gi" is 1024^3 bytes, so lexicographic comparison would be wrong.
  function quantityValue(value) {
    if (typeof value === "number") return Number.isFinite(value) ? value : null;
    if (isMissing(value)) return null;
    const text = String(value ?? "").trim();
    const match = /^([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)([A-Za-z]*)$/.exec(text);
    if (!match) return null;
    const number = Number(match[1]);
    if (!Number.isFinite(number)) return null;
    const suffix = match[2];
    if (Object.hasOwn(binary, suffix)) return number * binary[suffix];
    if (Object.hasOwn(decimal, suffix)) return number * decimal[suffix];
    return null;
  }
  function timeValue(value) {
    if (value instanceof Date) return Number.isFinite(value.getTime()) ? value.getTime() : null;
    if (typeof value === "number") return Number.isFinite(value) ? value : null;
    if (isMissing(value)) return null;
    const text = String(value ?? "").trim();
    const parsed = Date.parse(text);
    return Number.isFinite(parsed) ? parsed : null;
  }
  function severityValue(value) {
    if (isMissing(value)) return null;
    const text = String(value).trim().toLowerCase();
    // Negative compound states ("Not ready", "Not active", "Unavailable",
    // "Incomplete", "Unhealthy") must not match positive substrings inside them.
    if (/fail|error|review|denied|conflict|unavailable|invalid|unknown|incomplete|unhealthy|lost|not\s*ready|not\s*active|not\s*running|unschedul/.test(text)) return 3;
    if (/schedulingdisabled/.test(text)) return 2;
    if (/stopp|terminat|degrad|warn|expir|suspend/.test(text)) return 2;
    if (/pending|starting|queued|progress|apply|applying|creating|initial|accepted/.test(text)) return 1;
    if (/succeed|complete|ready|running|normal|active|bound|available|healthy|verified|synced/.test(text)) return 0;
    // Unrecognized nonempty states remain neutral rather than claiming health.
    return 2;
  }
  function readyValue(value) {
    if (typeof value === "number") return Number.isFinite(value) ? value : null;
    if (isMissing(value)) return null;
    const text = String(value ?? "").trim().toLowerCase();
    const ratio = /^(\d+)\s*\/\s*(\d+)(?:\s+ready)?$/.exec(text);
    if (ratio) {
      const ready = Number(ratio[1]), total = Number(ratio[2]);
      if (!Number.isFinite(ready) || !Number.isFinite(total) || total <= 0) return 0;
      return ready / total;
    }
    if (/^(true|ready|running|yes)$/.test(text)) return 1;
    if (/^(false|not ready|no|stopped)$/.test(text)) return 0;
    return null;
  }
  function naturalCompare(a, b) {
    return collator.compare(String(a ?? ""), String(b ?? ""));
  }
  function sortValue(row, key, type) {
    const raw = row && row.sort && Object.hasOwn(row.sort, key) ? row.sort[key] : row?.[key];
    if (type === "quantity") return quantityValue(raw);
    if (type === "number") { const n = Number(raw); return isMissing(raw) || !Number.isFinite(n) ? null : n; }
    if (type === "time") return timeValue(raw);
    if (type === "severity") return severityValue(raw);
    if (type === "ready") return readyValue(raw);
    if (type === "mixed") return isMissing(raw) ? null : String(raw).trim();
    const text = String(raw ?? "").trim();
    return isMissing(text) ? null : text;
  }
  // Stable typed sort. Missing values always sort last, regardless of direction,
  // and equal values retain their loaded order.
  function sortRows(rows, key, direction = "asc", type = "text") {
    const descending = direction === "desc";
    return rows.map((row, index) => ({ row, index, value: sortValue(row, key, type) }))
      .sort((a, b) => {
        if (a.value === null && b.value === null) return a.index - b.index;
        if (a.value === null) return 1;
        if (b.value === null) return -1;
        let compared;
        if (type === "text") compared = naturalCompare(a.value, b.value);
        else if (type === "mixed") {
          const quantityA = quantityValue(a.value), quantityB = quantityValue(b.value);
          compared = quantityA !== null && quantityB !== null
            ? (quantityA < quantityB ? -1 : quantityA > quantityB ? 1 : 0)
            : naturalCompare(a.value, b.value);
        } else compared = a.value < b.value ? -1 : a.value > b.value ? 1 : 0;
        if (descending) compared = -compared;
        return compared || a.index - b.index;
      })
      .map(item => item.row);
  }
  function searchMatch(text, query) {
    const terms = String(query ?? "").toLowerCase().trim().split(/\s+/).filter(Boolean);
    if (!terms.length) return true;
    const haystack = String(text ?? "").toLowerCase();
    return terms.every(term => haystack.includes(term));
  }
  function filterRows(rows, query) {
    return rows.filter(row => searchMatch(row.search ?? row, query));
  }
  function paginate(items, page = 1, perPage = 50) {
    const size = Math.max(1, Number(perPage) || 50);
    const pages = Math.max(1, Math.ceil(items.length / size));
    const current = Math.min(Math.max(1, Number(page) || 1), pages);
    return { items: items.slice((current - 1) * size, current * size), page: current, pages, total: items.length, perPage: size };
  }

  // Shared target grammar: kind:namespace/name and profile:name.
  function parseTarget(target) {
    const raw = String(target ?? "").trim();
    if (!raw) return { kind: "", namespace: "", name: "" };
    const colon = raw.indexOf(":");
    if (colon < 0) return { kind: "", namespace: "", name: raw };
    const kind = raw.slice(0, colon).trim();
    const rest = raw.slice(colon + 1).trim();
    const slash = rest.indexOf("/");
    if (slash < 0) return { kind, namespace: "", name: rest };
    return { kind, namespace: rest.slice(0, slash), name: rest.slice(slash + 1) };
  }
  function safeActorText(value, limit) {
    return String(value ?? "").replace(/[\u0000-\u001f\u007f]/g, "").trim().slice(0, limit);
  }
  function actorLabel(record) {
    const display = safeActorText(record?.actorDisplay, 160);
    if (display) return display;
    // Legacy records never expose the encoded issuer|subject key. The console
    // shows one readable label and an honest fallback for older audit rows.
    return "Legacy administrator";
  }
  function groupAudit(rows) {
    const groups = new Map();
    for (const row of Array.isArray(rows) ? rows : []) {
      const operationID = String(row.operationID ?? "").trim();
      const key = operationID ? "operation:" + operationID : "audit:" + String(row.id ?? "");
      if (!groups.has(key)) groups.set(key, { id: key, operationID, stages: [] });
      groups.get(key).stages.push(row);
    }
    return [...groups.values()].map(group => {
      const finalOutcome = result => /succeed|fail|review|complete|lost|error/.test(String(result ?? "").toLowerCase()) ? 1 : 0;
      // At an identical timestamp a terminal stage must outrank an earlier
      // queued/applying checkpoint regardless of incoming array order.
      const stages = group.stages.slice().sort((a, b) => ((timeValue(b.time) ?? 0) - (timeValue(a.time) ?? 0)) || (finalOutcome(b.result) - finalOutcome(a.result)));
      const latest = stages[0] || {};
      const actorRecord = stages.find(stage => String(stage.actorDisplay ?? "").trim()) || latest;
      const target = stages.map(stage => parseTarget(stage.target)).find(part => part.kind || part.namespace || part.name) || { kind: "", namespace: "", name: "" };
      const actions = [...new Set(stages.map(stage => String(stage.action ?? "").trim()).filter(Boolean))];
      return {
        id: group.id, operationID: group.operationID, stages,
        time: latest.time, actor: latest.actor, actorDisplay: actorRecord.actorDisplay,
        actorLabel: actorLabel(actorRecord),
        kind: target.kind, namespace: target.namespace, name: target.name,
        action: actions.join(" · "),
        // Summary shows the latest observed stage outcome. Earlier acceptance
        // stages may still say "queued" after the operation has succeeded.
        result: String(latest.result ?? ""),
        // Actual administrator input reason only; operation IDs are separate.
        reason: stages.map(stage => String(stage.reason ?? "").trim()).find(Boolean) || ""
      };
    });
  }
  function namespaceOptions(resources) {
    const names = new Set();
    for (const resource of Array.isArray(resources) ? resources : []) {
      if (resource.kind === "Profile" || resource.kind === "Namespace") { if (resource.name) names.add(resource.name); }
      else if (resource.namespace) names.add(resource.namespace);
    }
    return [...names].sort(naturalCompare);
  }
  function profileNames(resources) {
    return new Set((Array.isArray(resources) ? resources : []).filter(r => r.kind === "Profile" && r.name).map(r => r.name));
  }
  function profileSummary(resources, profile) {
    const list = Array.isArray(resources) ? resources : [];
    const defs = [
      { kind: "Notebook", label: "Notebooks", href: "/notebooks" },
      { kind: "Pod", label: "Pods", href: "/pods" },
      { kind: "PersistentVolumeClaim", label: "Persistent volume claims", href: "/storage" }
    ];
    return defs.map(def => ({ ...def, count: list.filter(resource => resource.kind === def.kind && resource.namespace === profile).length }))
      .filter(entry => entry.count > 0);
  }
  function profileResources(resources) {
    // Profiles are cluster-scoped: always show every observed Profile, even
    // when a namespace filter is active on another page.
    return (Array.isArray(resources) ? resources : []).filter(resource => resource.kind === "Profile");
  }
  const namespaceResourceDefs = [
    { kind: "Notebook", label: "Notebooks", href: "/notebooks" },
    { kind: "Pod", label: "Pods", href: "/pods" },
    { kind: "PersistentVolumeClaim", label: "Persistent volume claims", href: "/storage" },
    { kind: "StatefulSet", label: "StatefulSets", href: "/notebooks" },
    { kind: "ResourceQuota", label: "Resource quotas", href: "/" },
    { kind: "LimitRange", label: "Limit ranges", href: "" },
    { kind: "RoleBinding", label: "Role bindings", href: "" },
    { kind: "Event", label: "Events", href: "/" }
  ];
  function namespaceResources(resources, name) {
    const target = String(name ?? "").trim();
    if (!target) return [];
    const list = Array.isArray(resources) ? resources : [];
    // Only resources actually observed inside this namespace. The Namespace
    // object itself is the summary target, not one of its contents.
    return namespaceResourceDefs.map(def => {
      const items = list.filter(resource => resource.kind === def.kind && resource.kind !== "Namespace" && resource.namespace === target);
      return { kind: def.kind, label: def.label, count: items.length, href: def.href, items };
    }).filter(group => group.count > 0);
  }
  function formatAge(createdAt, nowMs) {
    const created = timeValue(createdAt);
    const now = nowMs === undefined ? Date.now() : Number(nowMs);
    if (created === null || !Number.isFinite(now)) return "—";
    const seconds = Math.max(0, Math.floor((now - created) / 1000));
    if (seconds < 60) return seconds + "s";
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return minutes + "m";
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return hours + "h";
    return Math.floor(hours / 24) + "d";
  }
  function restartTotal(resource) {
    const details = resource?.details || {};
    if (details.restarts !== undefined && details.restarts !== "") {
      const count = Number(details.restarts);
      return Number.isFinite(count) ? count : 0;
    }
    return Object.keys(details).filter(key => key.startsWith("restarts."))
      .reduce((sum, key) => sum + (Number(details[key]) || 0), 0);
  }
  function stateToneCount(defs, classify, kind, scoped) {
    const resources = scoped.filter(resource => resource.kind === kind);
    const counts = Object.fromEntries(defs.map(def => [def.label, 0]));
    for (const resource of resources) counts[classify(resource)]++;
    return { total: resources.length, states: defs.map(def => ({ label: def.label, count: counts[def.label], tone: def.tone })) };
  }
  // Dashboard summary for the active namespace scope. Every resource falls into
  // exactly one displayed state so the card counts always add up to the total.
  function workspaceHealthSummary(resources, namespace) {
    const all = Array.isArray(resources) ? resources : [];
    const scoped = scopedResources(all, namespace);
    const healthText = resource => String(resource?.state ?? "").trim().toLowerCase();
    return {
      Notebooks: stateToneCount([
        { label: "Running", tone: "good" }, { label: "Stopped", tone: "warn" },
        { label: "Transitioning", tone: "warn" }, { label: "Errors", tone: "bad" }
      ], resource => {
        const text = healthText(resource), rank = severityValue(text);
        if (rank === 3) return "Errors";
        if (/stopp|terminat/.test(text)) return "Stopped";
        if (rank === 1 || /pending|starting|creating|progress|applying|queued|initial|transition/.test(text)) return "Transitioning";
        return rank === 0 ? "Running" : "Transitioning";
      }, "Notebook", scoped),
      Pods: stateToneCount([
        { label: "Running", tone: "good" }, { label: "Warning", tone: "warn" }, { label: "Errors", tone: "bad" }
      ], resource => {
        const text = healthText(resource), ready = String(resource?.details?.ready ?? "").trim().toLowerCase();
        if (severityValue(text) === 3 || /failed|crashloop|error|unknown|evicted/.test(text)) return "Errors";
        if (/pending|containercreating|creating|terminating/.test(text) || (text === "running" && ready === "false") || restartTotal(resource) > 0) return "Warning";
        return text === "running" ? "Running" : "Warning";
      }, "Pod", scoped),
      Nodes: stateToneCount([
        { label: "Ready", tone: "good" }, { label: "Cordoned", tone: "warn" }, { label: "NotReady", tone: "bad" }
      ], resource => {
        const text = healthText(resource);
        if (/notready|unknown/.test(text) || severityValue(text) === 3) return "NotReady";
        if (/schedulingdisabled/.test(text)) return "Cordoned";
        return text === "ready" ? "Ready" : "NotReady";
      }, "Node", all),
      PersistentVolumeClaims: stateToneCount([
        { label: "Bound", tone: "good" }, { label: "Pending", tone: "warn" }, { label: "Errors", tone: "bad" }
      ], resource => {
        const text = healthText(resource), rank = severityValue(text);
        if (/bound/.test(text)) return "Bound";
        if (rank === 1 || /pending|waiting|creating/.test(text)) return "Pending";
        return "Errors";
      }, "PersistentVolumeClaim", scoped),
      PersistentVolumes: stateToneCount([
        { label: "Bound", tone: "good" }, { label: "Available", tone: "good" }, { label: "Other / Error", tone: "bad" }
      ], resource => {
        const text = healthText(resource);
        if (/bound/.test(text)) return "Bound";
        return text === "available" ? "Available" : "Other / Error";
      }, "PersistentVolume", scoped),
      Events: stateToneCount([
        { label: "Normal", tone: "good" }, { label: "Warning", tone: "warn" }
      ], resource => {
        const text = healthText(resource);
        return /warn|fail|error/.test(text) || severityValue(text) >= 2 ? "Warning" : "Normal";
      }, "Event", scoped)
    };
  }

  function routeFor(pathname, search) {
    const path = String(pathname || "/");
    if (path === "/operations") return { page: "history", tab: "operations" };
    if (path === "/audit") return { page: "history", tab: "audit" };
    if (path === "/history") {
      let tab = "";
      try { tab = new URLSearchParams(String(search || "")).get("tab") || ""; } catch { tab = ""; }
      return { page: "history", tab: tab === "audit" ? "audit" : "operations" };
    }
    const clean = path.replace(/^\/+|\/+$/g, "");
    if (!clean) return { page: "overview", tab: null };
    if (["overview", "profiles", "namespaces", "storage", "notebooks", "pods", "nodes"].includes(clean)) return { page: clean, tab: null };
    return { page: "overview", tab: null };
  }
  function queryNamespace(search) {
    try { return new URLSearchParams(String(search || "")).get("namespace") || ""; } catch { return ""; }
  }
  function hrefFor(path, namespace) {
    const url = new URL(String(path || "/"), "https://console.invalid");
    if (namespace) url.searchParams.set("namespace", namespace);
    return url.pathname + url.search;
  }
  function historyHref(tab, namespace) {
    const url = new URL("/history", "https://console.invalid");
    url.searchParams.set("tab", tab === "audit" ? "audit" : "operations");
    if (namespace) url.searchParams.set("namespace", namespace);
    return url.pathname + url.search;
  }
  function needsRefresh(observedAt, now, maxAge = 30000) {
    const seen = timeValue(observedAt);
    return seen === null || !Number.isFinite(Number(now)) || Number(now) - seen >= maxAge;
  }
  function notificationID(level, code, message) {
    const input = [level, code, message].join("\u0000");
    let hash = 0x811c9dc5;
    for (let index = 0; index < input.length; index++) {
      hash ^= input.charCodeAt(index);
      hash = Math.imul(hash, 0x01000193) >>> 0;
    }
    return "notice-" + hash.toString(16).padStart(8, "0");
  }
  function shouldRememberDismissals(scope) {
    return typeof scope === "string" && scope.trim().length > 0;
  }
  function dismissalStorageKey(scope) {
    return "kf-admin:notifications:v1:" + String(scope || "").trim();
  }
  function defaultTableState(sortKey = "name", sortDir = "asc") {
    return { search: "", sortKey, sortDir: sortDir === "desc" ? "desc" : "asc", page: 1 };
  }
  function nextSort(state, key) {
    const descending = state.sortKey === key && state.sortDir === "asc";
    return { ...state, sortKey: key, sortDir: descending ? "desc" : "asc", page: 1 };
  }
  function withPage(state, page) {
    return { ...state, page: Math.max(1, Number(page) || 1) };
  }
  function viewRows(rows, state, perPage = 50) {
    const filtered = filterRows(rows || [], state?.search || "");
    const sorted = sortRows(filtered, state?.sortKey || "name", state?.sortDir || "asc", state?.type || "text");
    return paginate(sorted, state?.page || 1, perPage);
  }

  const logic = {
    buildAction, scopedResources, containerNames, previewFields,
    quantityValue, timeValue, severityValue, readyValue, naturalCompare, isMissing,
    sortRows, searchMatch, filterRows, paginate,
    parseTarget, actorLabel, groupAudit,
    namespaceOptions, profileNames, profileSummary, profileResources,
    namespaceResources, formatAge, workspaceHealthSummary,
    routeFor, queryNamespace, hrefFor, historyHref, needsRefresh,
    notificationID, shouldRememberDismissals, dismissalStorageKey,
    defaultTableState, nextSort, withPage, viewRows
  };
  if (typeof module !== "undefined" && module.exports) module.exports = logic;
  else root.DashboardLogic = logic;
})(globalThis);
