"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const logic = require("../assets/logic.js");
const root = path.join(__dirname, "..", "assets");
const html = fs.readFileSync(path.join(root, "index.html"), "utf8");
const {buildAction, scopedResources, containerNames, previewFields} = logic;
const base = [["kind", "notebook.edit"], ["namespace", "team"], ["name", "work"], ["container", "main"], ["cpuRequest", "500m"], ["memoryLimit", "4Gi"], ["image", ""], ["gpuCount", ""], ["gpuPreset", ""]];
const unchanged = buildAction(base);
assert.equal(unchanged.kind, "notebook.edit");
assert.equal(unchanged.cpuRequest, "500m");
assert.equal(unchanged.memoryLimit, "4Gi");
for (const field of ["gpuCount", "gpuPreset", "mounts", "image"]) assert.equal(Object.hasOwn(unchanged, field), false, field + " must remain unspecified");
const clear = buildAction([...base, ["gpuCount", "0"]], {replaceMounts: true, mounts: [], restart: true});
assert.equal(clear.gpuCount, 0); assert.deepEqual(clear.mounts, []); assert.equal(clear.restart, true);
const mount = {name:"data",claim:"workspace",path:"/data",readOnly:true};
assert.deepEqual(buildAction(base, {replaceMounts:true,mounts:[mount]}).mounts, [mount]);
for (const [form, backend] of [["profile.create","profile.create"],["profile.quota","profile.quota.change"],["pvc.create","pvc.create.dynamic"],["pvc.expand","pvc.expand"],["pv.nfs.create","storage.create.static"],["pv.reserve","pv.reserve-and-claim"],["notebook.start","notebook.start"],["notebook.stop","notebook.stop"]]) {
  assert.equal(buildAction([["kind",form]]).kind,backend);
}
// Typed storage registration actions keep their exact kind. No generic alias
// can change their meaning, and each action only carries its own fields.
assert.equal(buildAction([["kind","pv.create.static"]]).kind, "pv.create.static");
assert.equal(buildAction([["kind","pvc.create.bound"]]).kind, "pvc.create.bound");
assert.deepEqual(
  buildAction([["kind","pv.create.static"],["name","nfs-new"],["storageClass","nfs-csi"],["template","nfs-template"],["nfsPath","/exports/kubeflow/team"],["podDefaultName","mount-x"]], {podDefault:{name:"mount-x",mountPath:"/home/jovyan/x",overwrite:false}}),
  {kind:"pv.create.static",name:"nfs-new",storageClass:"nfs-csi",template:"nfs-template",nfsPath:"/exports/kubeflow/team",podDefault:{name:"mount-x",mountPath:"/home/jovyan/x",overwrite:false}},
  "a static PV action carries only its approved typed fields");
