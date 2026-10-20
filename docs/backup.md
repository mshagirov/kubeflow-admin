# Database backup and restore checks

Operator-run procedure for the application's SQLite database on a node-local PV.
For another backend, adapt the filesystem backup step with the storage operator.
Stopping the admin app does not stop notebooks or other Kubeflow services.

**Backups contain identities, sessions, plans, operations and audit data.** Keep
archives, extracted copies and operational records outside Git, build contexts
and shared folders. Use encrypted storage, private permissions, and an approved
retention policy. A same-node backup is not protection against node loss.

## 1. Prepare

- Use an approved maintenance window. Disable new writes and account for active
  operations first; do not interrupt an ambiguous mutation without reviewing it.
- Identify the node holding the local PV and run the backup there. Verify the
  source directory is the app's dedicated database directory, not user storage.
- Record the time, image digest, configuration/schema version, source PV/node and
  any in-flight operations in a **private** backup record. Keep credentials in
  their existing secret-management system, not in these notes.
- The examples assume Linux/GNU tar on the source node, Python 3 with `sqlite3`
  for verification, and OpenSSH on the destination. Root access is needed only
  for reading the production directory and administering the deployment.

Replace `YOUR_CONTEXT` and the database path below with reviewed values. The
path shown is the generic local-PV template path supplied by this repository.
Do not run the procedure against a live database belonging to another service.

## 2. Stop, back up, verify and resume

Run the complete block on the database node. It stops on any error; if a step
fails after scaling down, the app deliberately remains stopped for investigation.
Do not resume until a consistent backup has been obtained or the failure reviewed.
The backup directory is printed so it remains discoverable after an error.

```sh
(
  set -eu
  umask 077
  CONTEXT=YOUR_CONTEXT
  DB_DIR=/var/lib/kubeflow-admin-db
  BACKUP_DIR=$(mktemp -d "$HOME/kubeflow-admin-backup.XXXXXXXX")
  printf 'Backup directory: %s\n' "$BACKUP_DIR"

  test -d "$DB_DIR"
  test ! -L "$DB_DIR"
  sudo kubectl --context "$CONTEXT" -n kubeflow-admin \
    scale deployment/kubeflow-admin --replicas=0
  sudo kubectl --context "$CONTEXT" -n kubeflow-admin \
    wait --for=delete pod -l app.kubernetes.io/name=kubeflow-admin --timeout=180s

  # Confirm no other process owns this directory before continuing.
  # Include the entire directory, never just admin.db or a selected journal.
  sudo tar --acls --xattrs --numeric-owner \
    -C "$(dirname "$DB_DIR")" -czf - "$(basename "$DB_DIR")" \
    > "$BACKUP_DIR/database.tar.gz"
  chmod 600 "$BACKUP_DIR/database.tar.gz"
  gzip -t "$BACKUP_DIR/database.tar.gz"
  (
    cd "$BACKUP_DIR"
    sha256sum database.tar.gz > SHA256SUMS
    sha256sum -c SHA256SUMS
  )

  sudo kubectl --context "$CONTEXT" -n kubeflow-admin \
    scale deployment/kubeflow-admin --replicas=1
  sudo kubectl --context "$CONTEXT" -n kubeflow-admin \
    rollout status deployment/kubeflow-admin --timeout=180s
)
```

Expected: checksum `OK`, followed by a successful rollout. Verify readiness and
login too. The archive includes any remaining `.lock`, `-journal`, `-wal` and
`-shm` files; never manually discard SQLite journals. `Retain` is not a backup.
Archive/checksum success establishes file-transfer integrity, not SQLite or
application correctness.

## 3. Verify an isolated restored copy

Use only your own trusted archive. Never extract over the live directory or
open the production database with this test. The scratch copy is also sensitive.
Set `BACKUP_DIR` to the directory printed above. If the source directory had a
different basename, adjust the Python path accordingly.

