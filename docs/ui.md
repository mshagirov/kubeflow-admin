# Console user guide

This guide describes current source; check your deployed image against [release evidence](verification.md). Sign in with an authorized OIDC administrator account. Logout ends the local session, not the upstream identity-provider session.

## Navigate and inspect

The sidebar contains Overview, Profiles, Storage, Namespaces, Notebooks, Pods, Nodes and History. Resource names open details; Profile and namespace names open summaries with scoped links.

- **Overview:** Workspace Health summarizes observed Nodes, Profiles, Notebooks, Pods, PVCs, PVs and Events. Expand the detailed table when needed. Profile protection labels describe policy, not readiness.
- **Filters:** namespace selection scopes namespaced resources; Profiles, Namespaces and Nodes remain cluster-wide. Search covers all loaded rows before pagination (50 rows by default). Clear filters removes search, namespace and resource focus; Back/Forward restores URL scope.
- **Node drilldown:** a Node's Scoped view opens its observed Running Pods across namespaces, with visible node/phase filters.
- **Freshness:** check `ObservedAt`. Navigation reuses recent inventory; Refresh fetches again. Failed fetches retain old observations with a stale warning. Counts are non-atomic snapshots, not live CPU/memory metrics or proof of convergence.

## Make a change

1. Select **New**, **Edit**, **Manage Notebook** or a resource action. Disabled controls explain missing feature/controller gates; the API also enforces authorization and policy.
2. Select the exact target and enter the intended changes and reason. Destructive actions require the exact typed resource name and applicable acknowledgements.
3. Prepare the server-validated **before/after preview**. Review warnings and confirm the plan separately; opening a drawer or preparing a plan performs no mutation. Expired or stale plans need a new preview.
4. Submit once, then follow **History → Operations** to completion. Acceptance means queued, not completed. Refresh inventory and independently verify application/storage behavior as needed.

Close, Escape or the backdrop closes a drawer and restores focus. Switching targets clears edits belonging to the previous target.

## Workflows

| Page | Use |
| --- | --- |
| Profiles | **New** creates a Profile with an email-formatted owner verified externally against your IdP. Names must be lowercase DNS labels, at most 63 characters; existing namespaces cannot be adopted. Summary → **Edit** changes quotas only: owner is read-only, existing keys cannot be removed, unmanaged values are preserved. Separately gated **Delete** cascades through the owned namespace and may destroy storage data. |
| Storage | **New** offers dynamic PVC creation, approved static registration, PV reservation and template-bound PVC creation. Resource details provide restricted YAML **Edit**, unused-only **Delete** and eligible Released-PV **Reclaim PV**. StorageClasses are read-only. See [storage](storage.md) for inputs, PodDefaults, isolation and partial outcomes. |
| Notebooks | Manage existing Kubeflow Notebooks; create them through the normal Kubeflow workflow first. Choose namespace, Notebook and named container. Blank image/resource fields preserve current values; they are not a prefill of deployed configuration. Mount replacement is explicit, same-namespace only, and cannot remove protected workspace mounts. |
| Pods | Inspect configured resources and ownership. Separately gated **Restart** supports only eligible Notebook StatefulSet-managed Pods, using eviction. See [Pods](pods.md). |
| Nodes | **Cordon/Uncordon** changes scheduling eligibility, not running workloads. See [Nodes](nodes.md) for reason/Force requirements. |

Notebook changes require writes and a confirmed controller contract. Images/GPU presets are allowlisted. Editing stops the Notebook first; **Restart after editing** can restore a previously running Notebook. Stop/edit/restart can lose kernels, processes and unsaved files; persistent storage does not checkpoint memory. Profile quota/device enforcement details are in [compatibility](compatibility.md).

## Notifications and history

Expand the notification stack to see all notices. Errors lead; repeats are deduplicated. Info/success notices expire after ten seconds; warnings/errors remain until dismissed. Dismissal does not acknowledge plan warnings or resolve operations. Only eligible dismissal IDs are remembered for the session, not message bodies or inventory.

**History → Operations** shows durable state; **Audit trail** groups stage events within a bounded loaded window. Details separate planned changes from actual mutation receipts (resource identity and observed outcome). An absent receipt does not prove nothing changed: an API response or persistence may have been lost.

For `needs-review` or partial completion, preserve the operation ID and inspect the exact objects using [operations and recovery](operations.md). There is no generic retry, replay, rollback or unlock action. See [history retention](history.md) for evidence limits and [live acceptance](ui-workflow-acceptance.md) for site verification.