// pvc.create.bound accepts no backend path at all.
const boundPayload = buildAction([["kind","pvc.create.bound"],["namespace","team"],["name","data"],["storageClass","nfs-csi"],["template","nfs-template"],["pvName","nfs-pv"],["targetUID","uid-pv"],["targetResourceVersion","77"],["directoryUID","1000"],["directoryGID","2000"],["nfsPath","/forged"],["beeGFSPath","forged/suffix"]]);
assert.equal(Object.hasOwn(boundPayload,"nfsPath"), false, "a bound PVC never accepts an NFS path");
assert.equal(Object.hasOwn(boundPayload,"beeGFSPath"), false, "a bound PVC never accepts a BeeGFS path");
assert.equal(boundPayload.directoryUID, "1000");
const pdAction = buildAction([["kind","pvc.create.bound"],["namespace","team"],["name","data"],["podDefaultName","raw-control"]], {podDefault:{name:"mount-toast",mountPath:"/home/jovyan/data",overwrite:true}});
assert.deepEqual(pdAction.podDefault, {name:"mount-toast",mountPath:"/home/jovyan/data",overwrite:true});
assert.equal(Object.hasOwn(pdAction,"podDefaultName"), false, "raw PodDefault controls are never action fields");
assert.equal(logic.podDefaultNameError("a".repeat(52)), "");
assert.ok(logic.podDefaultNameError("a".repeat(53)).includes("52"), "the PodDefault creation name is capped at 52 bytes");
assert.equal(logic.nfsPathError("/exports/kubeflow", ["/exports/kubeflow"]), "", "the approved NFS root itself is accepted");
assert.equal(logic.nfsPathError("/exports/kubeflow/team", ["/exports/kubeflow"]), "");
assert.ok(logic.nfsPathError("/exports/other", ["/exports/kubeflow"]), "an unapproved NFS root is refused");
assert.ok(logic.nfsPathError("/exports/kubeflow/../other", ["/exports/kubeflow"]), "NFS traversal is refused");
assert.equal(logic.beeGFSPathError("example.user/kf-shared-volume"), "");
for (const value of ["/example.user", "../x", "foo//bar", "beegfs://host/home/x", "x?y=1"]) assert.ok(logic.beeGFSPathError(value), "invalid BeeGFS suffix refused: " + value);
assert.equal(logic.directoryIDError("1000"), "");
for (const value of ["0", "00", "-1", "1.5", "+1"]) assert.ok(logic.directoryIDError(value), "invalid directory ID refused: " + value);
const nfsTemplate = {name:"nfs-template",kind:"nfs",storageClass:"nfs-csi",pvCapacity:"2400Gi",pvAccessModes:["ReadWriteMany"],pvcRequest:"10Gi",volumeMode:"Filesystem",nfsRoots:["/exports/kubeflow"]};
const compatible = logic.compatiblePVs([
  {kind:"PersistentVolume",name:"exact",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"larger-compatible",state:"Available",details:{capacity:"5000Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"too-small",state:"Available",details:{capacity:"5Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"wrong-class",state:"Available",details:{capacity:"2400Gi",storageClass:"standard",reclaimPolicy:"Retain",accessModes:"ReadWriteMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"claimed",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany",claim:"team/data",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"wrong-mode-subset",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadOnlyMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"wrong-mode-superset",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany,ReadOnlyMany",registrationTemplate:"nfs-template"}}
], nfsTemplate, "nfs-csi").map(resource => resource.name);
assert.deepEqual(compatible, ["exact", "larger-compatible"], "a larger compatible PV passes, while a too-small, wrong-class, claimed or mode-mismatched PV does not");
const projectedModes = {...nfsTemplate, pvAccessModes:["ReadWriteMany","ReadOnlyMany"]};
const observedStringModes = [
  {kind:"PersistentVolume",name:"string-modes",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany,ReadOnlyMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"wrong-subset",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany",registrationTemplate:"nfs-template"}},
  {kind:"PersistentVolume",name:"other-template",state:"Available",details:{capacity:"2400Gi",storageClass:"nfs-csi",reclaimPolicy:"Retain",accessModes:"ReadWriteMany,ReadOnlyMany",registrationTemplate:"other-template"}}
];
assert.deepEqual(logic.compatiblePVs(observedStringModes, projectedModes, "nfs-csi").map(resource => resource.name), ["string-modes"], "comma-separated modes must match the complete template set exactly and the template name must match");
assert.equal(logic.beeGFSHandleError("example.user/x", "beegfs://beegfs.example.test/home"), "", "a bounded BeeGFS suffix passes");
assert.ok(logic.beeGFSHandleError("a".repeat(120), "beegfs://beegfs.example.test/home"), "a joined BeeGFS handle over 128 bytes is refused");
for (const value of ["bad%2fpath", "bad\\path", "bad:path", "bad@path", "bad?x", "bad#x", "bad path"]) assert.ok(logic.beeGFSPathError(value), "unsafe BeeGFS suffix refused: " + value);
assert.ok(logic.nfsPathError("/exports/kubeflow%2fteam", ["/exports/kubeflow"]), "an encoded NFS escape is refused");
assert.ok(logic.nfsPathError("\\exports\\kubeflow", ["/exports/kubeflow"]), "a backslash NFS path is refused");
assert.equal(logic.directoryIDError("4294967294"), "", "the maximum 10-digit directory ID passes");
assert.ok(logic.directoryIDError("4294967295"), "an ID above the 32-bit unsigned bound is refused");
assert.ok(logic.directoryIDError("10000000000"), "an 11-digit directory ID is refused");
for (const id of ["storage-directory-uid", "storage-directory-gid"]) {
  const tag = [...html.matchAll(new RegExp(`<input[^>]*id="${id}"[^>]*>`, "g"))][0]?.[0] || "";
  assert.ok(tag.includes('maxlength="10"') && tag.includes('inputmode="numeric"'), "the directory ID input keeps the 10-digit numeric contract: " + id);
}

assert.equal(logic.podDefaultSuggestion(nfsTemplate, "data").mountPath, "/home/jovyan/data", "the mount suggestion is prefilled but remains editable");
assert.equal(logic.podDefaultSuggestion(null, "data").name, "mount-data");

// The Profile quota count fields accept a deliberate zero hard limit. The
// server enforces a non-negative quantity, so a native minimum of one would
// make the browser refuse every update of a Profile that limits a count to
// zero. The count fields stay integer valued.
const profileQuotaInputs = [...html.matchAll(/<input[^>]*data-profile-quota="([^"]+)"[^>]*>/g)].map(match => ({ key: match[1], tag: match[0] }));
for (const key of ["pods", "persistentvolumeclaims"]) {
  const input = profileQuotaInputs.find(entry => entry.key === key);
  assert.ok(input, "the profile quota field " + key + " exists");
  assert.ok(input.tag.includes('min="0"'), "the profile quota field " + key + " accepts a zero hard limit");
  assert.ok(input.tag.includes('step="1"'), "the profile quota field " + key + " stays an integer");
}
for (const entry of profileQuotaInputs.filter(item => item.key.startsWith("requests.") && item.tag.includes('type="number"'))) {
  assert.ok(entry.tag.includes('min="0"'), "the profile GPU quota field " + entry.key + " accepts a zero hard limit");
  assert.ok(entry.tag.includes('step="1"'), "the profile GPU quota field " + entry.key + " stays an integer");
}
assert.equal(html.includes('type="number" min="1"'), false, "no profile quota field still demands a minimum of one");
assert.ok(html.includes('<input name="size" placeholder="20Gi">'), "the storage size field keeps its own contract");
assert.equal(html.includes('name="size" type="number"'), false, "the storage size field is not turned into a count field");
// The legacy Profile quota alias keeps its deprecated kind, but it is guarded
// like profile.update: the exact current owner and the configuration
// precondition travel with the request.
assert.deepEqual(buildAction([["kind","profile.quota"],["name","team"],["owner","alice@example.test"],["baseConfigDigest","sha256:9f2b1c"],["quota.requests.cpu","2"],["quota.requests.memory",""]]),
  {kind:"profile.quota.change",name:"team",owner:"alice@example.test",baseConfigDigest:"sha256:9f2b1c",quota:{"requests.cpu":"2"}},
  "the legacy alias keeps its kind and carries the guard");
assert.equal(buildAction([["kind","profile.create"],["owner","CaseSensitive-Username"]]).owner,"CaseSensitive-Username");
assert.deepEqual(buildAction([["kind","pod.restart"],["namespace","team"],["name","work-0"],["reason","  recover stalled kernel  "]]), {kind:"pod.restart",namespace:"team",name:"work-0",reason:"recover stalled kernel"});
assert.deepEqual(buildAction([["kind","node.cordon"],["namespace","team"],["owner","ignored"],["name","worker-1"],["reason","  drain maintenance  "]]), {kind:"node.cordon",name:"worker-1",reason:"drain maintenance",force:false});
assert.deepEqual(buildAction([["kind","node.uncordon"],["name","worker-1"],["reason","maintenance complete"],["force","on"]]), {kind:"node.uncordon",name:"worker-1",reason:"maintenance complete",force:true});
assert.equal(buildAction([["kind","node.cordon"],["name","worker-1"],["reason","x"]], {force:true}).force, true, "explicit force option is preserved");
assert.deepEqual(containerNames({details:{"image.main":"registry/image","image.sidecar":"proxy","capacity":"10Gi"}}),["main","sidecar"]);
const resources = [
  {kind:"Profile",name:"team"}, {kind:"Profile",name:"other"},
  {kind:"Notebook",name:"work",namespace:"team"}, {kind:"Notebook",name:"outside",namespace:"other"},
  {kind:"PersistentVolumeClaim",name:"data",namespace:"team",details:{volume:"bound-team",storageClass:"team-class"}},
  {kind:"PersistentVolume",name:"bound-team",state:"Bound",details:{}},
  {kind:"PersistentVolume",name:"claim-team",state:"Bound",details:{claim:"team/another"}},
  {kind:"PersistentVolume",name:"outside-pv",state:"Bound",details:{claim:"other/data"}},
  {kind:"PersistentVolume",name:"available",state:"Available",details:{reclaimPolicy:"Retain"}},
  {kind:"PersistentVolume",name:"reserved-other",state:"Available",details:{claim:"other/data"}},
  {kind:"StorageClass",name:"team-class",details:{approved:"true"}}
];
assert.deepEqual(scopedResources(resources,"team").map(r=>r.name),["team","work","data","bound-team","claim-team","available","team-class"]);

// A Node scoped Pod count uses the canonical Pod kind and phase and an exact
// projected node identity. Notebooks, StatefulSets, pending Pods, a node-name
// prefix and Pods without a node are not part of the count.
const nodePods = [
  { kind: "Pod", name: "alpha-running", namespace: "alpha", state: "Running", details: { node: "node-a" } },
  { kind: "Pod", name: "beta-running", namespace: "beta", state: "Running", details: { node: "node-a" } },
  { kind: "Pod", name: "pending", namespace: "alpha", state: "Pending", details: { node: "node-a" } },
  { kind: "Pod", name: "other-running", namespace: "alpha", state: "Running", details: { node: "node-b" } },
  { kind: "Pod", name: "prefix-running", namespace: "alpha", state: "Running", details: { node: "node-a-extra" } },
  { kind: "Pod", name: "missing-node", namespace: "alpha", state: "Running", details: {} },
  { kind: "Pod", name: "unknown-node", namespace: "alpha", state: "Running", details: { node: "node-unknown" } },
  { kind: "Notebook", name: "notebook", namespace: "alpha", state: "Running", details: { node: "node-a" } },
  { kind: "StatefulSet", name: "statefulset", namespace: "alpha", state: "Running", details: { node: "node-a" } }
];
assert.deepEqual(logic.runningPodsOnNode(nodePods, "node-a").map(pod => pod.name).sort(), ["alpha-running", "beta-running"], "only exact Running Pod resources on the exact node are counted");
assert.equal(logic.runningPodsOnNode(nodePods, "node").length, 0, "a node-name prefix is not the selected node");
assert.equal(logic.runningPodsOnNode(nodePods, "node-zero").length, 0, "a node with no Running Pods has a genuine zero count");
assert.equal(logic.runningPodsOnNode(nodePods, "").length, 0, "an empty node name never means every node");
assert.equal(logic.runningPodsOnNode(null, "node-a").length, 0, "a missing inventory is safe");
assert.equal(logic.podMatchesFilters(nodePods[0], { node: "node-a", phase: "Running" }), true);
assert.equal(logic.podMatchesFilters(nodePods[2], { node: "node-a", phase: "Running" }), false, "the Pending Pod is not Running");
assert.equal(logic.podMatchesFilters(nodePods[0], { node: "node-a", phase: "Succeeded" }), false);
assert.equal(logic.podMatchesFilters(nodePods[7], { node: "node-a", phase: "Running" }), false, "a Notebook is never counted as a Pod");
assert.deepEqual(logic.queryPodFilters("?node=node-a&phase=Running"), { node: "node-a", phase: "Running" });
assert.deepEqual(logic.queryPodFilters("?node=node-a&phase=Pending"), { node: "node-a", phase: "Pending" });
assert.deepEqual(logic.queryPodFilters("?node=node-a&phase=NotAPhase"), { node: "node-a", phase: "" }, "an unknown phase is ignored");
assert.deepEqual(logic.queryPodFilters("?node=%20node-a%20&phase=Running"), { node: "node-a", phase: "Running" });
assert.deepEqual(logic.queryPodFilters(null), { node: "", phase: "" }, "a missing query is safe");
assert.equal(logic.podNodeHref("node-a"), "/pods?node=node-a&phase=Running", "the scoped link carries only the exact node and Running filters");
const escapedNodeHref = logic.podNodeHref('node "a"<img>');
assert.ok(escapedNodeHref.includes("node+%22a%22%3Cimg%3E"), "an unusual node name is URL encoded");
assert.equal(escapedNodeHref.includes("<"), false, "a node name never appears as markup in the link");
const plan = {id:"server-plan",actor:"issuer|admin",actorDisplay:"Alice Admin",prepared:{summary:"server summary",action:clear,warnings:["warning"],data:{policy:"DO_NOT_RENDER_OPAQUE_HASH",dependencies:[{digest:"also-opaque"}]}}};
const preview = previewFields(plan), text = JSON.stringify(preview);
assert.equal(text.includes("DO_NOT_RENDER_OPAQUE_HASH"),false); assert.equal(text.includes("also-opaque"),false);
assert.equal(preview.find(([label])=>label==="GPU count")[1],0);
assert.deepEqual(preview.find(([label])=>label==="Replacement PVC mounts")[1],[]);
assert.equal(preview.find(([label])=>label==="Actor")[1],"Alice Admin");
assert.equal(preview.find(([label])=>label==="Actor ID"),undefined,"raw actor IDs are never displayed");
assert.equal(text.includes("issuer|admin"),false,"legacy actor keys never leak into plan previews");
assert.ok(text.includes("500m") && text.includes("4Gi") && text.includes("Remove all PVC mounts"));
assert.ok(text.includes("previously running"));
const preservePreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:unchanged}}));
assert.ok(preservePreview.includes("Unchanged (replacement not requested)") && preservePreview.includes("Unchanged (not specified)"));
assert.ok(preservePreview.includes("leave the notebook stopped"));
const podPreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:buildAction([["kind","pod.restart"],["namespace","team"],["name","work-0"],["reason","recover"]])}}));
assert.ok(podPreview.includes("pod.restart") && podPreview.includes("recover"));
assert.ok(podPreview.includes("disruption-budget-aware eviction"));
assert.ok(podPreview.includes("Force deletion") && podPreview.includes("Never used automatically"));
const cordonPreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:buildAction([["kind","node.cordon"],["name","worker-1"],["reason","maintenance"]])}}));
assert.ok(cordonPreview.includes("spec.unschedulable=true") && cordonPreview.includes("SchedulingDisabled") && cordonPreview.includes("not evicted"));
assert.ok(cordonPreview.includes("Force") && cordonPreview.includes("maintenance"));
const uncordonPreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:buildAction([["kind","node.uncordon"],["name","worker-1"],["reason","done"],["force","on"]])}}));
assert.ok(uncordonPreview.includes("spec.unschedulable=false") && uncordonPreview.includes("scheduled onto this node again"));

