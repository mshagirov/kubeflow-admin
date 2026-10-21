"use strict";
// Shared pure presentation/payload logic, executable in local Node tests and in
// the browser. No network access, credentials or Kubernetes manifests live here.
((root) => {
  const kinds = { "profile.quota": "profile.quota.change", "pvc.create": "pvc.create.dynamic", "pv.nfs.create": "storage.create.static", "pv.reserve": "pv.reserve-and-claim" };
  // A profile action carries only the fields its kind defines. Anything else in
  // the form (storage settings, a resource version, a stale confirmation) is
  // dropped before the request leaves the browser. A namespace is optional and
  // is only kept when it is consistent with the Profile name.
  // Both quota paths are guarded the same way: the legacy alias keeps its
  // deprecated kind, but it no longer bypasses the owner and precondition
  // guard that profile.update requires.
  const guardedProfileKinds = new Set(["profile.update", "profile.quota.change"]);
  const profileActionFields = {
    "profile.update": ["kind", "name", "namespace", "owner", "quota", "baseConfigDigest", "reason"],
    "profile.quota.change": ["kind", "name", "namespace", "owner", "quota", "baseConfigDigest", "reason"],
    "profile.delete": ["kind", "name", "reason", "confirmName"]
  };
  function keepProfileFields(action) {
    const allowed = profileActionFields[action.kind];
    if (!allowed) return action;
    for (const key of Object.keys(action)) if (!allowed.includes(key)) delete action[key];
    if (action.namespace && action.namespace !== action.name) delete action.namespace;
    return action;
  }
  function buildAction(entries, options = {}) {
    const settings = options || {};
    const action = {}, quota = {};
    for (const [name, value] of entries || []) {
      if (value === "" || value === null || value === undefined || name === "restart") continue;
      if (name.startsWith("quota.")) {
        // An empty quota key is never a quota entry.
        const key = name.slice(6);
        if (!key) continue;
        quota[key] = value;
      }
      else action[name] = name === "gpuCount" ? Number(value) : value;
    }
    action.kind = kinds[action.kind] || action.kind;
    if (Object.keys(quota).length) action.quota = quota;
    if (action.kind === "profile.quota.change" || action.kind === "profile.update") {
      // The owner is read-only: the installed Profile controller cannot
      // transfer it, so the current owner is sent back exactly as it was
      // observed, never trimmed or rewritten. The configuration precondition
      // travels with the request and is never rendered. The legacy alias
      // keeps its deprecated kind but is guarded exactly like an update; a
      // caller that omits the guard is encoded as it is and the server
      // refuses it.
      action.owner = String(action.owner ?? "");
      action.baseConfigDigest = String(action.baseConfigDigest ?? "");
      if (!action.owner) delete action.owner;
      if (!action.baseConfigDigest) delete action.baseConfigDigest;
    }
    if (action.kind === "profile.delete") {
      // Deletion is confirmed by an exact name match plus a mandatory reason.
      // The confirmation is sent exactly as typed: it is never trimmed or
      // otherwise normalized, so a padded or differently cased name is refused
      // instead of silently corrected.
      action.reason = String(action.reason || "").trim();
      action.confirmName = String(action.confirmName ?? "");
      delete action.owner;
      delete action.quota;
      delete action.baseConfigDigest;
    }
    if (action.kind === "notebook.edit") {
      action.restart = !!settings.restart;
      if (settings.replaceMounts) action.mounts = settings.mounts || [];
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
      action.force = !!settings.force || formForce;
      delete action.namespace;
      delete action.owner;
    }
    return keepProfileFields(action);
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
    force: "Force", confirmName: "Confirmation name"
  };
  function previewFields(plan) {
    const p = plan.prepared, action = p.action;
    const fields = [["Plan ID", plan.id], ["Actor", actorLabel(plan)], ["Created", plan.createdAt], ["Expires", plan.expiresAt], ["Summary", p.summary], ["Target UID", p.targetUID], ["Resource version", p.resourceVersion]];
    if (p.parentNotebook) fields.push(["Parent Notebook", p.parentNotebook]);
    // Only typed server-owned action fields are a change preview. Prepared.Data
    // is internal revalidation evidence, not a diff and never rendered here.
    // The configuration precondition is a request guard, not a preview field:
    // it has no label, so its raw hash never reaches the console.
    for (const [key, label] of Object.entries(actionLabels)) {
      // The guarded Profile previews state the owner as a read-only value.
      if (key === "owner" && guardedProfileKinds.has(action.kind)) continue;
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
    if (guardedProfileKinds.has(action.kind)) {
      const owner = Object.hasOwn(action, "owner") ? `${action.owner}. ` : "";
      fields.push(["Current owner (read-only)", `${owner}Owner transfer is unsupported by the installed Profile controller; the current owner is read-only. Only quotas are changed.`]);
      fields.push(["Quota keys", "Only the listed quota keys are sent. Supply every existing quota key with its current value. Removing a quota key is unsupported."]);
    }
    if (action.kind === "profile.delete") {
      fields.push(["Deletion request", "Requests the deletion of the Profile itself, guarded by its UID and resource version. The console never deletes the namespace directly and never removes a finalizer to force a deletion."]);
      fields.push(["Namespace cleanup", "The Profile controller and namespace garbage collection remove the namespace once its ownership is verified. Cleanup cannot skip namespaced objects: everything inside the namespace goes with it, including objects the Profile does not own."]);
      fields.push(["Deletion result", "The operation succeeds only after the Profile and its namespace are both absent. A namespace that stays in deletion with an unresolved finalizer is left for the operator and is never reported as a completed deletion."]);
      fields.push(["Storage and data", "The namespace cleanup deletes the volume claims inside it. A Delete reclaim policy lets the storage controller delete the backing volume, which can be irreversible. A Retain reclaim policy is not protection: the backend lifecycle or a later operator delete can still remove the volume."]);
      fields.push(["Snapshots and external data", "A namespaced VolumeSnapshot is deleted with its namespace, and the snapshot content behind it follows its own Delete or Retain policy. Retained volumes, volume snapshots and external storage such as NFS exports or object storage can keep their data. The console never wipes backend data itself."]);
      fields.push(["Confirmation", `The confirmation name must match the profile name "${action.name}" character for character. The console sends it exactly as typed and never trims it.`]);
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
  // The Notebooks page lists the kubeflow.org Notebook custom resource only.
  // Pods and StatefulSets are owned workloads with their own views; they are
  // never mixed into the Notebook table under the same name.
  function notebookResources(resources) {
    return (Array.isArray(resources) ? resources : []).filter(resource => resource.kind === "Notebook");
  }
  // The safe, consolidated facts of one Profile. The internal configuration
  // precondition is excluded by visibleResourceDetails and is never part of
  // the summary; the owner and quota are each projected exactly once.
  function profileSummaryDetails(resource) {
    const details = visibleResourceDetails(resource);
    return {
      kind: String(resource?.kind || "Profile"),
      name: String(resource?.name || ""),
      apiVersion: String(details.apiVersion ?? ""),
      owner: String(resource?.owner ?? "") || String(details.owner ?? ""),
      ownerKind: String(details.ownerKind ?? ""),
      state: String(resource?.state ?? ""),
      scope: resource?.namespace ? String(resource.namespace) : "Cluster scoped",
      createdAt: String(details.createdAt ?? ""),
      resourceVersion: String(details.resourceVersion ?? ""),
      quota: profileQuotaValues(resource)
    };
  }

  // ---- Profile naming, form values and deletion confirmation --------------
  // A Profile name is also its namespace name, so it must be a Kubernetes
  // DNS-1123 label: at most 63 characters of lowercase alphanumerics and
  // hyphens, starting and ending with an alphanumeric.
  const profileNameMaxLength = 63;
  const profileNamePattern = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
  // Names already taken by a Profile or by a namespace. An existing namespace
  // cannot be adopted by a new Profile, so it reserves the name as well.
  function profileExistingNames(existing) {
    const names = new Set();
    const items = existing instanceof Set ? [...existing] : Array.isArray(existing) ? existing : existing ? [existing] : [];
    for (const item of items) {
      if (typeof item === "string") { if (item) names.add(item); continue; }
      if (!item || typeof item !== "object") continue;
      if ((!item.kind || item.kind === "Profile" || item.kind === "Namespace") && item.name) names.add(item.name);
    }
    return names;
  }
  // Dots and underscores are the only replacements offered. The typed name is
  // never rewritten by this helper; a replacement is a suggestion that the
  // administrator must apply explicitly.
  function profileNameSuggestion(text) {
    return String(text ?? "").replace(/[._]+/g, "-");
  }
  // profileNameValidation(name, existing, options) -> { name, valid, message,
  // suggestion, duplicate }. "existing" accepts profile names, a Set of names
  // or observed resources. options.allowExisting keeps an already used name
  // valid, which an update or a deletion of that same profile needs.
  function profileNameValidation(name, existing, options = {}) {
    const settings = options || {};
    const text = String(name ?? "");
    const taken = profileExistingNames(existing);
    const result = { name: text, valid: false, message: "", suggestion: "", duplicate: false };
    if (!text.trim()) { result.message = "Profile name is required."; return result; }
    if (text.length > profileNameMaxLength) {
      result.message = `Profile names are limited to ${profileNameMaxLength} characters.`;
      return result;
    }
    const hasDot = text.includes(".");
    const hasUnderscore = text.includes("_");
    const hasUpper = /[A-Z]/.test(text);
    const hasSpace = /\s/.test(text);
    const hasOther = /[^a-z0-9._\-\s]/.test(text);
    const edgeHyphen = text.startsWith("-") || text.endsWith("-");
    if (hasDot || hasUnderscore) {
      result.message = hasUpper
        ? "Profile names cannot contain dots or underscores; use hyphens and lowercase letters instead."
        : "Profile names cannot contain dots or underscores; use hyphens instead.";
    } else if (hasUpper) {
      result.message = "Profile names must be lowercase.";
    } else if (hasSpace) {
      result.message = "Profile names cannot contain spaces.";
    } else if (hasOther) {
      result.message = "Profile names allow only lowercase letters, digits and hyphens.";
    } else if (edgeHyphen) {
      result.message = "Profile names must start and end with a lowercase letter or digit.";
    }
    // A suggestion is offered only when it is itself a valid, unused label.
    const suggestion = (hasDot || hasUnderscore) ? profileNameSuggestion(text) : "";
    if (suggestion && suggestion.length <= profileNameMaxLength && profileNamePattern.test(suggestion) && !taken.has(suggestion)) {
      result.suggestion = suggestion;
    }
    if (result.message) return result;
    if (settings.allowExisting !== true && taken.has(text)) {
      result.duplicate = true;
      result.message = `A Profile named "${text}" already exists; choose a different name.`;
      return result;
    }
    result.valid = true;
    return result;
  }
  // The narrow quota projection of a Profile: details.quota.<resource key>.
  function profileQuotaValues(resource) {
    const details = resource?.details || {};
    const values = {};
    for (const key of Object.keys(details).sort()) {
      if (!key.startsWith("quota.")) continue;
      const value = details[key];
      if (value === undefined || value === null) continue;
      values[key.slice(6)] = String(value);
    }
    return values;
  }
  // The quota keys this console may author without an approved device preset.
  // "cpu" and "memory" are the Kubernetes ResourceQuota aliases of
  // requests.cpu and requests.memory: Kubernetes charges them against the same
  // pod request. A Profile that declares them is edited under its own key
  // names. The key is never renamed and no second limit is added beside it.
  const builtinQuotaKeys = ["requests.cpu", "limits.cpu", "requests.memory", "limits.memory", "requests.storage", "persistentvolumeclaims", "pods", "cpu", "memory"];
  // The quota keys that count whole objects instead of measuring a resource.
  const builtinQuotaCounts = ["pods", "persistentvolumeclaims"];
  // The API groups that never describe an editable device. The cluster's own
  // namespaces describe storage capacity and control-plane resources.
  const reservedQuotaDomains = ["kubernetes.io", "k8s.io"];
  // quotaPrefix reports whether a quota key part carries the canonical
  // "requests." or "limits." prefix of a quota resource name.
  function quotaPrefix(part) { return part.startsWith("requests.") || part.startsWith("limits."); }
  // reservedQuotaDomain reports whether a resource domain belongs to the
  // cluster's own reserved namespaces.
  function reservedQuotaDomain(domain) {
    const name = String(domain || "").toLowerCase();
    return reservedQuotaDomains.some(reserved => name === reserved || name.endsWith("." + reserved));
  }
  // validQuotaDomain validates the domain of a quota key as a DNS-1123
  // subdomain: at most 253 characters, dot-separated labels of 1-63 lowercase
  // alphanumerics and hyphens, each starting and ending with an alphanumeric.
  // The server is authoritative, but a key that cannot be a qualified resource
  // name must never be presented as an editable device.
  function validQuotaDomain(domain) {
    const text = String(domain || "");
    if (!text || text.length > 253) return false;
    return text.split(".").every(label => label.length >= 1 && label.length <= 63 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(label));
  }
  // deviceQuotaKey reports whether a quota key is a raw extended device
  // resource: a domain/resource name whose domain is not a reserved cluster
  // namespace, whose resource part is a plain device name and that carries no
  // canonical quota prefix. A canonical key ("requests.nvidia.com/gpu"), a
  // reserved storage capacity key ("foo.storageclass.storage.k8s.io/
  // requests.storage") and an enforced object count ("count/jobs.batch") are
  // never raw device keys. The server owns the decision; the browser only uses
  // this to label an observed key.
  function deviceQuotaKey(key) {
    const text = String(key || "");
    const cut = text.indexOf("/");
    if (cut <= 0) return false;
    const domain = text.slice(0, cut), name = text.slice(cut + 1);
    if (!name || name.includes("/")) return false;
    // count/<resource>.<group> is an enforced object-count quota, not a device.
    if (domain === "count") return false;
    if (quotaPrefix(domain) || quotaPrefix(name)) return false;
    if (reservedQuotaDomain(domain)) return false;
    if (!validQuotaDomain(domain)) return false;
    if (name.length > 63) return false;
    return /^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$/.test(name);
  }
  // profileQuotaKind classifies an observed quota key: "builtin", "device" or
  // "unmanaged". An unmanaged key is operator/controller managed: it is
  // preserved verbatim and it is never editable.
  function profileQuotaKind(key, presetKeys) {
    if (builtinQuotaKeys.includes(key)) return "builtin";
    if (Array.isArray(presetKeys) && presetKeys.includes(key)) return "device";
    if (deviceQuotaKey(key)) return "device";
    return "unmanaged";
  }
  // profileQuotaCounts reports whether an observed quota key counts whole
  // objects: a fraction of a GPU is not a quota.
  function profileQuotaCounts(key, presetKeys) {
    if (builtinQuotaCounts.includes(key)) return true;
    return profileQuotaKind(key, presetKeys) === "device";
  }
  // isWholeQuotaCountValue accepts every finite, non-negative integer spelling
  // a count field may carry (0, 1.0, 1e2). It never rewrites the value: the
  // raw string is what the server receives and validates. A fraction,
  // negative number, NaN or Infinity is refused before the request.
  function isWholeQuotaCountValue(value) {
    if (value === "" || value === null || value === undefined) return false;
    const number = typeof value === "number" ? value : Number(String(value).trim());
    return Number.isFinite(number) && Number.isInteger(number) && number >= 0;
  }
  // bareDeviceQuotaKeys returns the extended device quota keys the Profile
  // declares without the canonical requests.<device> prefix. Kubernetes
  // normally enforces a device quota only under requests.<device>; a bare key
  // is declared but is not enforced by ResourceQuota admission.
  function bareDeviceQuotaKeys(quota) {
    return Object.keys(quota || {}).filter(key => deviceQuotaKey(key)).sort();
  }
  // profileDeviceQuotaWarning explains an existing bare device key. It never
  // claims that editing renames the key or activates enforcement, and it never
  // offers an automatic migration: that requires explicit authorization.
  function profileDeviceQuotaWarning(quota) {
    const keys = bareDeviceQuotaKeys(quota);
    if (!keys.length) return "";
    return `This Profile declares the bare extended device quota ${keys.join(", ")}. Standard Kubernetes charges devices under requests.<device>; a bare key is declared but is normally not enforced by ResourceQuota admission. Editing its value here does not rename the key and does not activate enforcement. Migrating the key requires explicit authorization and a separate reviewed change.`;
  }
  // The configuration precondition is an internal request guard. It travels
  // with a Profile update and is read from the observation only to be sent
  // back, so its raw hash is never rendered and never searchable.
  const profileInternalDetailKeys = new Set(["configDigest"]);
  function visibleResourceDetails(resource) {
    const details = resource?.details || {};
    const visible = {};
    for (const key of Object.keys(details).sort()) {
      if (profileInternalDetailKeys.has(key)) continue;
      visible[key] = details[key];
    }
    return visible;
  }
  // Form values of one observed Profile: the exact owner identity, the
  // observed quota keys and the internal configuration precondition, and
  // nothing else. Keys match the form field names, so the result can be
  // submitted as the same typed entries.
  // The owner is a legacy identity that is kept exactly as it was observed:
  // it is never trimmed, rewritten or replaced.
  // baseConfigDigest is an internal request precondition: it travels with the
  // request untouched and is never rendered or edited.
  function profileFormValues(resource) {
    const details = resource?.details || {};
    const owner = String(resource?.owner ?? "") || String(details.owner ?? "");
    const values = { name: String(resource?.name ?? ""), owner };
    for (const [key, value] of Object.entries(profileQuotaValues(resource))) values["quota." + key] = value;
    const digest = String(details.configDigest ?? "");
    if (digest) values.baseConfigDigest = digest;
    return values;
  }
  // A deletion needs a mandatory reason and an exact name confirmation. The
  // confirmation is compared character for character: surrounding whitespace or
  // a different case is a mismatch, never a sanitized match.
  function profileDeleteValidation(name, inputs = {}) {
    const values = inputs || {};
    const target = String(name ?? "");
    const reason = String(values.reason ?? "").trim();
    const confirmName = String(values.confirmName ?? "");
    if (!target.trim()) return { valid: false, message: "Select the profile to delete." };
    if (!reason) return { valid: false, message: "A reason is required to delete a profile." };
    if (!confirmName) return { valid: false, message: `Type "${target}" to confirm the deletion.` };
    if (confirmName !== target) return { valid: false, message: `The confirmation does not match the profile name "${target}". Type it exactly.` };
    return { valid: true, message: "" };
  }
  const namespaceResourceDefs = [
    { kind: "Notebook", label: "Notebooks", href: "/notebooks" },
    { kind: "Pod", label: "Pods", href: "/pods" },
    { kind: "PersistentVolumeClaim", label: "Persistent volume claims", href: "/storage" },
    // StatefulSets no longer appear on the Notebooks page, which lists the
    // Notebook custom resource only. Their aggregate observation stays on the
    // Overview table, focused by its canonical kind.
    { kind: "StatefulSet", label: "StatefulSets", href: "/" },
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
    namespaceOptions, profileNames, profileSummary, profileResources, profileSummaryDetails, notebookResources,
    profileNameMaxLength, profileNameValidation, profileFormValues, profileQuotaValues, profileDeleteValidation, visibleResourceDetails,
    builtinQuotaKeys, deviceQuotaKey, profileQuotaKind, profileQuotaCounts, isWholeQuotaCountValue, bareDeviceQuotaKeys, profileDeviceQuotaWarning,
    namespaceResources, formatAge, workspaceHealthSummary,
    routeFor, queryNamespace, hrefFor, historyHref, needsRefresh,
    notificationID, shouldRememberDismissals, dismissalStorageKey,
    defaultTableState, nextSort, withPage, viewRows
  };
  if (typeof module !== "undefined" && module.exports) module.exports = logic;
  else root.DashboardLogic = logic;
})(globalThis);
