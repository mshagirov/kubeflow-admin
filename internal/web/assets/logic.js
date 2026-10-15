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
    const fields = [["Plan ID", plan.id], ["Actor", plan.actor], ["Created", plan.createdAt], ["Expires", plan.expiresAt], ["Summary", p.summary], ["Target UID", p.targetUID], ["Resource version", p.resourceVersion]];
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
    return fields;
  }
  const logic = { buildAction, scopedResources, containerNames, previewFields };
  if (typeof module !== "undefined" && module.exports) module.exports = logic;
  else root.DashboardLogic = logic;
})(globalThis);