// ---- Narrow storage YAML edit and deletion actions ------------------------
assert.equal(logic.storageActionKind("PersistentVolume", "edit"), "pv.edit");
assert.equal(logic.storageActionKind("PersistentVolume", "delete"), "pv.delete");
assert.equal(logic.storageActionKind("PersistentVolumeClaim", "edit"), "pvc.edit");
assert.equal(logic.storageActionKind("PersistentVolumeClaim", "delete"), "pvc.delete");
assert.equal(logic.storageActionKind("StorageClass", "edit"), "", "StorageClass has no storage action");
assert.equal(logic.storageTargetKey("PersistentVolume", "", "data"), "PersistentVolume||data", "a PV target is cluster scoped");
assert.equal(logic.storageTargetKey("PersistentVolumeClaim", "team", "data"), "PersistentVolumeClaim|team|data", "a PVC target is namespaced");
const pvcEdit = buildAction([["kind","pvc.edit"],["name","data"],["namespace","team"],["targetUID","uid-pvc"],["targetResourceVersion","7"],["storageYAML","apiVersion: v1\nkind: PersistentVolumeClaim\n"],["reason","  grow the claim  "],["confirmName","ignored"],["force","on"]]);
assert.deepEqual(pvcEdit, {kind:"pvc.edit",name:"data",namespace:"team",targetUID:"uid-pvc",targetResourceVersion:"7",storageYAML:"apiVersion: v1\nkind: PersistentVolumeClaim\n",reason:"grow the claim"}, "a PVC edit carries the exact projection and target identity only");
assert.equal(Object.hasOwn(pvcEdit,"confirmName"), false, "an edit carries no confirmation");
assert.equal(Object.hasOwn(pvcEdit,"force"), false, "an edit carries no force flag");
const pvDelete = buildAction([["kind","pv.delete"],["name","data"],["targetUID","uid-pv"],["targetResourceVersion","9"],["confirmName"," data "],["reason","  remove unused volume  "],["storageYAML","ignored"],["force","on"]]);
assert.deepEqual(pvDelete, {kind:"pv.delete",name:"data",targetUID:"uid-pv",targetResourceVersion:"9",confirmName:" data ",reason:"remove unused volume"}, "a PV deletion carries no namespace, YAML or force flag and keeps the typed confirmation exact");
const pvcDelete = buildAction([["kind","pvc.delete"],["name","data"],["namespace","team"],["targetUID","uid-pvc"],["targetResourceVersion","7"],["confirmName","data"],["reason","unused"]]);
assert.deepEqual(pvcDelete, {kind:"pvc.delete",name:"data",namespace:"team",targetUID:"uid-pvc",targetResourceVersion:"7",confirmName:"data",reason:"unused"});
const storageEditPreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:pvcEdit}}));
assert.ok(storageEditPreview.includes("Restricted YAML projection") && storageEditPreview.includes("not the full manifest") && storageEditPreview.includes("features.pvcExpansion"), "the edit preview explains the partial projection and the growth gate");
assert.ok(storageEditPreview.includes("server-provided warnings are authoritative for metadata.labels"), "the edit preview delegates label semantics to the server projection");
assert.equal(storageEditPreview.includes("API requires an edit reason"), false, "the preview never claims the API requires an edit reason");
assert.ok(storageEditPreview.includes("Read-only identity") && storageEditPreview.includes("targetUID") && storageEditPreview.includes("targetResourceVersion"), "the edit preview explains the read-only identity guard");
const storageDeletePreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:pvDelete}}));
assert.ok(storageDeletePreview.includes("not a filesystem wipe") && storageDeletePreview.includes("Retain reclaim policy") && storageDeletePreview.includes("never removes a finalizer"), "the deletion preview warns about data and cleanup");
assert.ok(storageDeletePreview.includes("no observed Pod or Notebook") || storageDeletePreview.includes("Available, unclaimed"), "the deletion preview names the eligibility rule");
assert.ok(storageDeletePreview.includes("needs-review") && storageDeletePreview.includes("do not retry"), "the deletion preview tells the operator not to retry an ambiguous result");
assert.equal(storageDeletePreview.includes("force:true"), false, "the deletion preview never claims a force flag");

// Kubernetes quantities, not lexicographic text.
assert.equal(logic.quantityValue("500m"), 0.5);
assert.equal(logic.quantityValue("2"), 2);
assert.equal(logic.quantityValue("1Gi"), 1024 ** 3);
assert.equal(logic.quantityValue("1Mi"), 1024 ** 2);
assert.ok(logic.quantityValue("20Mi") > logic.quantityValue("3Mi"));
assert.ok(logic.quantityValue("1Gi") > logic.quantityValue("900Mi"));
assert.equal(logic.quantityValue(""), null);
assert.equal(logic.quantityValue("Not reported"), null);
assert.equal(logic.quantityValue("unbounded"), null);

// Stable typed sorting; missing values always last.
const sortable = [
  {sort:{name:"beta",q:"1Gi",state:"Failed",time:"2026-02-01T00:00:00Z"},search:"beta"},
  {sort:{name:"alpha",q:"500Mi",state:"Running",time:"2026-01-01T00:00:00Z"},search:"alpha"},
  {sort:{name:"gamma",q:"2Gi",state:"Pending",time:"2026-03-01T00:00:00Z"},search:"gamma"},
  {sort:{name:"delta",q:"1Gi",state:"Stopped",time:""},search:"delta"},
  {sort:{name:"missing",q:"",state:"",time:""},search:"missing"}
];
assert.deepEqual(logic.sortRows(sortable,"q","asc","quantity").map(r=>r.sort.name), ["alpha","beta","delta","gamma","missing"]);
assert.deepEqual(logic.sortRows(sortable,"q","desc","quantity").map(r=>r.sort.name), ["gamma","beta","delta","alpha","missing"], "equal quantities keep loaded order and missing stays last");
assert.deepEqual(logic.sortRows(sortable,"state","asc","severity").map(r=>r.sort.name), ["alpha","gamma","delta","beta","missing"]);
assert.deepEqual(logic.sortRows(sortable,"time","desc","time").map(r=>r.sort.name), ["gamma","beta","alpha","delta","missing"]);
assert.deepEqual(logic.sortRows(sortable,"name","asc","text").map(r=>r.sort.name), ["alpha","beta","delta","gamma","missing"]);
assert.equal(logic.readyValue("2/4"), 0.5);
assert.equal(logic.readyValue("1/1"), 1);
assert.equal(logic.readyValue("0/1"), 0);
assert.equal(logic.severityValue("Unavailable"), 3);
assert.equal(logic.severityValue("Not ready"), 3);
assert.equal(logic.severityValue("Incomplete"), 3);
assert.equal(logic.severityValue("Unhealthy"), 3);
assert.equal(logic.severityValue("Available"), 0);
assert.equal(logic.severityValue("Normal"), 0);
assert.equal(logic.severityValue("Lost"), 3);
assert.equal(logic.severityValue("Applying"), 1);
assert.equal(logic.severityValue("NotActive"), 3);
assert.equal(logic.severityValue("Not running"), 3);
assert.equal(logic.severityValue("Ready"), 0);
assert.equal(logic.severityValue("Ready,SchedulingDisabled"), 2, "cordoned but ready node is a warning");
assert.equal(logic.severityValue("SchedulingDisabled"), 2);
assert.equal(logic.severityValue("NotReady"), 3);
assert.equal(logic.severityValue("NotReady,SchedulingDisabled"), 3, "not-ready outranks the cordon warning");
assert.equal(logic.severityValue("Mystery-state"), 2, "unrecognized state is neutral, not healthy");
assert.equal(logic.readyValue("1/1 ready"), 1);
assert.equal(logic.readyValue("0/2 ready"), 0);
assert.equal(logic.readyValue("Not reported"), null);
assert.equal(logic.isMissing("NOT REPORTED"), true);
assert.equal(logic.isMissing("—"), true);
assert.equal(logic.isMissing("n/a"), true);
assert.equal(logic.isMissing("0"), false);
const placeholders = [
  {sort:{name:"beta"}}, {sort:{name:"Not reported"}}, {sort:{name:"—"}}, {sort:{name:"n/a"}},
  {sort:{name:"alpha"}}
];
assert.deepEqual(logic.sortRows(placeholders,"name","asc","text").map(row=>row.sort.name), ["alpha","beta","Not reported","—","n/a"]);
assert.deepEqual(logic.sortRows(placeholders,"name","desc","text").map(row=>row.sort.name), ["beta","alpha","Not reported","—","n/a"], "placeholders stay last in both directions");
const mixed = [{sort:{value:"500m"}},{sort:{value:"2"}},{sort:{value:"Running"}},{sort:{value:"Not reported"}},{sort:{value:"—"}},{sort:{value:"n/a"}}];
assert.deepEqual(logic.sortRows(mixed,"value","asc","mixed").map(row=>row.sort.value), ["500m","2","Running","Not reported","—","n/a"], "mixed desired values compare numerically only when both parse as quantities");
const textDesired = [{sort:{value:"10Gi"}},{sort:{value:"2Gi"}},{sort:{value:"Running"}}];
assert.deepEqual(logic.sortRows(textDesired,"value","asc","text").map(row=>row.sort.value), ["2Gi","10Gi","Running"], "text fallback remains natural and never quantity-aware by accident");

