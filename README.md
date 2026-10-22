# Kubeflow Admin

A single Go service for OIDC-authorized Kubeflow administrators: cross-profile inventory, typed previews, durable operations and bounded audit history. No arbitrary manifests, Kubernetes proxy, Secret reads, exec, impersonation or unrestricted resource deletion. Existing PV/PVC editing uses a separately enabled restricted YAML projection; direct storage deletion is separately enabled and unused-only. Template-driven storage adds a standalone static PV registration workflow and PVC creation bound to an existing PV, with read-only server-owned registration-template values. Optional PodDefault creation/overwrite is a substep of reviewed PVC creation, and related PodDefault cleanup is a substep of reviewed PVC deletion; there is no standalone PodDefault page or mutation endpoint. The separately enabled Pod restart workflow uses normal eviction of supported controller-owned Pods, never forced deletion. The separately enabled Profile deletion workflow deletes a reviewed Profile and waits for normal cleanup of its owned namespace; it never bypasses finalizers and can destroy PVC-backed data. Profile quota editing keeps the owner read-only because the upstream controller does not support owner transfers.

**Start read-only.** Deployment requires a dedicated HTTPS host, issuer/client registration, exact administrator group/subject, image and durable database storage. The base uses the default StorageClass with WAL for local disks; verified BeeGFS additionally needs `journalMode: delete` and `allowUnverifiedFilesystem: true`. See [database requirements](docs/database.md). A node-local PV option is also supplied; deployment remains an explicit operator action.

## Local checks

Use the reviewed Go 1.27.3 release toolchain (the module's Go 1.26 minimum is not a security-version recommendation), Kustomize 5 and Python 3. Install the pinned manifest/OpenAPI tooling in a virtual environment:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install --require-hashes -r scripts/requirements.txt
```

If using the repository's `.venv`, activate it first (`source .venv/bin/activate`), so `make` uses the Python interpreter that has PyYAML installed.

```sh
make test
make vet
make build
make manifests
make render                         # offline; does not contact Kubernetes
make render OVERLAY=deploy/overlays/local-storage
```

Tests use local HTTP/fake Kubernetes fixtures. They do not prove RKE2 admission, Notebook controller behavior, storage isolation/expansion, crash durability, OIDC integration or gateway security. No development authentication bypass exists. A real local browser integration needs a dedicated trusted HTTPS origin, a registered OIDC client and an explicitly selected **disposable** kubeconfig; do not point development at production.

## Deploy in brief

Read [docs/deployment.md](docs/deployment.md) for the exact, reviewed steps. Summary:

```sh
# 1. Prepare a site overlay (do NOT deploy deploy/base unconfigured)
mkdir -p deploy/site && cp deploy/base/config.yaml deploy/site/config.yaml
#    Edit config.yaml: HTTPS publicURL, Dex issuer, clientID, exact adminGroups/subjects.
#    Keep all `features` false and `controllerContractConfirmed` false for the read-only stage.

# 2. Use ../base in the site overlay to provision on the default StorageClass.
#    For BeeGFS set database.journalMode: delete AND allowUnverifiedFilesystem: true,
#    only after verifying client settings in docs/database.md.
#    Alternatively configure deploy/overlays/local-storage for a local disk (WAL).

# 3. Register a Dex OIDC client (code + PKCE), callback https://YOUR_HOST/auth/callback,
#    then create the namespace and secret (never commit these):
kubectl apply -f deploy/base/namespace.yaml
kubectl -n kubeflow-admin create secret generic kubeflow-admin-oidc \
  --from-file=client-secret=/secure/local/path/oidc-client-secret

# 4. Build and scan the reviewed source and both runtime architectures BEFORE
#    publishing the verified artifacts; then pin the fresh registry digest.
#    See docs/deployment.md and docs/verification.md for release evidence.
kustomize build deploy/site > rendered.yaml   # review: no REPLACE/.invalid values remain
kubectl apply -f rendered.yaml
kubectl -n kubeflow-admin rollout status deployment/kubeflow-admin --timeout=180s
```

Then: log in as an admin over HTTPS, confirm a non-admin is denied, compare inventory with `kubectl`, and only afterward enable write features and the matching RBAC overlay for a disposable pilot namespace. The existing `kubeflow/kubeflow-gateway` is HTTP-only, so a dedicated HTTPS route is required before login works. Logout returns to the public `/auth/signed-out` page; login rate limiting defaults to `auth.loginSourceLimit: global`, and `forwarded` requires `auth.trustedProxyCIDRs` (see [deployment](docs/deployment.md), section 5). The optional `ui.footerText` key adds a site footer — plain text or a small, sanitized amount of branding HTML such as a company logo and a support link; the value is read once at startup, so a changed footer needs a rollout restart (see [deployment](docs/deployment.md), section 2.1). The optional PodDefault mutation RBAC (`deploy/overlays/poddefaults/`) is separate and must stay disabled until the installed CRD/admission schema and PVC cleanup policy are reviewed; the base read role still grants `get`/`list` so PVC deletion can detect references. Keep `features.podDefaults` false by default. Before adding `storage.registrationTemplates` or `features.podDefaults`, back up the stopped database/journal set and the old config: old binaries reject the new config keys, the store schema remains version 3 with no new columns assumed, and rolling back the image alone is invalid. Active operations using the new actions cannot be replayed by an old app; use reviewed restore/reconciliation instead.

## Deployment and operations

- [Learning: 15 hands-on lessons to reimplement, test and deploy this app](docs/learning.md)
- [Configure, build, deploy and stage writes](docs/deployment.md)
- [Current UI: pages, layouts, notifications and action boundaries](docs/ui.md)
- [Disposable-pilot acceptance checks for profile, storage and notebook workflows](docs/ui-workflow-acceptance.md)
- [Local evidence vs live acceptance](docs/verification.md)
- [Backups, recovery and safety boundaries](docs/operations.md)
- [Step-by-step database backup, restore checks and off-node SSH transfer](docs/backup.md)
- [Diagnostic logging and bounded file rotation](docs/logging.md)
- [History windows, readable actors and bounded audit retention](docs/history.md)
- [Pods page and separately authorized, disruption-budget-aware restart](docs/pods.md)
- [Nodes, namespaces, cordon/uncordon and Workspace Health](docs/nodes.md)
- [Responsive UI, bounded History and safe Pod restart delivery plan](docs/ui-history-plan.md)
- [Approved storage templates, static provisioning and PodDefault cleanup](docs/storage.md)
- [Consolidated final plan and agent work packages](kubeflow-admin-webapp-plan-final.md)
- [Confirmed site compatibility and remaining gates](docs/compatibility.md)
- [Implemented HTTP contract](api/openapi.yaml) — the older design plans describe a broader proposed API; this spec follows the implementation.
- `bash scripts/discover.sh YOUR_CONTEXT > discovery.txt`: optional user-run, read-only selected metadata. Review locally before sharing; do not commit its output.

`deploy/base` is a dedicated namespace, one-replica Recreate deployment, hardened nonroot container, reduced read-only RBAC, persistent SQLite and ingress-only NetworkPolicy. Permission overlays do **not** automatically enable feature flags. HTTPS routing and egress isolation are clearly marked site templates, not ready certificates or universal network defaults.

## License

[MIT License](LICENSE) — Copyright (c) 2026 Murat Shagirov.
