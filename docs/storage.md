# User storage and PodDefaults

Configure approved classes/templates in the site's `config.yaml`; replace every placeholder with verified site values. Prepare backend storage and permissions externally. The app registers existing static storage; it does not allocate disks, create directories, chown/chmod them or enforce directory quotas. Profile identity is not filesystem UID/GID.

**Storage → New** opens creation/reservation workflows. Resource names open details with separately gated Edit/Delete/Reclaim actions. Every mutation requires writes, applicable feature flags, RBAC and a reviewed plan; see [deployment's capability matrix](deployment.md#6-apply-read-only-then-a-disposable-write-pilot). StorageClasses remain read-only.

## Template-driven provisioning (new static workflows)

| Action | Inputs and result |
| --- | --- |
| **Create PV** (`pv.create.static`) | Choose class/template, new PV name and full NFS path or relative BeeGFS suffix. Creates one unclaimed PV, without a PVC or claimRef. Refresh before binding it. |
| **Create PVC from existing PV** (`pvc.create.bound`) | Choose namespace, new claim name, class/template and compatible Available, unclaimed PV. The plan binds its current UID/resourceVersion; changes/replacements refuse execution. NFS requires directory UID/GID. |

PV capacity, access modes, Filesystem mode, Retain policy, mount options and driver settings are template-owned. PVC request/access modes are independently template-owned: a 2400Gi PV can serve a 10Gi request without enforcing a directory quota. Success verifies identity/spec and availability/binding, not backend mount readiness.

### Tenant isolation

Standalone unreserved PV creation requires the class in `storage.isolatedStorageClasses` (empty by default). This is an operator attestation, not enforcement:

1. Use dedicated registration-only classes; never make one the cluster default.
2. Install admission denying tenant PVCs for those classes; adapt [the isolation example](../deploy/examples/isolated-storage-policy.yaml).
3. Test explicit/defaulted class requests and the deprecated `volume.beta.kubernetes.io/storage-class` annotation with missing, empty and conflicting spec class. The example forbids that annotation because the binder gives it precedence.
4. Remove/migrate pre-existing unbound claims and verify console exact-PV binding still works.

The app refuses pre-existing unbound claims and dynamic creation into isolated classes. Its list check cannot fence future requests: an ordinary Pending claim in any namespace could otherwise bind an Available PV and expose retained data. Do not enable unreserved creation or reclaim without enforceable isolation.

### Registration configuration

```yaml
storage:
  allowedStorageClasses: [REPLACE_WITH_NFS_CLASS, REPLACE_WITH_BEEGFS_CLASS]
  registrationTemplates:
    - name: example-nfs
      backend: nfs
      storageClass: REPLACE_WITH_NFS_CLASS
      pvCapacity: 2400Gi
      pvAccessModes: [ReadWriteMany]
      reclaimPolicy: Retain
      volumeMode: Filesystem
      pvcRequest: 10Gi
      pvcAccessModes: [ReadWriteMany]
      nfsServer: nfs.example.invalid
      nfsRoots: [/exports/kubeflow/REPLACE_WITH_APPROVED_USER_ROOT]
      nfsMountOptions: [nfsvers=3]
      requireDirectoryUIDGID: true
      podDefaultName: mount-toast
      podDefaultMountPathPrefix: /home/jovyan
    - name: example-beegfs
      backend: beegfs
      storageClass: REPLACE_WITH_BEEGFS_CLASS
      pvCapacity: 500Gi
      pvAccessModes: [ReadWriteMany]
      reclaimPolicy: Retain
      volumeMode: Filesystem
      pvcRequest: 10Gi
      pvcAccessModes: [ReadWriteMany]
      beegfsDriver: beegfs.csi.netapp.com
      beegfsPrefix: beegfs://beegfs.example.invalid/home
      podDefaultName: mount-hpc-home
      podDefaultMountPathPrefix: /home/jovyan
features:
  writesEnabled: false
  staticStorage: false
  podDefaults: false
```

Add `storage.isolatedStorageClasses` only after the isolation checks above. Classes must also be allowed and match the selected template/PV; static storage does not require an empty class name. Authenticated inventory exposes approved non-secret template settings only when writes and static storage are enabled; it never exposes arbitrary existing PV sources or Secret references.

- **NFS:** the approved root itself or a descendant is accepted. Paths must be canonical, non-root absolute paths without traversal, control characters or ambiguous encodings. Validation cannot establish server-side symlink/export/ACL safety. Bound-PVC UID/GID must be nonzero canonical decimals; the only directory annotations are `k8s-sigs.io/nfs-directory-uid` and `k8s-sigs.io/nfs-directory-gid`. They express intent, not actual ownership changes.
- **BeeGFS:** prefix `beegfs://beegfs.example.invalid/home` plus suffix `example.user/kf-shared-volume` yields the corresponding joined handle. The final handle must fit **128 bytes**; it is not truncated. The suffix cannot replace the scheme, authority, fixed path or driver attributes.
- **Legacy distinction:** `nfsTemplates`/`csiTemplates` are separate contracts. Combined static creation, reservation and dynamic creation retain typed size/access-mode inputs. Legacy NFS remains strict-descendant-only; do not silently widen its path or reinterpret a deployed CSI handle when migrating configuration.

### Optional PodDefault during PVC creation

With `features.podDefaults` and its separate grant, PVC creation can include one dedicated PodDefault; the switch defaults off. Review its editable name/mount path. Namespace and claimName are fixed to the new PVC; the generated spec contains one PVC volume/mount, a selector derived from its final name and provenance annotations. New/overwrite names are capped at **52 bytes** for the upstream webhook's annotation-name limit.

For dynamic storage, the PodDefault is attempted after the PVC exists, even if a WaitForFirstConsumer claim remains Pending. Neither creation proves future binding, mount access or admission. Users must select the configuration/label through their normal Notebook workflow; it affects newly admitted matching Pods only, without relabeling/restarting existing workloads.

An existing name requires explicit overwrite acknowledgement. The preview describes selector changes and removal of other injected settings/PVC references; the conditional patch binds the existing nonterminating UID/resourceVersion. Unrelated metadata, ownerReferences and finalizers remain. Console provenance is updated to the new PVC UID. External/Helm/GitOps management warns rather than prohibits overwrite; another manager may reconcile it afterward.

If PVC creation succeeds but the PodDefault step fails, keep the PVC and inspect the partial result. No automatic overwrite, recreation or rollback occurs. There is no standalone PodDefault management or repair endpoint.

### PVC deletion and related PodDefault cleanup

Any Pod or Notebook template reference, including a stopped Notebook, blocks deletion **before** cleanup. The app then discovers/lists namespace-local PodDefaults and checks their PVC references. Denied/incomplete reads, incompatible APIs and malformed relevant structures fail closed; genuine CRD absence is distinguished from failed discovery.

The preview includes every related PodDefault (maximum **32**) and the PVC, with exact identities and reclaim/data-loss warnings. Shared or externally managed PodDefaults require acknowledgement: their other injected settings disappear from future matching Pods, but other PVCs are untouched. Existing valid names up to 253 characters remain eligible for cleanup regardless of the 52-byte creation limit.

Provide the PVC's exact name, reason, cleanup/shared-resource acknowledgements and separate plan confirmation. New references require a new plan; changed/replaced objects are refused. All deletes dry-run before any real write. Each has a durable checkpoint and UID/resourceVersion preconditions. PodDefaults use **Orphan** propagation; their disappearance and dependencies are rechecked before PVC deletion. No finalizers are removed.

If cleanup partly succeeds, completed deletions stay completed. The operation reports unfinished steps and retains unresolved evidence/locks; it never recreates PodDefaults or rolls back the PVC. Removing a PodDefault cannot remove an existing workload's mount or template reference.

### PodDefault RBAC and fail-closed read dependency

The base grants `get/list` on PodDefaults for dependency checks even with optional mutation disabled. Update sites using an older read-role copy. References block PVC deletion when cleanup is disabled or unauthorized.

The separate [PodDefault role](../deploy/overlays/poddefaults/rbac.yaml) grants `get/list/create/patch/delete` only on that resource. Prefer namespace-scoped pilot permissions and verify the installed CRD/webhook before enabling the flag. Installing RBAC does not enable a workflow or install its controller.

### Partial-outcome review

Mutation receipts record actual kind, namespace/name, UID/resourceVersion and outcome, separately from planned changes. They persist before later writes and survive preview expiry. Missing receipts do not prove no write occurred; a response or persistence may be lost.

1. Record the operation ID and reviewed PVC/PV/PodDefault identities from History. Do not retry the operation.
2. Inspect those exact objects using authorized operator tooling:

   ```sh
   kubectl config get-contexts
   CONTEXT=default # Example: select the intended context.
   kubectl --context "$CONTEXT" -n PVC_NAMESPACE get pvc PVC_NAME -o yaml
   kubectl --context "$CONTEXT" -n PVC_NAMESPACE get poddefault PODDEFAULT_NAME -o yaml
   kubectl --context "$CONTEXT" get pv PV_NAME -o yaml
   ```

3. Compare UIDs, claim phase/volumeName, PV claimRef, PodDefault selector, volume/mount and provenance. Keep sensitive settings out of the incident record.
4. Complete a missing PodDefault step only under the site's explicit operator procedure. Never delete the PVC as rollback or touch a replacement object. Manual repair does not release retained app locks; follow [operations](operations.md).

## Configuration rollout and rollback

Configuration loads at startup; unknown fields are rejected. Older binaries may reject new keys or misunderstand accepted action/evidence contracts even without a schema migration. Back up the complete stopped database/journals and previous configuration before changes. Rehearse a compatible restore; image-only rollback and flag/RBAC removal do not undo writes or resolve locks. See [upgrade and recovery](operations.md#recovery-and-upgrades).

## Existing NFS backend (legacy templates)

Use a prepared user's root, not a shared root containing other users' data. Preserve site-required mount options; NFSv3 is only an example:

```yaml
storage:
  allowedStorageClasses: [REPLACE_WITH_APPROVED_CLASS]
  nfsTemplates:
    - name: example-nfs
      server: nfs.example.invalid
      basePath: /exports/kubeflow/REPLACE_WITH_APPROVED_USER_ROOT
      storageClass: REPLACE_WITH_APPROVED_CLASS
      accessModes: [ReadWriteMany]
      mountOptions: [nfsvers=3]
  csiTemplates: []
```

The form takes a relative strict descendant, such as `kf-workspace`, prepared externally. Combined `storage.create.static` registers a Retain PV reserved for the new PVC and binds the pair. Overlapping registrations are refused; sharing backend data between Profiles requires separate site approval.

## Existing CSI volume import

Each legacy CSI template identifies one fixed existing volume; browser input cannot change its handle/attributes. Prepare distinct pilot storage and supply driver-specific values:

```yaml
storage:
  allowedStorageClasses: [REPLACE_WITH_APPROVED_CLASS]
  nfsTemplates: []
  csiTemplates:
    - name: example-csi
      driver: REPLACE_WITH_CSI_DRIVER
      volumeHandle: REPLACE_WITH_OPERATOR_APPROVED_VOLUME_HANDLE
      storageClass: REPLACE_WITH_APPROVED_CLASS
      accessModes: [ReadWriteMany]
      volumeAttributes: {}
```

Never reuse a Bound PV's handle for another user. Reservation (`pv.reserve-and-claim`) accepts only approved Available, unreserved Retain filesystem PVs without node-affinity/topology constraints, matching the claim's class, size and modes. It sets claimRef and creates the claim without changing the backend/spec.

## Static backend uniqueness cannot be guaranteed by this app alone

Creation/reservation checks existing PVs: overlapping NFS paths on the same server, exact generic CSI driver/handle, and equal/overlapping BeeGFS paths at the canonical endpoint are refused. Ambiguous/malformed identities and incomplete lists require review.

List and write are separate calls. Run one app writer against the cluster and coordinate **all** privileged registrars, including GitOps/operators/direct kubectl. Enforce cluster-wide uniqueness through a serialized registration process or state-aware component; CEL admission cannot list other PVs and close this race. Record approved backend ownership privately and investigate duplicate registrations manually.

## Dynamic PVCs and expansion

Dynamic creation requires an explicitly approved class and typed size/access mode; the external provisioner creates the PV. Review Delete/Retain policy. WaitForFirstConsumer may report successful **claim creation** while Pending; a consuming Pod must be scheduled externally to establish binding/provisioning. Pending does not prove a volume exists or is usable.

Expansion is upward-only, separately gated and limited to bound, nonterminating CSI claims whose driver/class and claimRef match. The class must advertise expansion; plain NFS is unsupported. Success requires reported capacity at least the requested size without active resize-pending conditions. Independently verify mounted filesystem capacity, backend support and quotas. Class/access mode/volume mode changes and shrinking are unsupported.

## Restricted YAML editing and unused-only deletion

```yaml
features:
  writesEnabled: true
  storageEditing: true
  storageDeletion: true
  # Needed additionally if increasing a PVC's storage request:
  pvcExpansion: true
```

Install separately reviewed [editing](../deploy/overlays/storage-editing/rbac.yaml) and/or [deletion](../deploy/overlays/storage-deletion/rbac.yaml) grants. Ordinary deletion needs no PV patch grant; reclaim adds [its own grant](../deploy/overlays/pv-reclaim/rbac.yaml). RBAC cannot enforce field restrictions or unused-only checks for other writers; use admission and coordination.

**Edit** exposes a fresh identity and restricted YAML: `kubeflow-admin.io/` labels, PV Retain/Delete policy or PVC storage request. PV edits require approved Available, unreserved targets; growth requires expansion checks. Label-only PVC edits remain Profile/namespace-scoped without requiring an expansion-capable class.

Identity, backend sources, annotations, unrelated labels, status, ownership and finalizers cannot be edited. Unsupported fields, aliases, duplicate keys, multiple documents, shrinkage and no-ops are refused. Omitted/null `metadata.labels` preserves console labels; a map replaces only that subset, and `{}` clears it. Other labels remain unchanged. A reclaim-policy edit changes potential future data destruction.

**Delete** requires a reason, exact typed name and separate plan confirmation. PVCs must be unused, not controller-owned and have an explicit approved class, even when Pending; bound claims require the exact PV claimRef. PVs must be Available, unreserved and unreferenced. Read failures refuse the action. Related PodDefaults follow the cleanup procedure above.

Deleting an unused bound PVC can destroy Delete-policy backend data. Deleting a Retain PV object does not wipe its data. Profile deletion separately cascades through all namespace PVCs and snapshots; do not use it as storage cleanup.

Fresh checks, dry runs, conditional writes and observed disappearance are required. Stale identity needs a new review. Uncertain responses/finalizers can leave `needs-review`, retaining locks; unresolved PVC deletion also fences the namespace against other app mutations. External workload creation and binding can still race these checks, so coordinate a maintenance window and cluster-side protections.

### Retain volume reclaim after PVC deletion

#### Reclaim an already-Released PV

Open a Released PV → **Reclaim PV** (`pv.reclaim`). Requires writes, storage deletion, `pvReclaim`, its patch grant and an approved isolated class. Provide reason, exact name and retained-data exposure acknowledgement.

The target must be nonterminating, Released/Retain, with a complete stale claimRef (namespace/name/UID) and absent old claim. Protected/out-of-scope old namespaces, replacements, referencing PVCs and unbound matching-class claims (including beta class aliases) block reclaim.

After checkpoint/revalidation, a conditional patch removes only the exact stale claimRef. Success observes the same PV UID Available without claimRef and records an `available` receipt. Kubernetes updates status. An ambiguous patch retains the PV lock as `needs-review`; nothing is replayed or rolled back. Refresh before reuse or a separate Delete plan. **Available exposes retained data; it is not a wipe.**

#### Optional reclaim during PVC deletion

Default Retain behavior leaves the PV Released with its claimRef. Optional reclaim adds the same feature/isolation/permission gates plus per-request `confirmPVReclaim`. The plan names the PV, warns about data exposure, locks it and dry-runs the patch before PVC deletion; missing patch permission refuses the plan early. Delete-policy volumes and claims without a PV are never reclaimed.

After recording PVC disappearance, the app awaits the exact PV becoming Released, still Retain with unchanged relevant spec and the deleted PVC's exact claimRef. It checkpoints and rechecks claims: a recreated old name, any reference to the PV, an unbound matching-class claim or incomplete list refuses reclaim. It conditionally removes claimRef only and observes Available.

If reclaim fails or cannot be verified, the PVC remains deleted and the operation becomes `needs-review` with receipts/locks preserved. Inspect the actual PV because a timed-out patch may have applied. No recreation, finalizer removal or automatic retry occurs. Isolation and external-writer coordination remain necessary between separate reads and writes.

## App database is separate

These templates configure user volumes, not SQLite storage. The base database PVC uses the default StorageClass and WAL; verified BeeGFS needs DELETE journaling and explicit opt-in. Follow [database requirements](database.md).