// Search applies to loaded rows only; pagination defaults to 50.
assert.deepEqual(logic.filterRows(sortable,"gamma failed").map(r=>r.search), []);
assert.deepEqual(logic.filterRows([...sortable, {search:"gamma failed"}],"gamma failed").map(r=>r.search), ["gamma failed"]);
const many = Array.from({length:120}, (_, index) => ({sort:{name:String(index).padStart(3,"0")},search:String(index)}));
const third = logic.paginate(many, 3, 50);
assert.equal(third.total, 120); assert.equal(third.pages, 3); assert.equal(third.items.length, 20); assert.equal(third.page, 3);
assert.equal(logic.paginate(many, 99, 50).page, 3, "out-of-range pages clamp");
assert.equal(logic.viewRows(many, {...logic.defaultTableState("name"), search:"11"}, 50).items.length, 11);

// Shared target grammar and human actor labelling.
assert.deepEqual(logic.parseTarget("notebook:team/work"), {kind:"notebook",namespace:"team",name:"work"});
assert.deepEqual(logic.parseTarget("pod:team/work-0"), {kind:"pod",namespace:"team",name:"work-0"});
assert.deepEqual(logic.parseTarget("profile:team"), {kind:"profile",namespace:"",name:"team"});
assert.deepEqual(logic.parseTarget("unstructured"), {kind:"",namespace:"",name:"unstructured"});
assert.equal(logic.actorLabel({actorDisplay:"Alice",actor:"issuer|subject"}), "Alice");
assert.equal(logic.actorLabel({actor:"issuer|subject"}), "Legacy administrator", "legacy rows never expose the encoded issuer|subject key");
assert.equal(logic.actorLabel({actor:"https://issuer.example|" + "a".repeat(400)}), "Legacy administrator", "legacy records fall back to one readable label");
assert.equal(logic.actorLabel({actorDisplay:"A".repeat(400)}).length, 160, "display labels are bounded");
assert.equal(logic.actorLabel({}), "Legacy administrator");

// Audit summary groups stages per operation, keeps input reason only and
// never concatenates operation identifiers into the reason.
const audit = [
  {id:"a1",operationID:"op1",time:"2026-01-01T00:00:00Z",actor:"iss|u",actorDisplay:"Alice",action:"profile.create",target:"profile:team",result:"succeeded",reason:"create the team profile"},
  {id:"a2",operationID:"op1",time:"2026-01-01T00:00:05Z",actor:"iss|u",action:"profile.quota.change",target:"profile:team",result:"failed",reason:""},
  {id:"a3",time:"2026-01-02T00:00:00Z",actor:"iss|v",action:"notebook.stop",target:"notebook:team/work",result:"succeeded",reason:"pause for maintenance"}
];
const groups = logic.groupAudit(audit);
assert.equal(groups.length, 2);
const operation = groups.find(group => group.operationID === "op1");
assert.equal(operation.stages.length, 2);
assert.equal(operation.result, "failed", "summary follows the latest stage outcome");
assert.equal(operation.stages[0].result, "failed");
assert.equal(operation.reason, "create the team profile");
assert.equal(operation.reason.includes("op1"), false);
assert.equal(operation.actorLabel, "Alice");
assert.equal(operation.kind, "profile");
assert.equal(operation.name, "team");
assert.equal(operation.stages[0].id, "a2", "stages remain accessible newest first");
assert.equal(groups.find(group => !group.operationID).reason, "pause for maintenance");
const progressed = logic.groupAudit([
  {id:"p1",operationID:"op2",time:"2026-02-01T00:00:00Z",actor:"iss|u",action:"notebook.stop",target:"notebook:team/work",result:"queued",reason:"maintenance"},
  {id:"p2",operationID:"op2",time:"2026-02-01T00:00:05Z",actor:"iss|u",action:"notebook.stop",target:"notebook:team/work",result:"succeeded",reason:""}
]);
assert.equal(progressed[0].result, "succeeded", "acceptance queued must not mask the latest success");
const equalTime = logic.groupAudit([
  {id:"q",operationID:"op-equal",time:"2026-03-01T00:00:00Z",actor:"iss|u",action:"notebook.stop",target:"notebook:team/work",result:"queued",reason:"maintenance"},
  {id:"s",operationID:"op-equal",time:"2026-03-01T00:00:00Z",actor:"iss|u",action:"notebook.stop",target:"notebook:team/work",result:"succeeded",reason:""}
]);
assert.equal(equalTime[0].result, "succeeded", "terminal stage outranks queued at an equal timestamp");
assert.equal(equalTime[0].stages[0].id, "s", "final stage is first in details");
const equalNeedsReview = logic.groupAudit([
  {id:"applying",operationID:"op-review",time:"2026-03-02T00:00:00Z",actor:"iss|u",action:"pod.restart",target:"pod:team/work-0",result:"applying",reason:""},
  {id:"review",operationID:"op-review",time:"2026-03-02T00:00:00Z",actor:"iss|u",action:"pod.restart",target:"pod:team/work-0",result:"needs-review",reason:""}
]);
assert.equal(equalNeedsReview[0].result, "needs-review");

// Profiles only expose real Profiles and name-associated namespaces.
const profileList = [
  {kind:"Profile",name:"team"}, {kind:"Profile",name:"empty"},
  {kind:"Namespace",name:"team"}, {kind:"Namespace",name:"kube-system"},
  {kind:"Notebook",name:"work",namespace:"team"}, {kind:"Notebook",name:"system",namespace:"kube-system"},
  {kind:"Pod",name:"work-0",namespace:"team"}, {kind:"Pod",name:"system-0",namespace:"kube-system"},
  {kind:"PersistentVolumeClaim",name:"data",namespace:"team"},
  {kind:"StatefulSet",name:"work",namespace:"team"},
  {kind:"ResourceQuota",name:"quota",namespace:"team"}, {kind:"ResourceQuota",name:"quota",namespace:"kube-system"},
  {kind:"LimitRange",name:"limits",namespace:"team"},
  {kind:"RoleBinding",name:"editors",namespace:"team"},
  {kind:"Event",name:"started",namespace:"team"}
];
assert.deepEqual(logic.profileSummary(profileList,"team").map(entry=>[entry.label,entry.count]), [["Notebooks",1],["Pods",1],["Persistent volume claims",1]]);
assert.deepEqual(logic.profileSummary(profileList,"empty"), [], "zero counts are omitted, never inferred");
const visibleProfiles = logic.profileResources(profileList, "").map(resource => resource.kind + ":" + (resource.namespace ? resource.namespace + "/" : "") + resource.name).sort();
assert.deepEqual(visibleProfiles, ["Profile:empty","Profile:team"], "Profiles lists only Profiles; namespaces have their own page");
assert.deepEqual(logic.profileResources(profileList, "kube-system").map(resource => resource.name).sort(), ["empty","team"], "Profiles stay cluster-wide when a namespace scope is active");
assert.deepEqual(logic.profileNames(profileList).size, 2);
assert.deepEqual(logic.namespaceOptions(profileList), ["empty","kube-system","team"]);

// Namespace summaries group observed resources by kind and omit empty groups.
const namespaceGroups = logic.namespaceResources(profileList, "team");
assert.deepEqual(namespaceGroups.map(group => [group.kind, group.label, group.count]), [
  ["Notebook","Notebooks",1], ["Pod","Pods",1], ["PersistentVolumeClaim","Persistent volume claims",1],
  ["StatefulSet","StatefulSets",1], ["ResourceQuota","Resource quotas",1], ["LimitRange","Limit ranges",1],
  ["RoleBinding","Role bindings",1], ["Event","Events",1]
]);
assert.equal(namespaceGroups.find(group => group.kind === "Notebook").href, "/notebooks");
assert.equal(namespaceGroups.find(group => group.kind === "ResourceQuota").href, "/");
// StatefulSets no longer appear on the Notebooks page. Their scoped group and
// resource links land on the Overview table, never on an empty Notebook view.
assert.equal(namespaceGroups.find(group => group.kind === "StatefulSet").href, "/");
assert.equal(namespaceGroups.find(group => group.kind === "Pod").items[0].name, "work-0");
assert.deepEqual(logic.namespaceResources(profileList, "empty"), [], "empty groups are omitted, never inferred");
assert.deepEqual(logic.namespaceResources(profileList, ""), []);
assert.equal(namespaceGroups.some(group => group.kind === "Namespace"), false, "the Namespace object is not listed as its own content");

