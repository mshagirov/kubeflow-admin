# Diagnostic logging and rotation

Diagnostic logs and the database Audit trail serve different purposes. Rotating
a JSON log file does **not** expire or back up SQLite audit records.

## Default: stdout

The app uses Go `log/slog` with a JSON handler. An empty `logging.path` keeps
output on stdout, so no additional writable container directory is needed.
Kubernetes/kubelet manages container-log rotation; operators must verify their
platform's size/count settings. A Deployment cannot impose a kubelet rotation
policy. Keep the application's disk, memory and log volume monitored.

## Optional private file sink

Go's standard `slog` package does not include a rotating file handler. The app
provides a bounded writer used by `slog.JSONHandler`:

```yaml
logging:
  path: /var/log/kubeflow-admin/admin.log
  maxSizeMiB: 10
  maxBackups: 5
  maxAgeDays: 7
```

- File output replaces stdout; these messages no longer appear in `kubectl logs`.
- Mount writable storage at the chosen directory. The supplied container root
  is read-only; enabling a file path without a writable mount fails startup.
- Use a dedicated directory owned by runtime UID/GID 65532, mode `0700`.
  Active and numbered backup files are `0600`; symlinks, foreign ownership and
  group/world access are rejected. Do not share a path with other processes.
- The active file rotates before a write would exceed the size budget. Up to
  `maxBackups` numbered backups are retained in addition to the active file.
  The example budgets at most **60 MiB of log content**, plus filesystem overhead.
- Existing retained files larger than the configured size limit fail setup
  rather than silently retaining oversized backups or truncating historical data.
  Archive them through an operator-reviewed procedure before reducing limits.
- Age cleanup occurs at setup and rotation, not continuously while idle.
  Size/count limits still bound stored content during idle periods.
- The sink substitutes a small fixed warning for a write exceeding 1 MiB.
  This bounds sink writes, not arbitrary memory allocated by callers before
  encoding: never log large resource objects or unbounded collections.
- Setup errors are fatal. Runtime sink errors are reported when the sink closes;
  file diagnostics are not a guaranteed durable audit channel. No automatic
  redaction or remote shipping is provided.

The application does not log credentials, session cookies, full manifests or
whole audit payloads. Operators must apply the same rule to any added logging.
Keep logs and backups private, preferably on encrypted storage. File rotation is
not off-node protection; use the [backup procedure](backup.md) for SQLite and
your approved diagnostic collection system for logs.
