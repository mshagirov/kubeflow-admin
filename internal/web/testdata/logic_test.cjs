"use strict";
const assert = require("node:assert/strict");
const {buildAction, scopedResources, containerNames, previewFields} = require("../assets/logic.js");
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
assert.deepEqual(buildAction([["kind","profile.quota"],["owner","unused"],["quota.requests.cpu","2"],["quota.requests.memory",""]]), {kind:"profile.quota.change",quota:{"requests.cpu":"2"}});
assert.equal(buildAction([["kind","profile.create"],["owner","CaseSensitive-Username"]]).owner,"CaseSensitive-Username");
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
const plan = {id:"server-plan",actor:"issuer|admin",prepared:{summary:"server summary",action:clear,warnings:["warning"],data:{policy:"DO_NOT_RENDER_OPAQUE_HASH",dependencies:[{digest:"also-opaque"}]}}};
const preview = previewFields(plan), text = JSON.stringify(preview);
assert.equal(text.includes("DO_NOT_RENDER_OPAQUE_HASH"),false); assert.equal(text.includes("also-opaque"),false);
assert.equal(preview.find(([label])=>label==="GPU count")[1],0);
assert.deepEqual(preview.find(([label])=>label==="Replacement PVC mounts")[1],[]);
assert.ok(text.includes("500m") && text.includes("4Gi") && text.includes("Remove all PVC mounts"));
assert.ok(text.includes("previously running"));
const preservePreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:unchanged}}));
assert.ok(preservePreview.includes("Unchanged (replacement not requested)") && preservePreview.includes("Unchanged (not specified)"));
assert.ok(preservePreview.includes("leave the notebook stopped"));
console.log("Dashboard action, exact preview, detail mapping and namespace tests passed.");