// Only kubeflow Notebook custom resources are Notebook rows: a Pod or
// StatefulSet with the same name is a different resource.
assert.deepEqual(logic.notebookResources(profileList).map(resource => resource.name), ["work", "system"]);
assert.deepEqual(logic.notebookResources([{kind:"Pod",name:"work"},{kind:"StatefulSet",name:"work"}]), [], "Pods and StatefulSets are never Notebook resources");

// ---- Quota key classification ---------------------------------------------
// The standard quota aliases stay editable builtins, including the enforced
// cpu/memory aliases and whole-object counts.
for (const key of ["requests.cpu", "limits.cpu", "requests.memory", "limits.memory", "requests.storage", "pods", "persistentvolumeclaims", "cpu", "memory"]) {
  assert.equal(logic.profileQuotaKind(key, []), "builtin", key + " is a builtin quota resource");
}
// count/<resource>.<group> is an enforced object-count quota, not an inert
// bare device. It stays unmanaged/read-only and never raises the GPU warning.
assert.equal(logic.deviceQuotaKey("count/jobs.batch"), false, "count/ quotas are object counts, not device resources");
assert.equal(logic.profileQuotaKind("count/jobs.batch", []), "unmanaged", "an enforced count quota is never editable as a device");
assert.equal(logic.profileDeviceQuotaWarning({"count/jobs.batch":"5"}), "", "an enforced count quota raises no GPU warning");
// A reserved storage-capacity key is operator managed, never a raw device.
assert.equal(logic.deviceQuotaKey("class.storage.k8s.io/requests.storage"), false);
assert.equal(logic.profileQuotaKind("class.storage.k8s.io/requests.storage", []), "unmanaged");
// An existing raw GPU key is an editable device under its own name; the
// canonical approved preset key is a device and counts whole devices.
assert.equal(logic.deviceQuotaKey("nvidia.com/gpu"), true);
assert.equal(logic.profileQuotaKind("nvidia.com/gpu", []), "device");
assert.equal(logic.profileQuotaKind("requests.nvidia.com/gpu", ["requests.nvidia.com/gpu"]), "device");
assert.equal(logic.profileQuotaKind("requests.nvidia.com/gpu", []), "unmanaged", "a canonical key without an approved preset is not authored");
assert.equal(logic.profileQuotaCounts("requests.nvidia.com/gpu", ["requests.nvidia.com/gpu"]), true, "an approved device preset counts whole devices");
assert.equal(logic.profileQuotaCounts("requests.cpu", []), false, "CPU quantity is not a count");
// The device domain follows DNS-1123 subdomain grammar; an invalid Profile key
// is never offered as editable.
for (const key of ["bad..domain/gpu", "-bad.domain/gpu", "bad-.domain/gpu", "UPPER.example/gpu", "a".repeat(64) + ".example/gpu", "nvidia.com/" + "g".repeat(64), "nvidia.com/bad name", "nvidia.com/bad/name"]) {
  assert.equal(logic.deviceQuotaKey(key), false, key + " is not a valid raw device key");
}
assert.equal(logic.deviceQuotaKey("gpu.example.com/a100"), true);

// Every finite non-negative integer spelling is a valid count value. The raw
// text is never rewritten.
for (const value of ["0", "1", "1.0", "1e2", "2E3", 5]) {
  assert.equal(logic.isWholeQuotaCountValue(value), true, String(value) + " is a whole count");
}
for (const value of ["", "0.5", "-1", "abc", NaN, Infinity, -Infinity]) {
  assert.equal(logic.isWholeQuotaCountValue(value), false, String(value) + " is not a whole count");
}
const gpuWarning = logic.profileDeviceQuotaWarning({"nvidia.com/gpu":"1"});
assert.ok(gpuWarning.includes("requests.<device>"), "the warning names the enforced canonical form");
assert.ok(gpuWarning.includes("declared but is normally not enforced"), "the warning states the bare key is not enforced");
assert.ok(gpuWarning.includes("does not rename the key") && gpuWarning.includes("does not activate enforcement"), "the warning never claims an implicit migration");
assert.ok(gpuWarning.includes("explicit authorization"), "a migration requires explicit authorization");
assert.equal(logic.profileDeviceQuotaWarning({"requests.nvidia.com/gpu":"1"}), "", "a canonical key raises no bare-device warning");
const summaryDetails = logic.profileSummaryDetails({kind:"Profile",name:"team",state:"Protected",owner:"a@b.test",
  details:{apiVersion:"kubeflow.org/v1",ownerKind:"User",createdAt:"2026-01-01T00:00:00Z",resourceVersion:"7",configDigest:"sha256:secret","quota.cpu":"4"}});
assert.equal(summaryDetails.apiVersion, "kubeflow.org/v1");
assert.equal(summaryDetails.owner, "a@b.test");
assert.equal(summaryDetails.ownerKind, "User");
assert.equal(summaryDetails.state, "Protected");
assert.equal(summaryDetails.scope, "Cluster scoped");
assert.equal(summaryDetails.createdAt, "2026-01-01T00:00:00Z");
assert.equal(summaryDetails.resourceVersion, "7");
assert.deepEqual(summaryDetails.quota, {cpu:"4"});
assert.equal(JSON.stringify(summaryDetails).includes("sha256:secret"), false, "the summary never carries the configuration digest");

// Concise kubectl-style ages.
assert.equal(logic.formatAge("2026-01-01T00:00:00Z", Date.parse("2026-01-01T00:00:45Z")), "45s");
assert.equal(logic.formatAge("2026-01-01T00:00:00Z", Date.parse("2026-01-01T00:12:00Z")), "12m");
assert.equal(logic.formatAge("2026-01-01T00:00:00Z", Date.parse("2026-01-01T06:00:00Z")), "6h");
assert.equal(logic.formatAge("2026-01-01T00:00:00Z", Date.parse("2026-01-15T00:00:00Z")), "14d");
assert.equal(logic.formatAge("", Date.parse("2026-01-15T00:00:00Z")), "—");
assert.equal(logic.formatAge("not-a-time", Date.parse("2026-01-15T00:00:00Z")), "—");
assert.equal(logic.formatAge(undefined, Date.parse("2026-01-15T00:00:00Z")), "—");

// Workspace health summary classifies each resource into exactly one state.
const healthResources = [
  {kind:"Profile",name:"team",state:""},
  {kind:"Profile",name:"protected-team",state:"Protected"},
  {kind:"Notebook",name:"running",namespace:"team",state:"Running / Ready"},
  {kind:"Notebook",name:"stopped",namespace:"team",state:"Stopped"},
  {kind:"Notebook",name:"broken",namespace:"team",state:"Failed"},
  {kind:"Pod",name:"ready",namespace:"team",state:"Running",details:{ready:"true",restarts:"0"}},
  {kind:"Pod",name:"restarting",namespace:"team",state:"Running",details:{ready:"true",restarts:"2"}},
  {kind:"Pod",name:"pending",namespace:"team",state:"Pending",details:{ready:"false",restarts:"0"}},
  {kind:"Pod",name:"failed",namespace:"team",state:"CrashLoopBackOff",details:{ready:"false"}},
  {kind:"Node",name:"n1",state:"Ready",details:{}},
  {kind:"Node",name:"n2",state:"Ready,SchedulingDisabled",details:{}},
  {kind:"Node",name:"n3",state:"NotReady",details:{}},
  {kind:"PersistentVolumeClaim",name:"pvc1",namespace:"team",state:"Bound"},
  {kind:"PersistentVolumeClaim",name:"pvc2",namespace:"team",state:"Pending"},
  {kind:"PersistentVolumeClaim",name:"pvc3",namespace:"team",state:"Lost"},
  {kind:"PersistentVolume",name:"pv1",state:"Bound",details:{}},
  {kind:"PersistentVolume",name:"pv2",state:"Available",details:{}},
  {kind:"PersistentVolume",name:"pv3",state:"Failed",details:{}},
  {kind:"Event",name:"e1",namespace:"team",state:"Normal"},
  {kind:"Event",name:"e2",namespace:"team",state:"Warning"}
];
const health = logic.workspaceHealthSummary(healthResources, "");
assert.deepEqual(health.Profiles.states.map(state => [state.label,state.count,state.tone]), [["Unprotected",1,"neutral"],["Protected",1,"warn"]]);
assert.equal(health.Profiles.total, 2);
assert.equal(logic.workspaceHealthSummary(healthResources, "team").Profiles.total, 1, "Profile cards follow namespace scope by Profile name");
assert.equal(logic.workspaceHealthSummary(healthResources, "missing").Profiles.total, 0);
assert.equal(logic.workspaceHealthSummary([], "").Profiles.total, 0);
assert.equal(health.Notebooks.total, 3);
assert.deepEqual(health.Notebooks.states.map(state => [state.label,state.count,state.tone]), [["Running",1,"good"],["Stopped",1,"warn"],["Transitioning",0,"warn"],["Errors",1,"bad"]]);
assert.deepEqual(health.Pods.states.map(state => [state.label,state.count]), [["Running",1],["Warning",2],["Errors",1]]);
assert.deepEqual(health.Nodes.states.map(state => [state.label,state.count]), [["Ready",1],["Cordoned",1],["NotReady",1]]);
assert.deepEqual(health.PersistentVolumeClaims.states.map(state => [state.label,state.count]), [["Bound",1],["Pending",1],["Errors",1]]);
assert.deepEqual(health.PersistentVolumes.states.map(state => [state.label,state.count]), [["Bound",1],["Available",1],["Other / Error",1]]);
assert.deepEqual(health.Events.states.map(state => [state.label,state.count]), [["Normal",1],["Warning",1]]);
for (const key of Object.keys(health)) assert.equal(health[key].states.reduce((sum,state)=>sum+state.count,0), health[key].total, key + " states add up to the total");
assert.equal(logic.workspaceHealthSummary(healthResources, "team").Nodes.total, 3, "Nodes stay cluster-wide even when a namespace is selected");
assert.equal(logic.workspaceHealthSummary([], "").Pods.total, 0);
const missingHealth = logic.workspaceHealthSummary([{kind:"Node", name:"missing"}, {kind:"PersistentVolume", name:"unavailable", state:"Unavailable"}], "");
assert.equal(missingHealth.Nodes.states.find(state => state.label === "NotReady").count, 1, "missing readiness never implies a healthy Node");
assert.equal(missingHealth.PersistentVolumes.states.find(state => state.label === "Other / Error").count, 1, "Unavailable never matches Available");

