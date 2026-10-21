# Compatibility and discovery notes

This project targets a self-hosted RKE2/Kubernetes cluster running Kubeflow with LDAP-backed identity. It does **not** encode any specific cluster's servers, paths, classes or volume handles. Complete discovery against your own cluster and record it locally (see the optional read-only `scripts/discover.sh`); do not commit site-specific output into this repository.

## What to confirm before enabling writes

| Area | Confirm |
| --- | --- |
| APIs | `kubeflow.org/v1` `profiles` (cluster-scoped) and `notebooks` (namespaced) are served |
| Controllers | Installed Profile and Notebook controller image digests/tags (do not infer from the Kubeflow platform version) |
| Lifecycle | The installed Notebook controller honours `kubeflow-resource-stopped` (presence → 0 replicas, absence → 1) and its idle-culling settings |
| Identity | Public Dex/OIDC issuer, exact admin group claim (or issuer-bound subject), and the Kubeflow user identifier used for Profile owners |
| Storage | The StorageClasses actually in use, their reclaim policy, binding mode, access modes, expansion support, and the real backend (dynamic CSI, static NFS, etc.) |
| Scheduling | GPU resource keys the device plugin advertises, and the node selectors/taints/tolerations/RuntimeClasses a valid GPU or node preset needs |
| DB disk | Local/block storage (WAL), or verified BeeGFS with DELETE journaling and explicit opt-in; see [database requirements](database.md) |
| Routing | An HTTPS entry point for the dedicated admin host, with its authentication policy tested to avoid double-auth callback loops |
| Admission | Installed admission policy that constrains privileged Pod specs, disallowed ServiceAccounts/volumes and unapproved PV sources |

## Controller source verification

The public Kubeflow `v1.10.0` sources were read during implementation and confirm the Notebook lifecycle semantics:

- Presence of `kubeflow-resource-stopped` requests zero StatefulSet replicas; absence requests one.
- The culler removes `notebooks.kubeflow.org/last-activity` and `notebooks.kubeflow.org/last_activity_check_timestamp` while a notebook is stopped or missing its Pod, and re-initializes them on a later running reconciliation.
- The controller transforms the Notebook template (working-directory/port defaults, `NB_PREFIX`, optional default `fsGroup`), so whole-template equality is not a valid generic convergence check. The application compares only the edited fields.

This verifies published source semantics, not your deployed image digest, culling configuration, or a successful live start/stop test.

### Profile owner and deletion contract

The [v1.10.0 Profile controller source](https://github.com/kubeflow/kubeflow/blob/v1.10.0/components/profile-controller/controllers/profile_controller.go)
checks the existing namespace's `owner` annotation before reconciling access or
quotas. Changing `spec.owner.name` in place causes that check to fail. This
console therefore shows the current owner read-only and rejects owner transfers;
it never patches namespace ownership to work around the controller. Quota
editing uses the exact existing owner and a configuration-digest precondition.
The deprecated `profile.quota.change` alias now requires the same guard; old
unguarded requests are refused. Reload the browser after upgrading and update
any direct clients before attempting quota changes.
New Profile owners must be email addresses; syntax validation does not verify
registration with LDAP or the identity provider. Existing non-email owners may
remain unchanged when editing quotas.

The controller sets the Profile as the namespace's controlling owner. On normal
Profile deletion it revokes plugins and removes its own finalizer; Kubernetes
garbage collection and namespace cleanup remove the owned namespace and its
contents. The separately enabled console workflow deletes only the reviewed
Profile, waits for both objects to disappear, and never bypasses finalizers.
Profile deletion can delete PVC-backed data under a Delete reclaim policy;
retained PVs and external data may remain. Verify the installed controller,
storage behavior and disposable-pilot cleanup before enabling this capability.

### Profile quota key policy

An installed Profile may declare quota keys this console does not author. The
Kubernetes ResourceQuota accepts the `cpu` and `memory` aliases and charges
them against the same pod request as `requests.cpu` and `requests.memory`
(upstream `pkg/apis/core/helper/helpers.go` lists both as standard quota
resources). This console therefore edits an existing alias **under its own key
name**: the key is never renamed to its `requests.*` counterpart and no second,
redundant limit is added beside an observed one.

An extended device resource the Profile already declares (`nvidia.com/gpu`) is
edited under its own name as well, as a whole number of devices. A device
resource that is not declared yet is never introduced: a new Profile only ever
declares a device quota under the canonical `requests.<device>` key of an
approved `notebooks.gpuPresets` entry. An existing Profile cannot gain a new
raw device key; approved canonical keys remain authorable explicitly, never
inserted automatically while editing another value. A reserved cluster key such as
`foo.storageclass.storage.k8s.io/requests.storage` stays operator managed: it
is preserved verbatim and it may only be round-tripped with an identical value.
The `count/<resource>` namespace (for example `count/jobs.batch`) is an enforced
object-count quota, not a raw device key; it also remains operator managed.

Every other key is operator/controller managed. It is preserved verbatim, shown
read-only and may only be round-tripped unchanged. Removing a quota key is
unsupported and an update that would change nothing is refused. Preserved keys
are not revalidated against an incomplete local copy of Kubernetes' quota-key
list. A Profile dry-run does not validate the controller-generated ResourceQuota;
the operation still requires actual quota convergence before success.

A raw device key is recorded but not enforced. Kubernetes 1.32 accepts and lists
a bare extended-resource key such as `nvidia.com/gpu` in a ResourceQuota, but its
quota accounting neither charges nor evaluates that key as a device request
quota: device enforcement uses `requests.nvidia.com/gpu`. A `ResourceQuota`
`status.hard` map, including the resource quota the Profile controller derives
from `spec.resourceQuotaSpec`, only restates declared limits; it is not evidence
that a device limit is enforced. Enforcement is measured in `status.used` after
an admitted pod requests the canonical `requests.<device>` resource, or by a
disposable admission test that asks for more than the declared limit and must
be refused. Both measurements are operator-run only, in an approved window
against a disposable namespace; this repository never runs them against a
cluster.

Every quota-edit plan built from a live Profile that declares such a raw device
key carries a warning naming the key, even when only an unrelated CPU or memory
key changes. Editing retains the key name and changes only requested values; it does
not rename, migrate or activate the limit. Migrating the declaration to
`requests.<device>` changes what Kubernetes enforces, so it is a separate,
explicitly authorized operator migration, never an automatic side effect of a
console edit.

The installed Profile controller copies `spec.resourceQuotaSpec` verbatim into
the `kf-resource-quota` resource quota of the namespace
(`components/profile-controller/controllers/profile_controller.go`); it never
normalizes or renames a key. The console therefore keeps the declared key
names exactly as they are observed.

## Read-only discovery (run yourself)

Do not export Secrets or authentication configuration. A representative command set:

```bash
kubectl version -o yaml
kubectl api-resources --api-group=kubeflow.org
kubectl get crd profiles.kubeflow.org notebooks.kubeflow.org -o yaml
kubectl get storageclasses -o yaml
kubectl get csidrivers
kubectl get pv
kubectl get pvc -A
kubectl get resourcequotas,limitranges -A
kubectl get deployments -A \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGES:.spec.template.spec.containers[*].image'
```

`scripts/discover.sh` wraps a read-only subset of this. Review any output locally before sharing, and never commit it.
