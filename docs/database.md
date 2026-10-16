# Database storage

SQLite stores application state only (sessions, plans, operations, audit), not
cluster inventory. The base PVC omits `storageClassName`, so it uses the
cluster's **default StorageClass**; the base configuration keeps the **WAL**
default for local/block storage. WAL requires local storage and is rejected on
network filesystems. Native BeeGFS instead requires DELETE journaling plus an
explicit operator opt-in:

```yaml
database:
  path: /var/lib/kubeflow-admin/admin.db
  journalMode: delete                  # BeeGFS only
  allowUnverifiedFilesystem: true      # deliberate acknowledgement, see below
```

The application cannot prove that a network filesystem implements durable
locking and `fsync`. `allowUnverifiedFilesystem: true` is an acknowledgement
that an operator verified the storage; without it, BeeGFS and unrecognized
filesystems are rejected at startup. NFS, SMB/CIFS, FUSE, tmpfs, Lustre,
CephFS, GPFS and OCFS2 are always rejected. A warning is logged whenever the
database runs on unverified storage.

overlayfs (common for container image roots and CI/dev workspaces) is accepted
so those environments can run, but it is also reported **unverified** and
triggers the same startup warning: its durability and cross-node locking are not
proven. Treat it as a development/CI location only, never as a production
database backend.

## BeeGFS deployment

- Verify `tuneUseGlobalFileLocks = true` and `tuneRemoteFSync = true` in the
  **effective CSI mount's client configuration on every eligible node**, and
  confirm with your storage operator that global locking covers both POSIX
  advisory locks (`fcntl`, used by SQLite) and `flock` (used by the app), and
  that `tuneRemoteFSync` commits to server disks on `fsync`. StorageClass
  parameters alone do not prove these settings.
- The database directory must be writable by UID/GID 65532 and not accessible
  to other users. The app sets `umask 0077` and creates the directory 0700;
  database, journal, `-wal`/`-shm` and lock files are then private (0600).
  Keep it separate from notebook/user data. Pre-existing sidecars (`.lock`,
  `-wal`, `-shm`, journal) must be private regular files: a planted symlink or a
  non-private sidecar is rejected at open, and the app never follows a symlink
  for its own files. SQLite inherits the main file's 0600 mode for new sidecars;
  this is regression-tested under a permissive umask in an isolated subprocess.
  Production still sets `umask 0077` before opening the database as defense in depth.
- Keep one replica, Recreate, and no HPA. RWO does not enforce single-process
  access on a shared filesystem. Fence an unreachable old node before replacing
  its Pod; do not force-delete a live owner to bypass locking.
- The app holds an exclusive lifetime flock, uses one database connection,
  immediate transactions, DELETE journaling and `synchronous=EXTRA` (including
  directory synchronization after journal deletion).

Read-only inspection on eligible nodes (CSI may use another config path):

```sh
findmnt -t beegfs -o TARGET,SOURCE,FSTYPE,OPTIONS
grep -HE '^[[:space:]]*(tuneRemoteFSync|tuneUseGlobalFileLocks)[[:space:]]*=' \
  /etc/beegfs/beegfs-client*.conf
```

Before production writes, run the one-time ownership test with
`deploy/examples/db-ownership-check.yaml` (site template): a holder pod keeps the
database open on one node, and an attempt pod on a second node must exit with
code 3 (`database is already owned by another application`). Exit 0 from the
attempt pod means the class does not propagate exclusive locks between nodes —
do not use it for the database. The same binary mode can be used directly:

```sh
kubeflow-admin -config /etc/kubeflow-admin/config.yaml -check-db        # exit 0 owns, 3 already owned
kubeflow-admin -config /etc/kubeflow-admin/config.yaml -check-db -hold 20m
```

After that, verify crash recovery on a disposable database (`kill -9` during
writes, then `PRAGMA integrity_check`) and restart. Local automated tests cannot
verify the installed BeeGFS client's lock, cache and crash-durability semantics.

## Upgrade and backup

The republished 0.1.0 image supports this feature. Pin the new digest from
[deployment.md](deployment.md); the original image with that tag rejected `journalMode`.
Stop the old application and back up the complete database directory before
changing journal mode. SQLite switches an existing WAL database to DELETE on open;
never manually discard its WAL. Copy the entire stopped directory, including any
`-journal`, `-wal` and `-shm` files. Existing PVC class/binding cannot be changed in
place; migrate to a new claim if changing backend.

References: [SQLite network caveats](https://sqlite.org/useovernet.html),
[synchronous modes](https://sqlite.org/pragma.html#pragma_synchronous),
[BeeGFS client tuning](https://doc.beegfs.io/latest/advanced_topics/client_tuning.html).