// Navigation, storage-scoped dismissal identifiers and refresh policy.
assert.deepEqual(logic.routeFor("/operations",""), {page:"history",tab:"operations"});
assert.deepEqual(logic.routeFor("/audit",""), {page:"history",tab:"audit"});
assert.deepEqual(logic.routeFor("/history","?tab=audit"), {page:"history",tab:"audit"});
assert.deepEqual(logic.routeFor("/history","?tab=operations"), {page:"history",tab:"operations"});
assert.deepEqual(logic.routeFor("/history","?tab=unknown"), {page:"history",tab:"operations"});
assert.deepEqual(logic.routeFor("/pods",""), {page:"pods",tab:null});
assert.deepEqual(logic.routeFor("/namespaces",""), {page:"namespaces",tab:null});
assert.deepEqual(logic.routeFor("/nodes",""), {page:"nodes",tab:null});
assert.deepEqual(logic.routeFor("/unknown",""), {page:"overview",tab:null});
assert.deepEqual(logic.routeFor("/",""), {page:"overview",tab:null});
assert.equal(logic.queryNamespace("?namespace=team"), "team");
assert.equal(logic.hrefFor("/notebooks","team"), "/notebooks?namespace=team");
assert.equal(logic.historyHref("audit","team"), "/history?tab=audit&namespace=team");
assert.equal(logic.needsRefresh("2026-01-01T00:00:00Z", Date.parse("2026-01-01T00:00:10Z"), 30000), false);
assert.equal(logic.needsRefresh("2026-01-01T00:00:00Z", Date.parse("2026-01-01T00:00:30Z"), 30000), true);
assert.equal(logic.needsRefresh("", Date.now(), 30000), true);
const scopeOne = logic.notificationID("warning","inventory","filesystem usage unavailable");
assert.equal(scopeOne, logic.notificationID("warning","inventory","filesystem usage unavailable"));
assert.notEqual(scopeOne, logic.notificationID("error","inventory","filesystem usage unavailable"));
assert.equal(logic.shouldRememberDismissals("random-session-scope"), true);
assert.equal(logic.shouldRememberDismissals(""), false);
assert.equal(logic.dismissalStorageKey("random-session-scope"), "kf-admin:notifications:v1:random-session-scope");
assert.equal(logic.dismissalStorageKey("random-session-scope").includes("filesystem"), false, "dismissal storage never contains message content");
const initial = logic.defaultTableState();
assert.deepEqual(initial, {search:"",sortKey:"name",sortDir:"asc",page:1});
const sortedOnce = logic.nextSort(initial,"state");
assert.equal(sortedOnce.sortKey, "state"); assert.equal(sortedOnce.sortDir, "asc");
const sortedTwice = logic.nextSort(sortedOnce,"state");
assert.equal(sortedTwice.sortDir, "desc"); assert.equal(sortedTwice.page, 1);
assert.equal(initial.sortKey, "name", "table state helpers are pure");
// Profile names are Kubernetes DNS-1123 labels and must be unique. A typed name
// is never rewritten: a replacement is only offered as a suggestion.
const takenProfiles = ["team", "other"];
const validName = logic.profileNameValidation("research-team", takenProfiles);
assert.equal(validName.valid, true);
assert.equal(validName.message, "");
assert.equal(validName.suggestion, "", "a valid name never carries a suggestion");
assert.equal(logic.profileNameMaxLength, 63);
const label63 = "a".repeat(63);
assert.equal(logic.profileNameValidation(label63, takenProfiles).valid, true, "63 characters is a valid label length");
const label64 = "a".repeat(64);
const tooLong = logic.profileNameValidation(label64, takenProfiles);
assert.equal(tooLong.valid, false, "64 characters is too long");
assert.ok(tooLong.message.includes("63"), "the length limit is stated");
assert.equal(tooLong.suggestion, "");
assert.equal(logic.profileNameValidation("-team", takenProfiles).valid, false, "a leading hyphen is invalid");
assert.ok(logic.profileNameValidation("-team", takenProfiles).message.includes("start and end"));
assert.equal(logic.profileNameValidation("team-", takenProfiles).valid, false, "a trailing hyphen is invalid");
assert.equal(logic.profileNameValidation("Team", takenProfiles).valid, false, "uppercase is invalid");
assert.ok(logic.profileNameValidation("Team", takenProfiles).message.includes("lowercase"));
assert.equal(logic.profileNameValidation("my team", takenProfiles).valid, false, "whitespace is invalid");
assert.equal(logic.profileNameValidation("my/team", takenProfiles).valid, false, "other symbols are invalid");
assert.equal(logic.profileNameValidation("team--x", takenProfiles).valid, true, "inner consecutive hyphens are allowed");
assert.equal(logic.profileNameValidation("a", takenProfiles).valid, true, "a single character is a valid label");
assert.equal(logic.profileNameValidation("1team", takenProfiles).valid, true, "a leading digit is a valid label");
assert.equal(logic.profileNameValidation("9".repeat(63), takenProfiles).valid, true, "63 digits are a valid label");
assert.equal(logic.profileNameValidation("9".repeat(64), takenProfiles).valid, false, "64 digits are too long");
assert.equal(logic.profileNameValidation("", takenProfiles).valid, false);
assert.ok(logic.profileNameValidation("", takenProfiles).message.includes("required"));
assert.equal(logic.profileNameValidation(null, null, null).valid, false, "a null name and null options are safe");
assert.equal(logic.profileNameValidation("team", null, null).valid, true, "null options never throw");
assert.equal(logic.profileNameValidation("team", null, {allowExisting:true}).valid, true, "null names are safe");
const dotted = logic.profileNameValidation("my.team", takenProfiles);
assert.equal(dotted.valid, false, "a dotted name is never accepted");
assert.equal(dotted.suggestion, "my-team", "the exact hyphen replacement is offered");
assert.equal(dotted.name, "my.team", "the typed name is never rewritten");
assert.equal(dotted.duplicate, false);
assert.equal(logic.profileNameValidation("my_team", takenProfiles).suggestion, "my-team");
assert.equal(logic.profileNameValidation("my..team", takenProfiles).suggestion, "my-team");
assert.equal(logic.profileNameValidation("my.team", ["my-team"]).suggestion, "", "a suggestion that is already taken is not offered");
assert.equal(logic.profileNameValidation("my.team", []).valid, false, "a suggestion never makes the typed name valid");
const duplicate = logic.profileNameValidation("team", takenProfiles);
assert.equal(duplicate.valid, false, "an existing profile name cannot be reused");
assert.equal(duplicate.duplicate, true);
assert.ok(duplicate.message.includes("already exists"));
assert.equal(logic.profileNameValidation("team", [{kind:"Profile",name:"team"}]).valid, false, "observed profiles reserve their names");
assert.equal(logic.profileNameValidation("team", [{kind:"Namespace",name:"team"}]).valid, false, "an existing namespace cannot be adopted");
assert.equal(logic.profileNameValidation("team", [{kind:"Notebook",name:"team",namespace:"x"}]).valid, true, "unrelated resources do not reserve a profile name");
assert.equal(logic.profileNameValidation("team", new Set(["team"])).valid, false, "a set of names is accepted");
assert.equal(logic.profileNameValidation("team", takenProfiles, {allowExisting:true}).valid, true, "an update may address an existing profile");
assert.equal(logic.profileNameValidation("team", takenProfiles, {allowExisting:true}).duplicate, false);
assert.equal(logic.profileNameValidation("team", takenProfiles, {allowExisting:true}).suggestion, "");

