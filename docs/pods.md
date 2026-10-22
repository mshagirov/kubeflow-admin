# Pod inventory and controlled restart

The Pods table shows Pod, Ready, Restarts, State, Node, Owner and Restart.
CPU/memory requests and limits remain in Resource Details (select a Pod name),
not in the table. Those values are configuration, not measured CPU/memory usage;
live usage is unavailable without a separate metrics integration.

Search, sorting and namespace filtering operate on the timestamped inventory.
URL-backed `node` and canonical `phase` filters additionally support Node scoped
navigation; `/pods?node=ENCODED_NAME&phase=Running` shows exact-node Running Pods.
Visible chips explain those filters. Clear filters removes node/phase as well as
search/namespace/resource focus; Back/Forward rehydrates the URL scope.

## Supported restart boundary

The first restart workflow supports only running Pods controlled by an
`apps/v1` StatefulSet that is itself controlled by a running `kubeflow.org/v1`
Notebook in an allowed Profile namespace. It refuses unmanaged/static/mirror
Pods, terminating targets, stopped/deleting parents and unsafe Node conditions
(not Ready, unreachable/not-ready taints, unschedulable or terminating).

It does not support arbitrary Deployment/Job/DaemonSet Pods, bulk restarts or
forced/zero-grace deletion. A restart interrupts kernels/processes and loses
writable container-layer and emptyDir state. Persistent volumes are not deleted,
but that does not guarantee application-level data consistency.

The workflow is **preview → explicit confirmation → durable operation**. It
revalidates the actual Pod UID, owner chain, parent/policy and Node state, writes
a durable checkpoint, and submits one normal `policy/v1` eviction with current
UID/resourceVersion preconditions. It does not evict during preview. The API
server enforces PodDisruptionBudgets; blocked eviction never falls back to delete.

Success requires the old UID to disappear and a distinct, correctly owned
replacement to be Ready at the StatefulSet's desired revision. Explicit rejection
before mutation is failed and releases locks. An ambiguous transport failure or
post-eviction timeout requires review and retains the Pod and parent Notebook
locks; it never blindly replays an eviction after restart.

Prepared evidence contains non-secret metadata and unkeyed fingerprints, not
full Pod manifests. Fingerprints are consistency checks, not encryption: anyone
with both a known candidate spec and a leaked digest could test candidates
offline. Environment values and selected secret-bearing fields are excluded
from the restart Pod projection, but command/probe arguments and the existing
Notebook fingerprint can still contribute to a digest. Do not embed credentials
in workload arguments or share plan/database payloads as anonymized data.

## Enable only after operator review

Both flags are required, along with a confirmed Notebook controller contract:

```yaml
features:
  writesEnabled: true
  podRestarts: true
notebooks:
  controllerContractConfirmed: true
policy:
  allowedNamespaces: [REPLACE_WITH_PILOT_NAMESPACE]
```

Keep other existing settings and restrictions; this is an excerpt, not a full
configuration. The normal base, and even the general all-features RBAC overlay,
do not silently grant eviction. The separate `pod-restarts` overlay demonstrates
the optional grant. Its ClusterRoleBinding is cluster-wide: for a pilot, prefer
the following **namespace-scoped** Role/RoleBinding in an ignored site file:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubeflow-admin-pod-restarts
  namespace: REPLACE_WITH_PILOT_NAMESPACE
rules:
  - apiGroups: ['']
    resources: [pods/eviction]
    verbs: [create]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kubeflow-admin-pod-restarts
  namespace: REPLACE_WITH_PILOT_NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeflow-admin-pod-restarts
subjects:
  - kind: ServiceAccount
    name: kubeflow-admin
    namespace: kubeflow-admin
```

This underlying permission can evict Pods throughout that namespace; backend
validation narrows actions but does not make RBAC field-limited. Keep admission
and network/authentication boundaries. Do not grant `delete pods`, wildcard
resources or `cluster-admin` to bypass a rejected restart.

## Disposable acceptance test

1. Take a consistent backup and choose an explicitly disposable running Notebook.
   Obtain the owner's permission to interrupt it; ensure valuable data is backed up.
2. Enable the feature and namespace-scoped permission only for the pilot; verify
   other namespaces, unsupported owners and feature-disabled requests are refused.
3. Prepare a restart with an actual reason. Review exact Pod UID, parent,
   interruption warnings and the fact that preview cannot guarantee PDB admission.
4. Confirm once. Observe operation status, old/new UIDs, replacement readiness
   and audit attribution. An accepted request is not proof of a ready replacement.
5. Test a PDB-blocked eviction on disposable resources: it must fail without
   deletion, force fallback or retained locks that prevent a fresh corrected plan.
6. Review timeout/cancellation/recovery cases using isolated fixtures, not by
   partitioning or force-deleting a production data writer.

Turning the feature off blocks new application actions but does not remove RBAC
or undo completed evictions. Revoke the exact optional RoleBinding through the
operator's reviewed procedure when ending the pilot; applying an older
Kustomization does not remove existing grants.
