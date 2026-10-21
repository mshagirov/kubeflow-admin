# History, identity labels and retention

History combines Operations and Audit tabs. Tables search/sort **loaded records**,
not every record retained in the database. Each normal response is a recent
window of at most 1,000 records; Operations additionally exposes bounded unresolved
work so old needs-review items cannot disappear behind recent successes.

Audit summaries group stages of an operation; detailed stage events remain
available in that group. A partial loaded window may not contain every stage.
Operations shows Time, Actor, Resource kind/namespace/name, Action and State.
Reason displays only the administrator's input, not concatenated operation IDs.
Failure details appear on demand from the State column; a fixed indicator slot
keeps rows aligned. Resource/action/operation identifiers have separate fields
or are retained in audit stage details.

The UI never renders the raw `issuer|subject` Actor ID. New records show the
captured display label; historical records without one show
`Legacy administrator`.

## Administrator display names

New plans and accepted operations capture a display label from the signed OIDC
preferred username, with email and subject fallbacks. It is stripped of control
characters and bounded in length. The label is presentation only: authorization,
actor-bound plans and idempotency still use the stable issuer/subject key.

Labels on accepted operations are immutable through checkpoints and completion.
Legacy records without a captured label are shown as `Legacy administrator`; the
UI does not expose the encoded issuer/subject key or infer a name from it. Group
changes are not instant LDAP revocation: existing application sessions remain
bounded by their short lifetime.

## Retention configuration

```yaml
history:
  retentionDays: 90
  maxAuditRecords: 50000
  maxAuditBytes: 67108864
  maxOperationRecords: 100000
```

- The Audit view has an upper age horizon of 90 days (configurable shorter).
  Row/serialized-byte budgets can retain **less** history under pressure; 90 days
  is not a guaranteed retention minimum or an archival export contract.
- A background janitor purges expired/oldest eligible audit rows in bounded
  batches. It does not run full VACUUM or long cleanup work on login/request paths.
- Unresolved operation locks and immutable acceptance evidence belong to safety
  state, not disposable display history. Never remove them merely to reclaim
  audit space. Needs-review items require operator reconciliation.
- Completed operations retain compact retry identity/tombstones after display
  history expires, preventing an old idempotency key from replaying a mutation.
  Their acceptance-ledger reason and change payloads are redacted after the
  terminal-history cutoff; unresolved acceptance payloads remain protected.
  Total admitted operation records are bounded; capacity exhaustion rejects new
  changes instead of silently forgetting replay protections. Do not delete claim
  rows by hand to reset this limit.
- At most 1,000 unresolved operations may be admitted. Reconcile unsafe outcomes
  rather than accumulating needs-review records indefinitely.
- These are **logical data bounds**, not a hard cap on the whole SQLite/WAL file,
  database indexes, safety state, temporary query memory or backups. Configure
  filesystem capacity/monitoring and maintain free-space headroom. SQLite may
  reuse freed pages without shrinking its database file.

Audit age filtering also occurs when records are read, independent of janitor
progress. Physical cleanup is gradual: separate short transactions remove at most
500 audit rows and compact at most 50 completed operations per pass, each with a
500 ms context budget. Completed work is found through an indexed queue rather
than by rescanning compacted history. Compaction failure cannot roll back an
already committed audit-pruning batch. A large upgrade backlog can remain on disk
across multiple passes even though expired events no longer appear in Audit.

History APIs expose `X-History-Limit`, `X-History-Window` and
`X-History-Retention-Days`. They keep array responses for compatibility. A viewer's
visible row count or search result is not proof that older records never existed.
Use approved archival tooling and platform Kubernetes audit logs if independent
or longer-term attribution is required.

## Upgrade and recovery precautions

Take and verify a [consistent backup](backup.md) before deploying the schema-3
acceptance-ledger migration. The migration copies authentic accepted evidence;
it must not invent missing audit reasons or repair corrupt active claims silently.
Missing/inconsistent required evidence fails closed for operator review.

Do not roll back only the image after retention has removed presentation rows
that an older release treated as safety evidence. Recovery needs a compatible
database/configuration backup and reconciliation with actual Kubernetes state;
restoring the database never undoes cluster changes.

Backup/export retention is separate. Expiring a live audit row does not erase
an old backup, log collector, snapshot or exported file. Protect and expire those
copies under your organization's policy. Diagnostic JSON log rotation is also
separate; see [logging](logging.md).