// Profile form values come from the narrow owner and quota projection only.
const profileResource = {kind:"Profile",name:"team",owner:"alice@example.test",
  details:{apiVersion:"kubeflow.org/v1",ownerKind:"User","quota.requests.cpu":"4","quota.requests.memory":"8Gi"}};
const formValues = logic.profileFormValues(profileResource);
assert.equal(formValues.name, "team");
assert.equal(formValues.owner, "alice@example.test");
assert.equal(formValues["quota.requests.cpu"], "4");
assert.equal(formValues["quota.requests.memory"], "8Gi");
assert.deepEqual(logic.profileQuotaValues(profileResource), {"requests.cpu":"4","requests.memory":"8Gi"});
assert.deepEqual(logic.profileFormValues({kind:"Profile",name:"team"}), {name:"team",owner:""});
assert.equal(logic.profileFormValues({kind:"Profile",name:"team",owner:"  bob@example.test  "}).owner, "  bob@example.test  ", "an observed owner is kept exactly, never trimmed");
assert.equal(logic.profileFormValues({kind:"Profile",name:"team",owner:"Legacy-Owner"}).owner, "Legacy-Owner", "a legacy owner identity is kept exactly");
assert.equal(logic.profileFormValues({kind:"Profile",name:"team",owner:"",details:{owner:"carol@example.test"}}).owner, "carol@example.test");
assert.equal(Object.hasOwn(logic.profileFormValues({kind:"Profile",name:"team",details:{ownerKind:"User"}}), "quota.requests.cpu"), false, "absent quota keys stay absent");
assert.equal(Object.hasOwn(logic.profileFormValues({kind:"Profile",name:"team",details:{ownerKind:"User"}}), "baseConfigDigest"), false, "an absent precondition stays absent");

// The configuration precondition travels with the request and is never shown.
const digestedProfile = {kind:"Profile",name:"team",owner:"alice@example.test",
  details:{apiVersion:"kubeflow.org/v1",ownerKind:"User",configDigest:"sha256:9f2b1c",resourceVersion:"42","quota.requests.cpu":"4"}};
const digestValues = logic.profileFormValues(digestedProfile);
assert.equal(digestValues.baseConfigDigest, "sha256:9f2b1c", "the precondition is a form value");
assert.equal(digestValues.owner, "alice@example.test");
assert.equal(digestValues["quota.requests.cpu"], "4");
// The precondition is never a visible detail.
assert.deepEqual(logic.visibleResourceDetails(digestedProfile), {apiVersion:"kubeflow.org/v1",ownerKind:"User",resourceVersion:"42","quota.requests.cpu":"4"}, "the precondition hash is not a visible detail");
assert.equal(Object.hasOwn(logic.visibleResourceDetails(digestedProfile), "configDigest"), false);
assert.equal(logic.visibleResourceDetails({kind:"Notebook",name:"work",details:{"image.main":"image"}})["image.main"], "image", "other resources keep their visible details");
const updateFromValues = logic.buildAction([["kind","profile.update"], ...Object.entries(digestValues)]);
assert.equal(updateFromValues.baseConfigDigest, "sha256:9f2b1c", "the precondition round-trips into the request");
assert.equal(updateFromValues.owner, "alice@example.test", "the unchanged owner round-trips into the request");
assert.deepEqual(updateFromValues.quota, {"requests.cpu":"4"});

// A profile update carries the owner and the quotas; a quota-only change
// keeps its existing shape and still drops the owner.
assert.deepEqual(logic.buildAction([["kind","profile.update"],["name","team"],["owner","alice@example.test"],["quota.requests.cpu","4"],["quota.requests.memory",""]]),
  {kind:"profile.update",name:"team",owner:"alice@example.test",quota:{"requests.cpu":"4"}});
assert.deepEqual(logic.buildAction([["kind","profile.update"],["name","team"],["quota.requests.cpu","4"]]),
  {kind:"profile.update",name:"team",quota:{"requests.cpu":"4"}}, "an empty owner field is not sent");
assert.equal(logic.buildAction([["kind","profile.update"],["name","team"],["owner","Legacy-Owner"]]).owner, "Legacy-Owner", "a legacy owner identity is sent exactly");
assert.equal(logic.buildAction([["kind","profile.update"],["name","team"],["owner","  legacy@example.test  "]]).owner, "  legacy@example.test  ", "an owner is never trimmed on an update");
// A legacy caller that omits the guard is encoded exactly as it is; the
// server refuses that request, so the console never pretends it is valid.
assert.deepEqual(logic.buildAction([["kind","profile.quota"],["quota.requests.cpu","2"]]),
  {kind:"profile.quota.change",quota:{"requests.cpu":"2"}}, "an unguarded legacy caller is only encoded, not a valid server operation");
assert.equal(logic.buildAction([["kind","profile.quota"],["owner","alice@example.test"],["baseConfigDigest","sha256:9f2b1c"]]).owner, "alice@example.test", "the legacy alias preserves the raw owner");
assert.equal(logic.buildAction([["kind","profile.quota"],["owner","  legacy@example.test  "]]).owner, "  legacy@example.test  ", "the legacy alias never trims the owner");
const noisyLegacy = logic.buildAction([["kind","profile.quota"],["name","team"],["owner","alice@example.test"],["baseConfigDigest","sha256:9f2b1c"],["quota.requests.cpu","4"],["storageClass","team-class"],["resourceVersion","42"],["confirmName","team"],["namespace","team"]]);
assert.deepEqual(Object.keys(noisyLegacy).sort(), ["baseConfigDigest","kind","name","namespace","owner","quota"], "the legacy alias carries only its own fields");
assert.equal(noisyLegacy.storageClass, undefined, "a stale storage field is stripped from the legacy alias");
assert.equal(noisyLegacy.resourceVersion, undefined, "a stale resource version is stripped from the legacy alias");
assert.equal(noisyLegacy.confirmName, undefined, "a stale confirmation is stripped from the legacy alias");
assert.equal(logic.buildAction([["kind","profile.quota"],["name","team"],["namespace","other"]]).namespace, undefined, "an inconsistent namespace is dropped from the legacy alias");
const deleteAction = logic.buildAction([["kind","profile.delete"],["name","team"],["reason","  decommission  "],["confirmName","team"],["owner","alice@example.test"],["quota.requests.cpu","4"]]);
assert.deepEqual(deleteAction, {kind:"profile.delete",name:"team",reason:"decommission",confirmName:"team"});
assert.equal(Object.hasOwn(deleteAction,"owner"), false, "a deletion carries no owner");
assert.equal(Object.hasOwn(deleteAction,"quota"), false, "a deletion carries no quota");
// A profile action carries only the fields its kind defines.
const noisyUpdate = logic.buildAction([["kind","profile.update"],["name","team"],["owner","alice@example.test"],["quota.requests.cpu","4"],["baseConfigDigest","sha256:9f2b1c"],["reason","rebalance"],["storageClass","team-class"],["resourceVersion","42"],["confirmName","team"],["namespace","team"]]);
assert.deepEqual(Object.keys(noisyUpdate).sort(), ["baseConfigDigest","kind","name","namespace","owner","quota","reason"], "an update carries only its own fields");
assert.equal(noisyUpdate.namespace, "team", "a namespace consistent with the profile name is kept");
assert.equal(logic.buildAction([["kind","profile.update"],["name","team"],["namespace","other"]]).namespace, undefined, "an inconsistent namespace is dropped");
assert.deepEqual(Object.keys(logic.buildAction([["kind","profile.delete"],["name","team"],["reason","decommission"],["confirmName","team"],["owner","alice@example.test"],["quota.requests.cpu","4"],["baseConfigDigest","sha256:9f2b1c"],["namespace","team"]])).sort(), ["confirmName","kind","name","reason"], "a deletion carries only its own fields");
const staleDigestDelete = logic.buildAction([["kind","profile.delete"],["name","team"],["reason","decommission"],["confirmName","team"],["baseConfigDigest","sha256:9f2b1c"]]);
assert.equal(Object.hasOwn(staleDigestDelete,"baseConfigDigest"), false, "a deletion drops a stale precondition");
assert.equal(staleDigestDelete.confirmName, "team", "the exact confirmation survives the stripped fields");
// The confirmation is typed exactly: it is never trimmed, so a padded or
// differently cased name reaches the server unchanged and is refused there.
assert.equal(logic.buildAction([["kind","profile.delete"],["name","team"],["reason","decommission"],["confirmName"," team "]]).confirmName, " team ", "a padded confirmation is sent unchanged");
assert.equal(logic.buildAction([["kind","profile.delete"],["name","team"],["reason","decommission"],["confirmName","Team"]]).confirmName, "Team", "a differently cased confirmation is sent unchanged");
assert.equal(logic.buildAction([["kind","profile.delete"],["name","team"],["reason","  decommission  "]]).reason, "decommission", "the reason is trimmed");

