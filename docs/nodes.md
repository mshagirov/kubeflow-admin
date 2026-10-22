# Nodes, namespaces and workspace health

## Nodes page

The Nodes table shows Name, Status, Roles, Age, Version (kubelet), Internal-IP,
External-IP and Cordon/Uncordon. OS image, kernel and container runtime remain
available by selecting a Node name to open Resource Details, not as table columns.
Above 1200px, the table fits its container with wrapping and tighter padding;
smaller views retain a bounded horizontal scroll region. Values come from the
existing cluster-wide inventory read, not a live health probe or capacity guarantee.

Resource Details includes **Scoped view** above **Observed details**. Its count
includes only observed Pods in phase `Running` assigned to that exact Node,
across all namespaces. Clicking it opens `/pods?node=ENCODED_NAME&phase=Running`
with visible filters, clearing unrelated namespace/search/resource-focus state.
Zero-count links still work; Back/Forward and Clear filters restore the scope.
The count is a snapshot, not a live or atomic proof of running workload health.

`SchedulingDisabled` is appended to the readiness state when
`spec.unschedulable` is true, for example `Ready,SchedulingDisabled`.

## Cordon and uncordon

The workflow is disabled by default. It requires:

```yaml
features:
  writesEnabled: true
  nodeCordon: true
```

and `deploy/overlays/node-cordon/` (or an equivalent reviewed grant). The
overlay grants **only** `patch` on cluster-scoped `nodes`; it does not include
the broader Profile/Notebook/PVC write role. The supplied grant covers all Nodes.
A site may restrict it to named Nodes with `resourceNames`, but Kubernetes RBAC
cannot restrict a Node patch to only `spec.unschedulable`; admission policy is
needed for field-level enforcement.

This is the same operation as `kubectl cordon` / `kubectl uncordon`: a JSON
patch sets `spec.unschedulable` to true or false. It does not drain, evict or
move existing Pods, and it does not guarantee workload availability.

- A reason is required.
- Cordon requires the **Force** option for control-plane and NotReady Nodes.
- Force also allows re-applying the requested state.
- The preview shows the exact before/after `spec.unschedulable` and status.
- Execution re-reads the live Node, requires its stable UID, and uses a
  `resourceVersion` precondition with bounded retries for kubelet heartbeat
  churn. A concurrent scheduling/role change rejects the plan.
- Explicit API rejection before mutation fails and releases the Node lock.
  Ambiguous post-patch failures remain `needs-review` and retain the lock.

Do not use cordon as a replacement for safe maintenance draining or node
shutdown procedures. Control-plane cordon can reduce control-plane availability.

## Namespaces page

The Namespaces page lists observed Namespace objects with status and age.
Selecting a name opens a right-side drawer with the resources observed inside
that namespace: Notebooks, Pods, PVCs, StatefulSets, quotas, limit ranges, role
bindings and Events. Groups with no observed resources are omitted. The drawer
is based on the current inventory snapshot; it does not query new resource kinds
or prove that a namespace is empty.

## Workspace Health

Overview contains only the Workspace Health panel, with no separate metrics row.
On wide screens, a larger Nodes card sits beside six cards in three columns and
two rows: Profiles, Notebooks, Pods / PVCs, PVs, Events. The layout reflows on
smaller screens. The namespace selector scopes the summaries; Nodes always remain
cluster-wide. The full detailed resource table is optional and collapsed by default.

| Resource | Summary categories |
| --- | --- |
| Nodes | Ready, Cordoned, NotReady |
| Profiles | Unprotected, Protected (namespace-policy classification, not readiness) |
| Notebooks | Running, Stopped, Transitioning, Errors |
| Pods | Running, Warning, Errors |
| PVCs | Bound, Pending, Errors |
| PVs | Bound, Available, Other / Error |
| Events | Normal, Warning |

State categories are presentation summaries, not a compliance or availability
verdict. For example, a Pod with restarts is counted as a warning even while it
is running. Missing metrics or pipeline integrations are not inferred as zero.

## Actor display

History and previews show only the readable **Actor** label. The raw
`issuer|subject` identifier stays in storage for authorization and audit
attribution but is not rendered. Historical records without a captured display
label show `Legacy administrator` rather than exposing the encoded identifier.
