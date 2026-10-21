# Approved user-storage configuration

These snippets belong under `storage` in the site `config.yaml`. They are **disabled examples**, not existing resources to adopt. Keep `features.staticStorage: false` until paths, owners, access modes, admission policy and isolation have been reviewed. Add the static-storage RBAC separately as described in [deployment](deployment.md).

Replace every placeholder with values verified against your own cluster. No real server, host, path, class or volume handle from any specific deployment should be committed here.

## Existing NFS backend

Configure a **new approved prepared user's root**, not a shared root containing other users' data. Preserve whatever mount options your NFS server requires (the example uses NFSv3, which is not a universal default):

```yaml
storage:
  allowedStorageClasses: [REPLACE_WITH_APPROVED_CLASS]
  nfsTemplates:
    - name: example-nfs
      server: nfs.example.org
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

Before reserving or creating a static PV the application lists PVs and rejects a backend that an existing PV already represents (same NFS `server`+overlapping path, or same CSI `driver`+`volumeHandle`). That check is **not atomic**: the list and the subsequent create/patch are separate API calls, so two independent application processes sharing one cluster (for example two database instances, or an administrator using `kubectl`) can both observe "absent" and then both register the same backend. The application's single-writer lock only covers **its own** database, not the cluster.

Consequently:

- Run exactly one application writer against the cluster (one replica/`Recreate`, one database). This is already required for the database, and it is also required here.
- Treat static backend registration as a site-owned resource: enforce cluster-wide uniqueness with admission policy and a registration/coordination process covering **every** privileged writer (other tooling, operators, GitOps and direct `kubectl`), not only this app.
- Do not rely on a `ValidatingAdmissionPolicy` CEL expression to make this atomic. CEL in an admission rule cannot list and check the other PVs in the cluster; such a policy cannot close the race and would give false assurance. A real guard needs a component that can observe cluster state (for example a controller/reconciler, or an external registration system) and serialize the writers.
- Document the intended backend (`server`/path or driver/handle), owner and approval, and reconcile manually if two PVs are ever found pointing at the same backend; the app cannot repair that.

## Dynamic PVCs and expansion

For a new dynamically provisioned workspace, choose an approved class explicitly; do not create a PV yourself. Review Delete versus Retain behavior before choosing a class. The app never directly deletes a PVC/PV. The separately enabled Profile deletion workflow can cascade through its owned namespace and delete every PVC inside it: a Delete reclaim policy can irreversibly delete backing volumes, while Retain-policy PVs and external storage data may remain. Never use Profile deletion as storage cleanup; see [workflow acceptance](ui-workflow-acceptance.md).

With `volumeBindingMode: WaitForFirstConsumer`, the operation reports creation success while the claim is still `Pending`: the PVC cannot bind until a consuming Pod is scheduled, and the app never creates that Pod. This is creation success only, not provisioning verification — no volume exists yet, and real binding, capacity and backend behavior remain unproven until the operator schedules a workload and observes the bound claim.

Expansion is upward-only and feature/RBAC gated. A class advertising `allowVolumeExpansion` does not prove its backend procedure works. The initial adapter requires a bound CSI volume whose driver matches the class; plain NFS PV expansion is unsupported. A successful spec patch is not proof that filesystem resize completed. Do not use expansion to change a class, access mode or volume mode.

## App database is separate

SQLite WAL requires local/block storage and is the base default. The base PVC omits `storageClassName`, so it uses the cluster's **default** StorageClass with **WAL**; verified BeeGFS instead requires `journalMode: delete` plus `allowUnverifiedFilesystem: true`. See [database requirements](database.md).
