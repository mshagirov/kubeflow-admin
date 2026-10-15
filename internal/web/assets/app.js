"use strict";
(() => {
  const $ = (s, root = document) => root.querySelector(s);
  const $$ = (s, root = document) => [...root.querySelectorAll(s)];
  const el = (tag, text, cls) => { const n = document.createElement(tag); if (text !== undefined) n.textContent = text; if (cls) n.className = cls; return n; };
  const csrf = $('meta[name="csrf-token"]').content;
  const notebookWrites = $('meta[name="notebook-writes"]').content === "true";
  const logic = globalThis.DashboardLogic;
  const titles = { overview: ["Overview", "A clear view of your Kubeflow workspace."], profiles: ["Profiles", "Manage ownership, namespaces and resource quotas."], storage: ["Storage", "Provision and grow persistent storage with explicit safeguards."], notebooks: ["Notebooks", "Manage workloads and resources with observed-state verification."], operations: ["Operations", "Track changes from submission to verified convergence."], audit: ["Audit trail", "Accountable administrative changes, recorded durably."] };
  const page = location.pathname.slice(1) || "overview";
  let inventory = null, currentPlan = null, submitKey = null, loading = false;
  const notice = (text, error = false) => { const n = $("#notice"); n.textContent = text; n.className = error ? "error" : "success"; n.hidden = false; };
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
    return data;
  }
  function badge(state) { const s = String(state || "Not reported"); return el("span", s, "badge" + (/^(bound|available|running|active|succeeded)|ready/i.test(s) ? " good" : /failed|error|review|pending|stopping|warning/i.test(s) ? " bad" : "")); }
  function table(target, headings, rows) {
    const parent = $(target); parent.replaceChildren();
    if (!rows.length) { parent.append(el("div", "No matching records. Choose another namespace or refresh inventory.", "empty")); return; }
    const wrap = el("div", undefined, "table-wrap"), t = el("table"), head = el("thead"), tr = el("tr"), body = el("tbody");
    headings.forEach(h => tr.append(el("th", h))); head.append(tr); t.append(head);
    rows.forEach(row => { const r = el("tr"); row.forEach(value => { const td = el("td"); if (value instanceof Node) td.append(value); else td.textContent = value || "—"; r.append(td); }); body.append(r); });
    t.append(body); wrap.append(t); parent.append(wrap);
  }
  function details(resource) {
    const node = el("div"), d = resource.details || {}, name = el("span", resource.name); node.append(name);
    node.append(el("small", [resource.kind, resource.namespace, resource.owner].filter(Boolean).join(" · ")));
    if (Object.keys(d).length) {
      const disclosure = el("details"), summary = el("summary", "Details", "detail-button"), content = el("div", undefined, "details");
      content.textContent = Object.entries(d).sort().map(([k, v]) => `${k}: ${v}`).join("\n"); disclosure.append(summary, content); node.append(disclosure);
    }
    return node;
  }
  function desired(resource) { return resource.desired || resource.details?.requested || "Not reported"; }
  function selectedResources() { return logic.scopedResources(inventory?.resources || [], $("#namespace-filter").value); }
  function resourceRows(resources) { return resources.map(r => [details(r), desired(r), badge(r.state), r.details?.capacity || r.details?.["condition.Ready"] || "—"]); }
  function fillSelect(select, options, placeholder) {
    const old = select.value; select.replaceChildren(); if (placeholder !== undefined) select.append(new Option(placeholder, ""));
    [...new Set(options)].sort().forEach(v => select.append(new Option(v, v))); if ([...select.options].some(o => o.value === old)) select.value = old;
  }
  function scopeNamespaces() {
    const resources = inventory?.resources || [];
    return resources.filter(r => r.kind === "Profile" || r.kind === "Namespace").map(r => r.name).concat(resources.map(r => r.namespace).filter(Boolean));
  }
  function selectors(applyFilter = false) {
    const ns = $("#namespace-filter").value, resources = inventory?.resources || [];
    $$('[data-namespaces]').forEach(s => { fillSelect(s, scopeNamespaces(), "Select a namespace"); if (ns && (applyFilter || !s.value)) s.value = ns; });
    if (applyFilter) { $("#mount-rows").replaceChildren(); $("#replace-mounts").checked = false; }
    $("#profile-names").replaceChildren(...resources.filter(r => r.kind === "Profile").map(r => new Option(r.name, r.name)));
    $("#claim-names").replaceChildren(...resources.filter(r => r.kind === "PersistentVolumeClaim" && (!ns || r.namespace === ns)).map(r => new Option(r.name, r.name)));
    fillSelect($("#pv-selector"), resources.filter(r => r.kind === "PersistentVolume" && r.state === "Available" && !r.details?.claim && r.details?.reclaimPolicy === "Retain").map(r => r.name), "Select an available Retain PV");
    notebookOptions();
  }
  function notebookOptions() {
    const ns = $('#notebook-form [name="namespace"]').value;
    fillSelect($("#notebook-selector"), (inventory?.resources || []).filter(r => r.kind === "Notebook" && r.namespace === ns).map(r => r.name), "Select a notebook"); containerOptions();
  }
  function containerOptions() {
    const ns = $('#notebook-form [name="namespace"]').value, name = $("#notebook-selector").value;
    const notebook = (inventory?.resources || []).find(r => r.kind === "Notebook" && r.namespace === ns && r.name === name);
    const names = logic.containerNames(notebook);
    fillSelect($("#container-selector"), names, "Select the exact named container");
    if (names.length === 1) $("#container-selector").value = names[0];
  }
  function render() {
    const resources = selectedResources();
    const groups = [["Profiles", "Profile", "Workspace ownership"], ["Notebooks", "Notebook", "Desired and observed lifecycle"], ["Volume claims", "PersistentVolumeClaim", "Persistent workspace storage"], ["Storage classes", "StorageClass", "Provisioning policies"]];
    $("#metrics").replaceChildren(...groups.map(([label, kind, caption]) => { const m = el("div", undefined, "metric"); m.append(el("small", label), el("strong", String(resources.filter(r => r.kind === kind).length)), el("span", caption)); return m; }));
    table("#overview-table", ["Resource", "Desired / requested", "Observed", "Capacity / condition"], resourceRows(resources.filter(r => ["Notebook", "PersistentVolumeClaim", "Pod", "ResourceQuota", "Event"].includes(r.kind))));
    table("#profiles-table", ["Resource", "Desired / requested", "Observed", "Capacity / condition"], resourceRows(resources.filter(r => ["Profile", "Namespace", "ResourceQuota", "LimitRange", "RoleBinding"].includes(r.kind))));
    table("#storage-table", ["Resource", "Desired / requested", "Observed", "Capacity / condition"], resourceRows(resources.filter(r => ["PersistentVolume", "PersistentVolumeClaim", "StorageClass"].includes(r.kind))));
    table("#notebooks-table", ["Resource", "Desired / requested", "Observed", "Capacity / condition"], resourceRows(resources.filter(r => ["Notebook", "Pod", "StatefulSet"].includes(r.kind))));
  }
  function updateTimestamp() {
    if (!inventory) return;
    const time = new Date(inventory.observedAt), age = Date.now() - time.getTime(), stale = !Number.isFinite(age) || age > 60000;
    const out = $("#observed-at"); out.textContent = `${stale ? "STALE · " : "Observed · "}${Number.isFinite(age) ? time.toLocaleString() : "Unknown timestamp"}`; out.classList.toggle("stale", stale);
  }
  async function history() {
    if (page !== "operations" && page !== "audit") return;
    const ns = $("#namespace-filter").value;
    if (page === "operations") {
      const data = await api("operations");
      table("#operations-table", ["Target / action", "State", "Current step", "Updated", "Result"], (data || []).filter(o => !ns || o.target.includes(":" + ns + "/") || o.target === "profile:" + ns).map(o => {
        const n = el("div", o.target); n.append(el("small", o.action), el("small", o.id)); return [n, badge(o.state), o.step, new Date(o.updatedAt).toLocaleString(), o.error || (o.state === "succeeded" ? "Observed convergence verified" : "Not yet verified")];
      }));
    } else {
      const data = await api("audit");
      table("#audit-table", ["Time", "Actor", "Action / target", "Result", "Reason / operation"], (data || []).filter(a => !ns || a.target.includes(":" + ns + "/") || a.target === "profile:" + ns).map(a => [new Date(a.time).toLocaleString(), a.actor, `${a.action} · ${a.target}`, badge(a.result), [a.reason, a.operationID].filter(Boolean).join(" · ")]));
    }
  }
  async function refresh() {
    if (loading || document.hidden) return; loading = true; $("#refresh").disabled = true;
    try {
      if (!inventory || !["operations", "audit"].includes(page)) {
        inventory = await api("inventory"); fillSelect($("#namespace-filter"), scopeNamespaces(), "All permitted namespaces");
        selectors(); render(); updateTimestamp(); const warnings = inventory.warnings || [];
        $("#warnings").textContent = warnings.join(" "); $("#warnings").hidden = warnings.length === 0;
      }
      await history();
    } catch (e) { notice(e.message + (inventory ? " Previous observations remain visible and may be stale." : ""), true); }
    finally { loading = false; $("#refresh").disabled = false; }
  }
  function storageFields() {
    const kind = $("#storage-kind").value;
    $$('[data-storage]').forEach(label => { const visible = label.dataset.storage.split(" ").includes(kind); label.hidden = !visible; $$("input,select", label).forEach(i => i.disabled = !visible); });
    const template = $("#storage-template").selectedOptions[0];
    if (kind === "pv.nfs.create" && template?.dataset.type === "csi") { $("#subdirectory-field").hidden = true; $("#subdirectory-field input").disabled = true; }
    $('#storage [name="size"]').required = true;
    $("#pv-selector").required = kind === "pv.reserve";
    $("#storage-template").required = kind === "pv.nfs.create";
    $("#subdirectory-field input").required = kind === "pv.nfs.create" && template?.dataset.type !== "csi";
    $("#storage-class").required = kind === "pvc.create";
  }
  function notebookFields() {
    const edit = $("#notebook-kind").value === "notebook.edit"; $("#notebook-edit").hidden = !edit;
    $$("input,select,button", $("#notebook-edit")).forEach(i => i.disabled = !edit);
    $("#container-selector").required = edit;
    mountFields();
  }
  function profileFields() { const owner = $('#profile-form [name="owner"]'); owner.disabled = $("#profile-kind").value !== "profile.create"; owner.required = !owner.disabled; }
  function mountFields() { const enabled = $("#notebook-kind").value === "notebook.edit" && $("#replace-mounts").checked; $$("#mount-rows input,#mount-rows select").forEach(i => i.disabled = !enabled); }
  // A new static PV has its own name, separate from the new PVC name. The
  // existing-PV selector is intentionally only enabled for reservations.
  const newPVLabel = el("label", "New PV name"); newPVLabel.dataset.storage = "pv.nfs.create";
  const newPVInput = el("input"); newPVInput.name = "pvName"; newPVInput.required = true; newPVInput.placeholder = "team-workspace-pv";
  newPVLabel.append(newPVInput); $("#pv-selector").parentElement.before(newPVLabel);
  function addMount() {
    const row = el("div", undefined, "mount-row");
    const ns = $('#notebook-form [name="namespace"]').value;
    const name = el("input"); name.dataset.field = "name"; name.required = true; name.placeholder = "volume-name";
    const claim = el("select"); claim.dataset.field = "claim"; claim.required = true;
    fillSelect(claim, (inventory?.resources || []).filter(r => r.kind === "PersistentVolumeClaim" && r.namespace === ns).map(r => r.name), "Select PVC in this namespace");
    const path = el("input"); path.dataset.field = "path"; path.required = true; path.placeholder = "/home/jovyan/data"; path.pattern = "/.*";
    [["Volume name", name], ["Claim", claim], ["Mount path", path]].forEach(([title, input]) => { const label = el("label", title); label.append(input); row.append(label); });
    const ro = el("input"); ro.type = "checkbox"; ro.dataset.field = "readOnly"; const label = el("label", undefined, "check"); label.append(ro, document.createTextNode("Read only")); row.append(label);
    const remove = el("button", "Remove mount", "secondary"); remove.type = "button"; remove.addEventListener("click", () => row.remove()); row.append(remove); $("#mount-rows").append(row); $("#replace-mounts").checked = true; mountFields();
  }
  function actionFrom(form) {
    return logic.buildAction(new FormData(form), {
      restart: !!$('[name="restart"]', form)?.checked,
      replaceMounts: form.id === "notebook-form" && $("#replace-mounts").checked,
      mounts: $$(".mount-row").map(row => Object.fromEntries($$("[data-field]", row).map(i => [i.dataset.field, i.type === "checkbox" ? i.checked : i.value])))
    });
  }
  function preview(plan) {
    const root = $("#plan-preview"); root.replaceChildren();
    const dl = el("dl", undefined, "preview-fields");
    logic.previewFields(plan).forEach(([k, v]) => { dl.append(el("dt", k), el("dd", typeof v === "object" ? JSON.stringify(v, null, 2) : String(v ?? "—"))); }); root.append(dl);
    if (plan.prepared.changes?.length) {
      root.append(el("h3", "Validated before / after"));
      const changes = el("div"); changes.id = "plan-changes"; root.append(changes);
      table("#plan-changes", ["Allowed field", "Before", "After"], plan.prepared.changes.map(change => [change.field, change.before, change.after]));
    }
    (plan.prepared.warnings || []).forEach(w => root.append(el("div", w, "warning")));
  }
  $$('[data-action-form]').forEach(form => form.addEventListener("submit", async event => {
    event.preventDefault(); const button = $('button[type="submit"]', form); button.disabled = true;
    try {
      const plan = await api("plans", actionFrom(form)); currentPlan = plan; submitKey = crypto.randomUUID(); preview(plan);
      $("#confirm-plan").checked = false; $("#submit-plan").disabled = true; $("#submit-status").textContent = ""; $("#plan-dialog").showModal();
    } catch (e) { notice(e.message, true); } finally { button.disabled = document.body.dataset.writes !== "true" || (form.id === "notebook-form" && !notebookWrites); }
  }));
  $("#confirm-plan").addEventListener("change", () => { $("#submit-plan").disabled = !$("#confirm-plan").checked || !currentPlan || Date.now() >= new Date(currentPlan.expiresAt).getTime(); });
  $("#cancel-plan").addEventListener("click", () => $("#plan-dialog").close());
  $("#submit-plan").addEventListener("click", async () => {
    if (!currentPlan || !$("#confirm-plan").checked) return;
    if (Date.now() >= new Date(currentPlan.expiresAt).getTime()) { $("#submit-status").textContent = "This plan has expired. Cancel and prepare a new plan."; $("#submit-plan").disabled = true; return; }
    $("#submit-plan").disabled = true; $("#cancel-plan").disabled = true;
    try {
      const op = await api("operations", { planID: currentPlan.id, idempotencyKey: submitKey }, submitKey);
      $("#plan-dialog").close(); currentPlan = null; notice(`Operation ${op.id} accepted (${op.state}). Watch Operations for observed convergence.`); await refresh();
    } catch (e) {
      // Preserve the same plan/key after an ambiguous network failure: retry is safe.
      $("#submit-status").textContent = e.message + (e.retryable === false ? " Cancel to review the target or prepare a new plan; check Operations first." : " Check Operations before preparing another plan. Retrying here reuses the same idempotency key.");
      $("#submit-plan").disabled = e.retryable === false;
    } finally { $("#cancel-plan").disabled = false; }
  });
  $("#namespace-filter").addEventListener("change", () => { selectors(true); render(); history().catch(e => notice(e.message, true)); });
  $("#refresh").addEventListener("click", refresh);
  $("#storage-kind").addEventListener("change", storageFields); $("#notebook-kind").addEventListener("change", notebookFields);
  $("#profile-kind").addEventListener("change", profileFields);
  $("#storage-template").addEventListener("change", () => { const option = $("#storage-template").selectedOptions[0]; if (option?.dataset.class) $("#storage-class").value = option.dataset.class; storageFields(); });
  $("#pv-selector").addEventListener("change", () => { const volume = (inventory?.resources || []).find(r => r.kind === "PersistentVolume" && r.name === $("#pv-selector").value); if (volume) $("#storage-class").value = volume.details?.storageClass || ""; });
  $('#notebook-form [name="namespace"]').addEventListener("change", () => { notebookOptions(); $("#mount-rows").replaceChildren(); $("#replace-mounts").checked = false; });
  $("#notebook-selector").addEventListener("change", containerOptions); $("#add-mount").addEventListener("click", addMount);
  $("#replace-mounts").addEventListener("change", mountFields);
  $('#replace-mounts').closest('fieldset').querySelector('.hint').textContent = "Unchecked preserves current mounts. Checked with no rows requests removal of all PVC mounts from this container; the server rejects removing protected workspace mounts.";
  $("#page-title").textContent = titles[page]?.[0] || "Overview"; $("#page-subtitle").textContent = titles[page]?.[1] || "";
  $$(".page").forEach(p => p.hidden = p.id !== page); $$('nav a').forEach(a => a.classList.toggle("active", a.dataset.page === page));
  const quotaKeys = new Set(); $$('[data-gpu-quota]').forEach(label => { if (quotaKeys.has(label.dataset.gpuQuota)) label.remove(); else quotaKeys.add(label.dataset.gpuQuota); });
  storageFields(); notebookFields(); profileFields();
  if (document.body.dataset.writes !== "true") $$('[data-action-form] button[type="submit"]').forEach(b => b.disabled = true);
  if (!notebookWrites) { const message = el("div", "Notebook changes disabled: the installed controller contract has not been confirmed by the operator.", "warning"); $("#notebook-form").prepend(message); $('#notebook-form button[type="submit"]').disabled = true; }
  refresh(); setInterval(updateTimestamp, 10000); setInterval(refresh, 30000);
})();