```sh
(
  set -eu
  umask 077
  BACKUP_DIR=/absolute/path/to/private/backup
  python3 -c 'import sqlite3; print("SQLite:", sqlite3.sqlite_version)'
  (cd "$BACKUP_DIR" && sha256sum -c SHA256SUMS)

  RESTORE_DIR=$(mktemp -d "$HOME/kubeflow-admin-restore.XXXXXXXX")
  export RESTORE_DIR
  printf 'Private restore directory: %s\n' "$RESTORE_DIR"
  # Run unprivileged: the scratch copy belongs to the operator, not UID 65532.
  tar --extract --gzip --file="$BACKUP_DIR/database.tar.gz" \
    --directory="$RESTORE_DIR" --no-same-owner --no-same-permissions

  python3 - <<'PY'
import os
import sqlite3
from pathlib import Path

db = Path(os.environ["RESTORE_DIR"]) / "kubeflow-admin-db/admin.db"
if not db.is_file():
    raise SystemExit("Restored database not found")
conn = sqlite3.connect(db.as_uri() + "?mode=rw", uri=True)
try:
    if conn.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
        raise SystemExit("FAIL: SQLite integrity check")
    if conn.execute("PRAGMA foreign_key_check").fetchall():
        raise SystemExit("FAIL: foreign-key check")
    print("Restored database integrity: OK")
    print("Foreign-key check: OK")
    print("Journal mode:", conn.execute("PRAGMA journal_mode").fetchone()[0])
finally:
    conn.close()
PY
)
```

Expected: both checks `OK`, with the selected journal mode (`wal` for the local
default). This is an archive restoration and SQLite consistency check, **not a
complete application recovery drill**. Test application recovery in an isolated,
write-disabled environment with disposable cluster/API fixtures; never run a
second restored instance against production to test it.

## 4. Copy off-node over SSH

Example: run on an operator workstation with encrypted local storage (such as
FileVault, BitLocker or LUKS). Verify encryption is enabled first. The source
account must be able to read the private backup; do not make the archive public
to facilitate transfer. Verify the SSH server's host key through a trusted
channel; never disable host-key checking.

Replace the SSH alias and source directory with your private values. They are
examples, not actual infrastructure identifiers. This method uses SSH encryption
in transit and filesystem encryption at rest; the archive itself is not encrypted.

```sh
(
  set -eu
  umask 077
  BACKUP_HOST=admin-backup-source  # your configured SSH host alias
  REMOTE_BACKUP_DIR=/home/OPERATOR/kubeflow-admin-backup.REPLACE_ME
  BASE="$HOME/.local/share/kubeflow-admin-backups"
  mkdir -p "$BASE"
  test ! -L "$BASE"
  chmod 700 "$BASE"
  OFFNODE_DIR=$(mktemp -d "$BASE/transfer.XXXXXXXX")
  export OFFNODE_DIR
  printf 'Off-node backup directory: %s\n' "$OFFNODE_DIR"

  scp -o BatchMode=yes -o ConnectTimeout=10 \
    "$BACKUP_HOST:$REMOTE_BACKUP_DIR/database.tar.gz" \
    "$BACKUP_HOST:$REMOTE_BACKUP_DIR/SHA256SUMS" "$OFFNODE_DIR/"
  chmod 600 "$OFFNODE_DIR/database.tar.gz" "$OFFNODE_DIR/SHA256SUMS"

  # Portable verification for Linux/macOS; print no database contents.
  python3 - <<'PY'
import gzip
import hashlib
import os
from pathlib import Path

root = Path(os.environ["OFFNODE_DIR"])
lines = (root / "SHA256SUMS").read_text().strip().splitlines()
if len(lines) != 1:
    raise SystemExit("Unexpected checksum manifest")
parts = lines[0].split()
if len(parts) != 2 or parts[1].lstrip("*") != "database.tar.gz":
    raise SystemExit("Unexpected checksum filename")
archive = root / "database.tar.gz"
digest = hashlib.sha256()
with archive.open("rb") as f:
    for chunk in iter(lambda: f.read(1024 * 1024), b""):
        digest.update(chunk)
if digest.hexdigest() != parts[0]:
    raise SystemExit("FAIL: checksum mismatch")
with gzip.open(archive, "rb") as f:
    while f.read(1024 * 1024):
        pass
print("Off-node backup checksum: OK")
print("Transferred gzip integrity: OK")
PY
)
```

Expected: both checks `OK`; destination directory `0700`, files `0600`. The
source copy remains untouched. Checksums detect corruption, not malicious
replacement of both files; SSH authentication and protected storage are essential.
Periodically repeat the isolated restore check on an off-node copy and maintain
an independent retained backup, rather than overwriting your only recovery point.

## 5. Actual recovery and retention

Follow [operations and recovery](operations.md#recovery-and-upgrades) before
restoring a production instance. Restore numeric ownership/modes for the runtime
UID/GID, reconcile operations with actual Kubernetes state, and handle restored
sessions/plans as sensitive stale state. A database restore does not undo cluster
changes. Back up non-secret deployment configuration separately and recover OIDC
credentials through your secret manager.

Retain and remove backups/scratch copies under your approved policy. Do not
commit them, attach them to issues, or paste their contents into chat. This
procedure creates no automatic schedule; assign an operator and test restores
regularly.
