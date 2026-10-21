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
