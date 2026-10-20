"use strict";
const assert = require("node:assert/strict");
const logic = require("../assets/logic.js");
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
assert.deepEqual(buildAction([["kind","profile.quota"],["owner","unused"],["quota.requests.cpu","2"],["quota.requests.memory",""]]), {kind:"profile.quota.change",quota:{"requests.cpu":"2"}});
assert.equal(buildAction([["kind","profile.create"],["owner","CaseSensitive-Username"]]).owner,"CaseSensitive-Username");
assert.deepEqual(buildAction([["kind","pod.restart"],["namespace","team"],["name","work-0"],["reason","  recover stalled kernel  "]]), {kind:"pod.restart",namespace:"team",name:"work-0",reason:"recover stalled kernel"});
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
const plan = {id:"server-plan",actor:"issuer|admin",actorDisplay:"Alice Admin",prepared:{summary:"server summary",action:clear,warnings:["warning"],data:{policy:"DO_NOT_RENDER_OPAQUE_HASH",dependencies:[{digest:"also-opaque"}]}}};
const preview = previewFields(plan), text = JSON.stringify(preview);
assert.equal(text.includes("DO_NOT_RENDER_OPAQUE_HASH"),false); assert.equal(text.includes("also-opaque"),false);
assert.equal(preview.find(([label])=>label==="GPU count")[1],0);
assert.deepEqual(preview.find(([label])=>label==="Replacement PVC mounts")[1],[]);
assert.equal(preview.find(([label])=>label==="Actor")[1],"Alice Admin");
assert.equal(preview.find(([label])=>label==="Actor ID")[1],"issuer|admin");
assert.ok(text.includes("500m") && text.includes("4Gi") && text.includes("Remove all PVC mounts"));
assert.ok(text.includes("previously running"));
const preservePreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:unchanged}}));
assert.ok(preservePreview.includes("Unchanged (replacement not requested)") && preservePreview.includes("Unchanged (not specified)"));
assert.ok(preservePreview.includes("leave the notebook stopped"));
const podPreview = JSON.stringify(previewFields({...plan,prepared:{...plan.prepared,action:buildAction([["kind","pod.restart"],["namespace","team"],["name","work-0"],["reason","recover"]])}}));
assert.ok(podPreview.includes("pod.restart") && podPreview.includes("recover"));
assert.ok(podPreview.includes("disruption-budget-aware eviction"));
assert.ok(podPreview.includes("Force deletion") && podPreview.includes("Never used automatically"));

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
assert.equal(logic.actorLabel({actor:"issuer|subject"}), "subject", "legacy labels use the subject after |, not the issuer key");
assert.equal(logic.actorID({actor:"issuer|subject"}), "issuer|subject", "full issuer key stays secondary metadata");
assert.equal(logic.actorLabel({actor:"https://issuer.example|" + "a".repeat(400)}).length, 160, "legacy labels are bounded");
assert.equal(logic.actorLabel({}), "Unknown administrator");

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
  {kind:"ResourceQuota",name:"quota",namespace:"team"}, {kind:"ResourceQuota",name:"quota",namespace:"kube-system"}
];
assert.deepEqual(logic.profileSummary(profileList,"team").map(entry=>[entry.label,entry.count]), [["Notebooks",1],["Pods",1],["Persistent volume claims",1]]);
assert.deepEqual(logic.profileSummary(profileList,"empty"), [], "zero counts are omitted, never inferred");
const visibleProfiles = logic.profileResources(profileList, "").map(resource => resource.kind + ":" + (resource.namespace ? resource.namespace + "/" : "") + resource.name).sort();
assert.deepEqual(visibleProfiles, ["Namespace:team","Profile:empty","Profile:team"], "Profiles lists only Profiles and name-associated namespaces");
assert.deepEqual(logic.namespaceOptions(profileList), ["empty","kube-system","team"]);

// Navigation, storage-scoped dismissal identifiers and refresh policy.
assert.deepEqual(logic.routeFor("/operations",""), {page:"history",tab:"operations"});
assert.deepEqual(logic.routeFor("/audit",""), {page:"history",tab:"audit"});
assert.deepEqual(logic.routeFor("/history","?tab=audit"), {page:"history",tab:"audit"});
assert.deepEqual(logic.routeFor("/history","?tab=operations"), {page:"history",tab:"operations"});
assert.deepEqual(logic.routeFor("/history","?tab=unknown"), {page:"history",tab:"operations"});
assert.deepEqual(logic.routeFor("/pods",""), {page:"pods",tab:null});
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
console.log("Dashboard action, exact preview, typed sorting, target grammar, profile counts, audit grouping and navigation tests passed.");
