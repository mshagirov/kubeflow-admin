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
    gpuPreset: "GPU preset", gpuCount: "GPU count", mounts: "Replacement PVC mounts", restart: "Restart after edit"
  };
  function previewFields(plan) {
    const p = plan.prepared, action = p.action;
    const fields = [["Plan ID", plan.id], ["Actor", plan.actorDisplay || plan.actor], ["Created", plan.createdAt], ["Expires", plan.expiresAt], ["Summary", p.summary], ["Target UID", p.targetUID], ["Resource version", p.resourceVersion]];
    if (plan.actorDisplay && plan.actor && plan.actorDisplay !== plan.actor) fields.push(["Actor ID", plan.actor]);
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
    const actor = String(record?.actor ?? "").trim();
    if (!actor) return "Unknown administrator";
    const pipe = actor.lastIndexOf("|");
    const subject = safeActorText(pipe >= 0 ? actor.slice(pipe + 1) : actor, 160);
    return subject || safeActorText(actor, 160) || "Unknown administrator";
  }
  function actorID(record) {
    return safeActorText(record?.actor, 320);
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
        actorLabel: actorLabel(actorRecord), actorID: actorID(actorRecord),
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
  // Profile association follows the Profile-name/namespace-name convention and
  // only counts resources actually observed by the integration.
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
  function profileResources(resources, namespace) {
    const scoped = scopedResources(resources, namespace);
    const profiles = profileNames(resources);
    // Profiles is a Profile/Namespace browser, not a governance-resource dump.
    return scoped.filter(resource => resource.kind === "Profile" ||
      (resource.kind === "Namespace" && profiles.has(resource.name)));
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
    if (["overview", "profiles", "storage", "notebooks", "pods"].includes(clean)) return { page: clean, tab: null };
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
    parseTarget, actorLabel, actorID, groupAudit,
    namespaceOptions, profileNames, profileSummary, profileResources,
    routeFor, queryNamespace, hrefFor, historyHref, needsRefresh,
    notificationID, shouldRememberDismissals, dismissalStorageKey,
    defaultTableState, nextSort, withPage, viewRows
  };
  if (typeof module !== "undefined" && module.exports) module.exports = logic;
  else root.DashboardLogic = logic;
})(globalThis);
