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

Never insert an already Bound PV's handle to assign it to another user. Existing PV inventory is checked for duplicates. A CSI import only registers existing storage; it does not allocate a disk, prepare backend directories or enforce a directory capacity quota. The same is true of declared NFS capacity. Profile identity is not filesystem UID/GID; test the notebook image's actual permissions.

## Dynamic PVCs and expansion

For a new dynamically provisioned workspace, choose an approved class explicitly; do not create a PV yourself. Review Delete versus Retain behavior before choosing a class. No app workflow deletes a PVC/PV.

Expansion is upward-only and feature/RBAC gated. A class advertising `allowVolumeExpansion` does not prove its backend procedure works. The initial adapter requires a bound CSI volume whose driver matches the class; plain NFS PV expansion is unsupported. A successful spec patch is not proof that filesystem resize completed. Do not use expansion to change a class, access mode or volume mode.

## App database is separate

SQLite WAL requires local/block storage. The base uses DELETE journaling and supports configured BeeGFS on the default class; see [database requirements](database.md).