// A deletion needs a mandatory reason and an exact name confirmation.
assert.equal(logic.profileDeleteValidation("team",{reason:"",confirmName:"team"}).valid, false);
assert.ok(logic.profileDeleteValidation("team",{reason:"",confirmName:"team"}).message.includes("reason"));
assert.equal(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:""}).valid, false);
assert.equal(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:"other"}).valid, false);
assert.ok(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:"other"}).message.includes("does not match"));
assert.equal(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:" team "}).valid, false, "a padded confirmation is refused, not sanitized");
assert.equal(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:"Team"}).valid, false, "a differently cased confirmation is refused");
assert.equal(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:"team"}).valid, true);
assert.equal(logic.profileDeleteValidation("team",{reason:"cleanup",confirmName:"team"}).message, "");
assert.equal(logic.profileDeleteValidation("team", null).valid, false, "null inputs are safe");
assert.equal(logic.profileDeleteValidation(null, {reason:"cleanup",confirmName:"team"}).valid, false, "a null profile name is safe");
assert.equal(logic.buildAction(null, null).kind, undefined, "a null entry list and null options are safe");
assert.deepEqual(logic.buildAction([["kind","profile.update"],["name","team"],["quota.requests.cpu","4"],["quota.","5"]]).quota, {"requests.cpu":"4"}, "an empty quota key is never a quota entry");

// The legacy quota alias previews the same read-only owner contract and never
// renders the internal precondition hash.
const legacyPreview = JSON.stringify(logic.previewFields({...plan,prepared:{...plan.prepared,action:logic.buildAction([["kind","profile.quota"],["name","team"],["owner","alice@example.test"],["baseConfigDigest","sha256:9f2b1c"],["quota.requests.cpu","2"]])}}));
assert.ok(legacyPreview.includes("profile.quota.change"), "the legacy alias keeps its deprecated kind in the preview");
assert.ok(legacyPreview.includes("Current owner (read-only)"));
assert.ok(legacyPreview.includes("Owner transfer is unsupported by the installed Profile controller; the current owner is read-only. Only quotas are changed."));
assert.equal(legacyPreview.includes("Owner identity"), false, "the legacy preview has no owner edit label");
assert.equal(legacyPreview.includes("sha256:9f2b1c"), false, "the legacy preview never shows the precondition hash");
assert.ok(legacyPreview.includes("Quota keys") && legacyPreview.includes("alice@example.test"), "the legacy preview still shows the owner and the quota contract");

// Plan previews explain an update and warn about an incomplete deletion.
const updatePreview = JSON.stringify(logic.previewFields({...plan,prepared:{...plan.prepared,action:updateFromValues}}));
assert.ok(updatePreview.includes("alice@example.test") && updatePreview.includes("Quota targets"));
assert.ok(updatePreview.includes("Current owner (read-only)"));
assert.ok(updatePreview.includes("Owner transfer is unsupported by the installed Profile controller; the current owner is read-only. Only quotas are changed."));
assert.equal(updatePreview.includes("Owner comparison"), false, "the preview never suggests an owner change");
assert.equal(updatePreview.includes("sha256:9f2b1c"), false, "the internal precondition hash is never rendered");
const deletePreview = JSON.stringify(logic.previewFields({...plan,prepared:{...plan.prepared,action:deleteAction}}));
assert.ok(deletePreview.includes("Confirmation name") && deletePreview.includes("team"));
assert.ok(deletePreview.includes("Requests the deletion"), "the preview asks for a deletion, it does not claim a direct removal");
assert.ok(deletePreview.includes("UID and resource version") && deletePreview.includes("never deletes the namespace directly"));
assert.ok(deletePreview.includes("never removes a finalizer"));
assert.ok(deletePreview.includes("cannot skip namespaced objects"), "namespace cleanup does not spare unowned namespaced objects");
assert.ok(deletePreview.includes("only after the Profile and its namespace are both absent"), "success requires both objects to be gone");
assert.ok(deletePreview.includes("unresolved finalizer") && deletePreview.includes("left for the operator"), "a stalled cleanup is left for the operator");
assert.ok(deletePreview.includes("Deletes the volume claims inside it") || deletePreview.includes("deletes the volume claims inside it"), "namespace cleanup deletes the claims");
assert.ok(deletePreview.includes("Delete reclaim policy") && deletePreview.includes("irreversible"));
assert.ok(deletePreview.includes("Retain reclaim policy is not protection"), "a Retain policy is not protection from the backend lifecycle or a later operator delete");
assert.ok(deletePreview.includes("namespaced VolumeSnapshot is deleted with its namespace"), "a namespaced snapshot goes with the namespace");
assert.ok(deletePreview.includes("snapshot content behind it follows its own Delete or Retain policy"), "the snapshot content keeps its own policy");
assert.ok(deletePreview.includes("NFS") && deletePreview.includes("external storage"));
assert.ok(deletePreview.includes("never wipes backend data"));
assert.equal(deletePreview.includes("The server removes the Profile and the namespace"), false, "the preview never claims a direct namespace removal");
assert.equal(deletePreview.includes("guaranteed"), false, "the preview never promises a complete erase");
assert.equal(deletePreview.includes("permanently deleted"), false);
assert.equal(deletePreview.includes("all data is removed"), false);

// The observed quota keys of an existing Profile. The cpu and memory aliases
// are standard Kubernetes quota resources, a device key of the Profile's own is
// an extended device resource, and everything else stays operator managed.
const legacyQuotas = { cpu: "128", memory: "256Gi", "nvidia.com/gpu": "4", persistentvolumeclaims: "20", "requests.storage": "500Gi" };
assert.equal(logic.profileQuotaKind("cpu", []), "builtin", "the cpu alias is a builtin quota resource");
assert.equal(logic.profileQuotaKind("memory", []), "builtin", "the memory alias is a builtin quota resource");
assert.equal(logic.profileQuotaKind("requests.storage", []), "builtin", "the storage request is a builtin quota resource");
assert.equal(logic.profileQuotaKind("nvidia.com/gpu", []), "device", "an observed device key is a device resource");
assert.equal(logic.profileQuotaKind("example.com/gpu", []), "device", "an observed extended device key is a device resource");
assert.equal(logic.profileQuotaKind("requests.nvidia.com/gpu", []), "unmanaged", "a canonical device key without an approved preset stays unmanaged");
assert.equal(logic.profileQuotaKind("requests.nvidia.com/gpu", ["requests.nvidia.com/gpu"]), "device", "an approved preset key is a device resource");
assert.equal(logic.profileQuotaKind("foo.storageclass.storage.k8s.io/requests.storage", []), "unmanaged", "a reserved storage capacity key stays unmanaged");
assert.equal(logic.profileQuotaKind("storage.k8s.io/requests.storage", []), "unmanaged", "a reserved cluster domain stays unmanaged");
assert.equal(logic.profileQuotaKind("requests.ephemeral-storage", []), "unmanaged", "an unmanaged canonical key stays unmanaged");
assert.equal(logic.profileQuotaCounts("nvidia.com/gpu", []), true, "a device key counts whole devices");
assert.equal(logic.profileQuotaCounts("pods", []), true, "a pod count counts whole objects");
assert.equal(logic.profileQuotaCounts("cpu", []), false, "a cpu alias measures a resource");
assert.equal(logic.deviceQuotaKey("nvidia.com/gpu"), true);
assert.equal(logic.deviceQuotaKey("requests.nvidia.com/gpu"), false, "a canonical device key is never a raw device key");
assert.equal(logic.deviceQuotaKey("cpu"), false, "a bare name is never a device key");
// The submitted payload keeps the observed key names. No canonical key is
// added beside an observed alias and nothing is renamed.
const legacyPayload = buildAction([
  ["kind", "profile.update"], ["name", "legacy"], ["owner", "legacy.owner@example.test"],
  ["baseConfigDigest", "sha256:legacydigest"],
  ["quota.cpu", "96"], ["quota.memory", "256Gi"], ["quota.nvidia.com/gpu", "2"],
  ["quota.persistentvolumeclaims", "20"], ["quota.requests.storage", "500Gi"]
]);
assert.deepEqual(legacyPayload.quota, { cpu: "96", memory: "256Gi", "nvidia.com/gpu": "2", persistentvolumeclaims: "20", "requests.storage": "500Gi" },
  "the payload carries exactly the observed key names");
assert.equal(legacyPayload.quota["requests.cpu"], undefined, "no canonical cpu key is added beside the observed alias");
assert.equal(legacyPayload.quota["requests.memory"], undefined, "no canonical memory key is added beside the observed alias");
assert.equal(legacyPayload.quota["requests.nvidia.com/gpu"], undefined, "no canonical device key is added beside the observed one");
console.log("Dashboard action, exact preview, typed sorting, target grammar, profile counts, profile naming, audit grouping and navigation tests passed.");
