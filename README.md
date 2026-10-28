# Kubeflow Admin

A Go web console for OIDC-authorized Kubeflow administrators, with cross-profile inventory, change previews, durable operations and bounded audit history.

- Inspect Profiles, namespaces, Notebooks, Pods, nodes and storage.
- Enable Profile creation/quota updates, Notebook start/stop/edit, storage provisioning and node cordon/uncordon as needed.
- Separately enable restricted PV/PVC edits, unused-only storage deletion, PV reclaim, PVC-associated PodDefaults, Profile deletion and restart of Notebook StatefulSet-managed Pods.

Profile owners are read-only on update. Profile deletion can destroy data through namespace cleanup. All cluster mutations require enabled feature flags and appropriate Kubernetes RBAC.

## Getting started

You need a Kubernetes cluster with Kubeflow, a dedicated HTTPS origin, an OIDC client, an administrator group or subject allowlist, a container image and durable SQLite storage.

1. Follow the [deployment guide](docs/deployment.md) to create `deploy/site/kustomization.yaml` and `deploy/site/config.yaml`, pin the image digest, configure HTTPS routing and install the OIDC client secret. The callback is `https://YOUR_ADMIN_HOST/auth/callback`.
2. Keep all `features` flags and `notebooks.controllerContractConfirmed` **false** for the initial read-only deployment.
3. Check [database requirements](docs/database.md). The base PVC uses the cluster's default StorageClass; SQLite defaults to WAL. Verified BeeGFS requires `database.journalMode: delete` and `database.allowUnverifiedFilesystem: true`.

Once the site configuration is complete, render, review and apply it:

```sh
kustomize build deploy/site > rendered.yaml
kubectl config get-contexts
CONTEXT=default                # Example: replace with the intended context's NAME.
# Review the rendered configuration before applying.
kubectl --context "$CONTEXT" apply -f rendered.yaml
kubectl --context "$CONTEXT" -n kubeflow-admin rollout status deployment/kubeflow-admin --timeout=180s
```

The deployment runs one replica with a `Recreate` strategy. Verify administrator login, non-admin denial and inventory before enabling writes. Feature flags and RBAC grants are configured separately; follow the guide's disposable-pilot steps for each capability.

## Local development

Prerequisites: Go (CI and Docker pin **1.27.3**; module minimum **1.26**), a C compiler for race tests, Node.js for frontend tests, Python 3 and Kustomize 5.

From the repository root:

```sh
python3 -m venv .venv
. .venv/bin/activate
python3 -m pip install --require-hashes -r scripts/requirements.txt
make check build
```

This runs Go race tests, vet, API/manifest checks and Python tests, then builds `bin/kubeflow-admin`. Go tests also run the Node.js frontend tests when Node is available. Tests use local fixtures and do not require a cluster; live acceptance is covered in [verification](docs/verification.md).

To run the service outside Kubernetes, configure `kubernetes.kubeconfig` explicitly, along with OIDC, an HTTPS reverse proxy and the database, then run `bin/kubeflow-admin -config /path/to/config.yaml`. An empty kubeconfig setting uses in-cluster credentials. The service listens over HTTP; HTTPS terminates at the proxy.

## Documentation

Start with the [documentation index](docs/README.md).

- [Deployment and configuration](docs/deployment.md) · [Database storage](docs/database.md)
- [UI guide](docs/ui.md) · [Workflow acceptance checks](docs/ui-workflow-acceptance.md)
- [Storage and PodDefaults](docs/storage.md) · [Pod restarts](docs/pods.md) · [Nodes and namespaces](docs/nodes.md)
- [Operations and recovery](docs/operations.md) · [Backups](docs/backup.md)
- [History retention](docs/history.md) · [Diagnostic logging](docs/logging.md)
- [Verification](docs/verification.md) · [HTTP API](api/openapi.yaml) · [Learning guide](docs/learning.md)

## License

[MIT](LICENSE) — Copyright (c) 2026 Murat Shagirov.
