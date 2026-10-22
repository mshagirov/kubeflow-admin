# Console UI — current source

This guide describes the implemented UI. See [verification](verification.md) for
the published image/digest and pending release status. A source change is not
automatically deployed. See [live acceptance](ui-workflow-acceptance.md) for
operator-run checks; synthetic browser tests do not prove cluster behavior.

## Navigation and observations

Sidebar order: Overview, Profiles, Storage, Namespaces, Notebooks, Pods, Nodes,
History. No WORKSPACE label is displayed above the navigation. Navigation reuses
the current in-memory inventory; Refresh and the freshness policy fetch new
observations. The toolbar says "Observed snapshot of the cluster state"; this is
not an atomic snapshot or proof of convergence. No live CPU/memory or pipeline
integration is inferred from Kubernetes object counts.

## Overview

The eyebrow reads **CLUSTER AT A GLANCE**. Overview has no subtitle paragraph or
separate metrics cards. Workspace Health is the only content panel, with the
caption "Inventory snapshots | Latest observed resource counts by status".

A larger Nodes card sits on the left of six cards in a three-column/two-row wide
layout: Profiles, Notebooks, Pods / PVCs, PVs, Events. Narrower views reflow. A
namespace selector scopes summaries; Nodes remain cluster-wide. Profiles are
classified by namespace policy as Unprotected/Protected, not by controller
readiness. Protected also includes names outside a configured allowlist.
The optional detailed resource table starts collapsed. See [Nodes](nodes.md) for
the exact summary categories.

## Profiles, Storage and Namespaces

- **Profiles:** the Workspace ownership information panel is above the full-width
  table. **New** opens the Profile management drawer; it does not submit a write.
  A Profile name opens its single Summary with metadata, declared quotas, scoped
  resource links and gated Edit/Delete actions. Editing preserves the owner;
  ownership transfer is unsupported. Unmanaged quota keys/types are preserved.
- **Storage:** **New** opens the existing storage management drawer. Dynamic PVC
  creation asks an approved StorageClass to provision storage; the app does not
  directly create a PV in that workflow. Static PV registration/reservation needs
  a separate opt-in, reviewed RBAC and approved prepared NFS/CSI templates.
  Resource names open details; PV/PVC details offer restricted Edit and unused-only
  Delete. StorageClasses remain read-only. See [storage](storage.md) for exact
  fields, gates, data-loss risks and backend limitations.
- **Namespaces:** names open observed-resource summaries. Empty groups are
  omitted, not interpreted as proof that the namespace is empty.

## Notebooks, Pods and Nodes

- **Notebooks:** only Kubeflow Notebook custom resources appear. The heading's
  **Manage notebook** button stays right-aligned and vertically centered;
  descriptions and missing-gate explanations wrap on the left. Both writes and
  `notebooks.controllerContractConfirmed` are required. A Notebook name opens
  details; Manage Notebook there binds the exact target. This app manages
  existing Notebooks, not their initial creation. Blank edit fields preserve
  current configuration; mount replacement is explicit, not an editable prefill.
- **Pods table:** Pod, Ready, Restarts, State, Node, Owner, Restart. CPU/memory
  requests/limits remain in Resource Details, not table columns or live metrics.
  Node/phase URL filters have visible chips and support Clear filters/Back/Forward.
  See [Pods](pods.md) for the separately gated restart boundary.
- **Nodes table:** Name, Status, Roles, Age, Version, Internal-IP, External-IP,
  Cordon/Uncordon. It fits its wide-screen container with wrapping; smaller views
  scroll. OS image, kernel and runtime remain in Resource Details. The Node's
  Scoped view links its exact-node Running-Pod count across namespaces to the
  filtered Pods page. See [Nodes](nodes.md) for scheduling-action safeguards.

## Notifications and History

One notification is visible by default; the count button expands/collapses the
stack. Errors always lead. Within error/non-error groups, newest arrival is
first; repeats update recency without duplicating cards. Info/success messages
expire after ten seconds, while warnings/errors wait for dismissal. Only eligible
inventory/policy dismissal identifiers are remembered for the current session.
Message bodies and inventory are not persisted in browser storage.

Notifications do not acknowledge mutation warnings or prove an accepted operation
completed. Follow durable state in **History → Operations**; **Audit trail** shows
bounded grouped events, not a complete archival export. See [history](history.md)
and [operations](operations.md) for retention and incident response.

## Writes and safety gates

New/Edit/Delete/Manage controls never bypass server authorization, feature gates,
fresh identity checks or the separate preview/confirmation step. Readiness probes
and observed rows do not certify RBAC, controller compatibility or data safety.
`policy.allowedNamespaces: []` removes the namespace allowlist, not built-in or
configured system protections. All-profile administration additionally requires
reviewed cluster-wide mutation RBAC. Destructive tests remain disposable-only.
