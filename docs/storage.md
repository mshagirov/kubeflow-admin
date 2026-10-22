# Approved user-storage configuration

These snippets belong under `storage` in the site `config.yaml`. They are **disabled examples**, not existing resources to adopt. Keep `features.writesEnabled`, `features.staticStorage` and `features.podDefaults` false until paths, owners, access modes, admission policy, CRD compatibility and isolation have been reviewed. Add the static-storage and PodDefault RBAC separately as described below.

Replace every placeholder with values verified against your own cluster. No real server, host, path, class or volume handle from any specific deployment should be committed here.

In the UI, **Storage → New** opens the existing management drawer. The current
workflows remain available: **Create dynamic PVC**, **Create from approved NFS /
CSI template** for the legacy combined static registration, **Reserve existing
PV**, optional expansion, and separately gated Edit/Delete. The template-driven
workflows add a standalone **Create PV** action and a **Create PVC from existing
PV** action as described in [template-driven provisioning](#template-driven-provisioning-new-static-workflows).
They require writes, `features.staticStorage`, the reviewed static-storage RBAC
and an approved registration template. The app does not allocate a backend disk
or create an export directory. Existing PV/PVC names open Resource Details with
separately gated restricted Edit and unused-only Delete actions.

## Template-driven provisioning (new static workflows)

The registration-template list is **separate from the legacy `nfsTemplates` and
`csiTemplates` lists**. Legacy templates keep their existing PV+PVC behavior and
strict NFS descendant rule. Do not reinterpret a deployed CSI handle or silently
widen a legacy NFS base path by moving it into the new section.

Two typed actions use a registration template:

- **Create PV** (`pv.create.static`) creates one standalone, cluster-scoped PV.
  The PV has no `claimRef` and no PVC is created. Choose an approved
  StorageClass and a matching registration template; capacity, access modes,
  `Filesystem` mode, `Retain` reclaim policy, mount options and driver details
  are read-only template values. Enter a full canonical absolute NFS path, or a
  relative BeeGFS suffix that is joined to the template's fixed prefix. Refresh
  before selecting the new PV for binding. Success verifies the created
  identity/spec and observed availability; it does not prove backend readiness.
- **Create PVC from existing PV** (`pvc.create.bound`) creates one PVC in an
  eligible Profile namespace and binds it to an approved, `Available`,
  unclaimed PV with compatible class, access modes, volume mode and capacity.
  The PVC request and access modes come from the template; they are independent
  of the PV's declared capacity. The plan binds the selected PV's current UID
  and resourceVersion, so a replacement or concurrently changed PV is refused.
  Where the selected NFS template requires directory annotations, the
  administrator supplies nonzero directory UID/GID; they are validated against
  the template and never derived from a browser display value or left over from
  another target.

**Legacy contract exception.** Only `pv.create.static` and `pvc.create.bound`
treat capacity and access modes as read-only registration-template values. The
legacy combined `storage.create.static` and `pv.reserve-and-claim` workflows
intentionally keep their existing typed size/access-mode inputs, and
`pvc.create.dynamic` keeps its existing size/access-mode contract. Legacy NFS
templates also keep the strict-descendant rule. A named approved StorageClass
that matches a static template/PV is valid; an empty StorageClass is not
required.

Example capacities are intentionally independent: a **2400Gi** NFS PV with a
**10Gi** PVC request, or a **500Gi** BeeGFS PV with a **10Gi** request. Both
examples use `ReadWriteMany` and `Retain`. These are examples, not universal
site sizes or an enforced directory quota.

The registration-template schema is versioned and typed. The current shape is:

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

The authenticated inventory exposes these templates only as the safe
`storageTemplates` projection when writes and static storage are both enabled.
There is no separate template-administration route, and the projection never
contains an existing PV's handle, path, Secret reference or unmanaged driver
attribute.

For a BeeGFS template prefix such as
`beegfs://beegfs.example.invalid/home`, entering the relative suffix
`example.user/kf-shared-volume` produces
`beegfs://beegfs.example.invalid/home/example.user/kf-shared-volume`. The
joined final handle must be **at most 128 bytes** for CSI contract compatibility;
a longer prefix/suffix combination is refused rather than
truncated or normalized. The administrator cannot change the scheme, authority,
fixed `/home` portion, driver, class or template attributes through the suffix
field.

Choose registration-template classes from `allowedStorageClasses`. A named
Static StorageClass matches the template/PV; do not force a static class to be
empty.

For a new NFS registration template, the approved root itself **or** a
descendant is accepted. For legacy NFS templates, only a strict descendant
remains accepted; this is not a silent relaxation of an existing rule. NFS paths
must be canonical non-root absolute paths with no traversal, control characters
or ambiguous encodings; validation is not proof about server-side symlinks,
exports or ACLs.

NFS registration templates require directory UID/GID metadata
(`requireDirectoryUIDGID: true`). The administrator supplies nonzero canonical
decimal UID/GID values on the bound-PVC workflow. The app writes only these
approved annotation keys:

- `k8s-sigs.io/nfs-directory-uid`
- `k8s-sigs.io/nfs-directory-gid`

The annotations are **intent-only metadata**: they neither prove nor change
filesystem ownership or permissions. The app does not create the directory,
chown/chmod it or verify server-side ACLs; the actual directory/export, UID/GID
ownership and workload access must be prepared and verified externally by the
storage operator. The same external-preparation caution applies to legacy NFS
templates. Declared NFS capacity/request sizes are not an enforced
directory-capacity quota. A CSI registration template likewise registers
existing storage only; it does not allocate a disk or prepare a directory.

### Optional PodDefault during PVC creation

There is **no standalone PodDefault page, editor, create/delete button or public
mutation endpoint**. When `features.podDefaults` is enabled (and writes plus the
applicable PVC workflow are enabled), PVC creation may optionally include one
dedicated PodDefault. The switch is off by default. The suggested name and mount
path may be reviewed/edited, but the namespace and `claimName` are fixed to the
PVC being created. The final PodDefault name is capped at **52 bytes** because
the upstream admission webhook derives a `poddefault-<name>` annotation segment
that must fit Kubernetes' 63-character segment bound. The generated object has
one PVC volume and mount, one independent selector derived from its final name,
and provenance annotations; it injects no unrelated environment, Secret,
container, service or PVC.

For dynamic PVC creation, the optional PodDefault is attempted only **after the
PVC object has been created**. A `WaitForFirstConsumer` claim may still be
`Pending` or unbound at that point: the workflow does not require the claim to
be `Bound` before creating the PodDefault. Acceptance of the PodDefault is not
proof that the claim will bind, that storage will be provisioned, that the
volume will mount, or that admission will accept a future Pod. It also does not
make a Pending claim ready.

Creating a PodDefault does not select it on a Notebook. Users choose the
corresponding configuration/label through the normal supported Notebook
workflow, and it applies only to matching Pods admitted afterward. Existing
Pods/Notebooks are not relabelled, restarted or remounted.

If the final name already exists, the request is refused unless the
administrator explicitly acknowledges an overwrite. The plan binds the exact
non-terminating object's UID/resourceVersion, explains selector changes and
removal of other PVC references or injected settings, then dry-runs and
revalidates immediately before a conditional patch. Unrelated metadata,
ownerReferences and finalizers are preserved. An externally managed/Helm/GitOps
object is a warning, not an ownership-based ban: another manager may reconcile
it afterward. Patching through the console does not prove exclusive ownership or
make it safe to delete without the later cleanup preview.

The two console-owned provenance annotations are updated to the current workflow
and exact newly created PVC UID on overwrite; unrelated annotations remain intact.

If PVC creation succeeds but the optional PodDefault step fails, the operation
reports the actual PVC/PV identity and the unfinished PodDefault step. It does
**not** delete the PVC as rollback, and it never recreates or overwrites a
PodDefault automatically. Ambiguous or unresolved partial outcomes become
`needs-review`; startup never replays them.

Operation details distinguish planned changes from append-only **mutation
receipts**. A receipt records the actual resource kind, namespace/name, UID,
resourceVersion and observed outcome (for example created, bound or deleted).
Receipts are persisted before subsequent writes and survive preview-plan expiry;
the exact reviewed cleanup identity set also remains in immutable acceptance
evidence. An absent receipt is not proof of no mutation: a lost API response or
failed persistence can leave the current checkpoint ambiguous. Creation receipts
alone do not establish mount/admission readiness. No backend path, full manifest
or injected setting belongs in a receipt.

Complete a partial PodDefault step only through explicit operator review:

1. Record the operation ID and the exact reviewed PVC, selected PV and
   PodDefault names/UIDs from History/Operations. Do not retry the operation.
2. With the operator's own authorized `kubectl`, inspect the exact reviewed
   objects by name and UID. For example, using placeholders:
   ```sh
   kubectl -n PVC_NAMESPACE get pvc PVC_NAME -o yaml
   kubectl -n PVC_NAMESPACE get poddefault PODDEFAULT_NAME -o yaml
   kubectl get pv PV_NAME -o yaml
   ```
   Verify the claim phase/`volumeName`, the PV binding/claimRef, and the
   PodDefault UID, selector, single volume/mount, claimName and provenance
   annotations. Do not copy Secret-bearing or unrelated settings into the
   review record.
3. If the PodDefault is missing or incomplete, complete or patch it manually
   only under the site's reviewed storage procedure, or leave it for explicit
   reconciliation. The PVC remains the source of truth for the claim.

There is **no standalone app repair, retry, replay or unlock endpoint**.
Acknowledging an incident does not release retained locks. Never delete the PVC
as rollback, never delete a replacement object, and never automatically
recreate a PodDefault.

### PVC deletion and related PodDefault cleanup

PVC Delete uses the existing unused checks first: any observed Pod or Notebook
template reference, including a stopped Notebook, blocks deletion **before**
cleanup. The server then lists all PodDefaults in the PVC namespace and inspects
their PVC volume references. Incomplete lists, malformed relevant structures,
denied reads, failed discovery or an incompatible API fail closed; errors are
never treated as "no references." Genuine absence of the PodDefault CRD is
reported as absence, but any unavailable or denied discovery path is a refusal.

The preview lists every related PodDefault selected for removal and the target
PVC, with names/UIDs and the target PVC reclaim/data-loss warning. Shared or
externally managed PodDefaults produce a prominent warning because deleting one
also removes its other injected mounts/settings from future matching Pods. Name
other affected PVC references, but never delete those PVCs and never recreate
their PodDefaults automatically. Shared/external management does not block an
explicitly reviewed deletion, but it requires explicit acknowledgement.

The reviewed cleanup set is bounded at **32 related PodDefaults**. A larger set
is refused explicitly rather than silently truncated. The administrator must
provide the PVC's exact typed name/reason confirmation, acknowledge the
PodDefault deletions and any shared/external warnings, and complete the existing
separate plan confirmation. A new or unreviewed PodDefault reference after
preview requires a fresh plan; a changed/replaced reviewed PodDefault is refused
before the replacement is touched.

The 52-byte creation/overwrite-name policy does not exclude existing related
PodDefaults from cleanup: existing valid Kubernetes object names up to 253
characters remain reviewable and deletable with the same identity checks.

All deletions are dry-run before any real write. Durable locks/checkpoints
precede the UID/resourceVersion-preconditioned PodDefault deletions; their
disappearance and Pod/Notebook/PodDefault dependencies are rechecked before the
conditional PVC delete. Removing PodDefaults does not remove existing Pod mounts
or Notebook template references. Separate checks/writes are not a Kubernetes
multi-object transaction and external writers/admission remain outside the app's
locks. If PodDefaults are already gone when PVC deletion fails, or only some
steps complete, the operation reports each completed/uncompleted step and keeps
unresolved evidence/locks; there is no automatic PVC rollback, PodDefault
recreation, force cleanup or finalizer removal.

PodDefault cleanup uses normal **Orphan** propagation: unreviewed dependents are
not cascade-deleted. Normal controllers may orphan their owner references; the
application never forces deletion or removes finalizers.

### PodDefault RBAC and fail-closed read dependency

The base read role always grants `get`/`list` on the discovered
`kubeflow.org/poddefaults` resource so PVC deletion can detect references even
when optional creation is disabled. This is a read dependency, not permission to
mutate. A site overlay that pinned an older copy of the read role must add this
read dependency; otherwise reference detection is denied and the PVC deletion
fails closed. If references exist but cleanup is disabled or unauthorized, PVC
deletion is refused with an explanation.

The separate `deploy/overlays/poddefaults/rbac.yaml` role grants only
`get/list/create/patch/delete` on `kubeflow.org/poddefaults`. It adds no wildcard
resource or verb, Secret read, exec/attach, impersonation, Namespace delete or
unrelated resource. Install it only after reviewing the installed PodDefault
CRD/schema/webhook and the site's exact namespaces; for a disposable pilot,
prefer a namespace-scoped Role. The overlay does not enable
`features.podDefaults`, `features.writesEnabled` or any other workflow, and it
is not part of a broad all-features bundle. Do not install the CRD/controller or
enable the option merely because the RBAC file exists.

The PVC-creation and cleanup action kinds are documented in the
[implemented OpenAPI contract](../api/openapi.yaml). There is no route that
creates, patches or deletes a PodDefault directly; PodDefault changes exist only
as substeps of a reviewed PVC plan and its durable operation/audit record.

## Configuration rollout and rollback

Adding `storage.registrationTemplates` or `features.podDefaults` is a
configuration rollout, not a live reload. Config is read at startup and unknown
keys are rejected. An older binary uses strict `KnownFields(true)` decoding and
will reject the new keys instead of ignoring them.

- Back up the complete stopped database/journal set **and the previous
  configuration** before changing storage or enabling the new feature. Record
  the release/config version and keep the backup off-node and off Git.
- This change assumes the existing database schema **version 3** and adds no new
  table or column migration. Do not use that assumption to skip a backup or to
  roll back a database copied from a newer schema.
- Do not roll back the image alone. An old binary cannot parse the new
  configuration and cannot interpret new action kinds or their durable
  operations. Active operations that use the new actions cannot be replayed by
  the old application.
- A reviewed rollback requires a matching restore of the database snapshot and
  the old configuration, or explicit reconciliation of active/`needs-review`
  operations. There is no generic app rollback, replay or unlock endpoint.
  Turning a feature flag off does not undo completed writes or release retained
  locks; revoking RBAC is also not a data rollback.
- Rehearse the config/DB restore on a disposable copy and retain operation IDs,
  exact object UIDs and audit evidence for any new-action operations that were
  accepted before the rollback.

## Existing NFS backend (legacy templates)

Configure a **new approved prepared user's root**, not a shared root containing other users' data. Preserve whatever mount options your NFS server requires (the example uses NFSv3, which is not a universal default):

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

The form's relative subdirectory selects a **strict descendant** of this root, for example `kf-workspace`. The storage operator must first prepare that exact backend directory and permissions. The app registers a Retain PV and reserves/binds its PVC; it does not make directories or set server ACLs. Do not point two profiles at the same backend data unless that sharing is separately approved. Existing overlapping exports are rejected rather than silently reused.

## Existing CSI volume import

Each CSI template identifies **one fixed approved existing volume**; the browser cannot supply arbitrary handles or driver attributes. Replace the driver, handle shape and attributes with the exact values required by your CSI driver, and prepare a distinct pilot directory with the storage operator first:

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

Never insert an already Bound PV's handle to assign it to another user. A CSI import only registers existing storage; it does not allocate a disk, prepare backend directories or enforce a directory capacity quota. The same is true of declared NFS capacity. Profile identity is not filesystem UID/GID; test the notebook image's actual permissions.

## Static backend uniqueness cannot be guaranteed by this app alone

Before reserving or creating a static PV the application lists PVs and rejects a backend that an existing PV already represents (same NFS server identity plus overlapping path; generic CSI uses exact `driver`+`volumeHandle`, while BeeGFS checks the canonical fixed endpoint and equal/overlapping backing paths). Ambiguous or malformed existing NFS/BeeGFS identities require operator review instead of being silently skipped. Incomplete safety-critical lists fail closed. That check is **not atomic**: the list and the subsequent create/patch are separate API calls, so two independent application processes sharing one cluster (for example two database instances, or an administrator using `kubectl`) can both observe "absent" and then both register the same backend. The application's single-writer lock only covers **its own** database, not the cluster.

Consequently:

- Run exactly one application writer against the cluster (one replica/`Recreate`, one database). This is already required for the database, and it is also required here.
- Treat static backend registration as a site-owned resource: enforce cluster-wide uniqueness with admission policy and a registration/coordination process covering **every** privileged writer (other tooling, operators, GitOps and direct `kubectl`), not only this app.
- Do not rely on a `ValidatingAdmissionPolicy` CEL expression to make this atomic. CEL in an admission rule cannot list and check the other PVs in the cluster; such a policy cannot close the race and would give false assurance. A real guard needs a component that can observe cluster state (for example a controller/reconciler, or an external registration system) and serialize the writers.
- Document the intended backend (`server`/path or driver/handle), owner and approval, and reconcile manually if two PVs are ever found pointing at the same backend; the app cannot repair that.

## Dynamic PVCs and expansion

For a new dynamically provisioned workspace, choose an approved class explicitly; do not create a PV yourself. Review Delete versus Retain behavior before choosing a class. Direct PV/PVC deletion is separately gated and unused-only, as described below. Profile deletion can cascade through its owned namespace and delete every PVC inside it: a Delete reclaim policy can irreversibly delete backing volumes, while Retain-policy PVs and external storage data may remain. Never use Profile deletion as storage cleanup; see [workflow acceptance](ui-workflow-acceptance.md).

With `volumeBindingMode: WaitForFirstConsumer`, the operation reports creation success while the claim is still `Pending`: the PVC cannot bind until a consuming Pod is scheduled, and the app never creates that Pod. This is creation success only, not provisioning verification — no volume exists yet, and real binding, capacity and backend behavior remain unproven until the operator schedules a workload and observes the bound claim.

Expansion is upward-only and feature/RBAC gated. A class advertising `allowVolumeExpansion` does not prove its backend procedure works. The initial adapter requires a bound CSI volume whose driver matches the class; plain NFS PV expansion is unsupported. A successful spec patch is not proof that filesystem resize completed. Do not use expansion to change a class, access mode or volume mode. Dynamic PVC creation may also include an optional dedicated PodDefault when `features.podDefaults` and the reviewed RBAC are enabled; see [Optional PodDefault during PVC creation](#optional-poddefault-during-pvc-creation).

## Restricted YAML editing and unused-only deletion

Storage resource names open **Resource Details**; the table has no separate
Details button. PV/PVC details offer **Edit** and **Delete**, subject to the
server capabilities and fresh target validation. StorageClasses are read-only.

These are separate opt-ins, not implied by `writesEnabled` or `staticStorage`:

```yaml
features:
  writesEnabled: true
  storageEditing: true
  storageDeletion: true
  # Needed additionally if increasing a PVC's storage request:
  pvcExpansion: true
```

Install only the permissions reviewed for the site:
`deploy/overlays/storage-editing/rbac.yaml` and/or
`deploy/overlays/storage-deletion/rbac.yaml`. These grant patch and delete
respectively on PVs/PVCs, not `cluster-admin`. For a composed site overlay,
copy the RBAC manifests into that overlay and list them as resources; do not
stack overlays that each include the base. All shipped feature flags stay false.
RBAC cannot enforce field-level patch restrictions or unused-only deletion;
admission policy and coordination must cover other privileged writers too.

**Edit is a restricted YAML projection, not a full manifest editor.** It shows
the fresh object identity (`apiVersion`, kind, name, namespace, UID and resource
version), console-owned labels under `kubeflow-admin.io/`, and either the PV
reclaim policy or PVC storage request. Approved changes are restricted to those
labels, a PV reclaim policy of `Retain`/`Delete`, and upward-only PVC requests
subject to the existing expansion checks. PV management is limited to approved,
Available, unreserved PVs. The identity cannot be changed. Backend addresses,
NFS paths, volume handles, driver attributes, annotations, unrelated labels,
status, owner references and finalizers remain hidden and untouched. Unsupported
fields, YAML aliases, duplicate keys, multiple documents, shrinkage and no-op
edits are refused. Omit `metadata.labels` to preserve the current console-owned
labels; supply a map to replace that console-owned subset, including `{}` to
remove all of it. Unrelated labels are always preserved. A reclaim-policy change can affect later data reclamation;
it is not just a cosmetic edit.

**Delete requires a reason, exact typed target name and a reviewed plan.** It
refuses PVCs referenced by observed Pods or Notebook templates (including stopped
Notebooks), and PVs that are bound, reserved, referenced or not Available. Read
errors fail closed. Deleting an unused bound PVC may still cause its Delete-policy
backing volume and data to be destroyed. Deleting a Retain PV object does not
erase or prove reclamation of the backing data. No finalizer removal, force
deletion or implicit delete/recreate path exists. PVC deletion additionally
reviews related namespace-local PodDefaults as described in
[PVC deletion and related PodDefault cleanup](#pvc-deletion-and-related-poddefault-cleanup):
at most 32 are included, shared/external warnings require explicit
acknowledgement, other PVCs are untouched, and a partial failure never rolls
back or recreates a PVC/PodDefault.

Both workflows fetch fresh identity, dry-run and revalidate. Edit issues one
conditional patch after its durable checkpoint. PVC cleanup conditionally deletes
each reviewed PodDefault before conditionally deleting the PVC, with a checkpoint
before each write and partial-outcome retention. A stale UID/resourceVersion
requires a new review. Deletion succeeds only after disappearance is observed;
finalizers or an uncertain API result may require operator review, not a retry.
Incomplete operations are not automatically replayed and retain their locks.

PVC deletion requires an explicit, approved StorageClass even while Pending. It
also refuses controller-owned claims and mismatched bound-PV reservations; a
Bound unused claim needs its exact PV claim-reference identity. Label-only PVC
editing is namespace/Profile-scoped but does not require an expansion-capable
class. An unresolved PVC deletion holds an exclusive namespace fence against
other app mutations; this does not constrain external writers or controllers.

An unused-resource check is **not atomic** with workload creation, a binder or
external administrator. Run one app writer, coordinate a maintenance window,
and enforce cluster-side admission/protection where necessary. The UI cannot
guarantee that storage remains unused between separate Kubernetes API calls.

## App database is separate

SQLite WAL requires local/block storage and is the base default. The base PVC omits `storageClassName`, so it uses the cluster's **default** StorageClass with **WAL**; verified BeeGFS instead requires `journalMode: delete` plus `allowUnverifiedFilesystem: true`. See [database requirements](database.md).
