# Deployment

Deploy read-only first, then enable individual workflows after [live acceptance](ui-workflow-acceptance.md). Examples require your own cluster, image and configuration values.

## 1. Compatibility and outstanding decisions

Confirm the installed Profile/Notebook APIs and controller contracts, OIDC claims, StorageClasses, admission rules and ingress configuration using [compatibility](compatibility.md). Local tests do not establish compatibility with your cluster.

Prepare a dedicated HTTPS hostname, confidential OIDC client, exact administrator group or subject allowlist, image digest, durable database storage and backup destination. The service itself listens on HTTP port 8080; the ingress terminates HTTPS.

## 2. Prepare a real site overlay and DB disk

From the repository root:

```sh
mkdir -p deploy/site
cp deploy/base/config.yaml deploy/site/config.yaml
cat > deploy/site/kustomization.yaml <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../base
configMapGenerator:
  - name: kubeflow-admin-config
    behavior: replace
    files:
      - config.yaml
images:
  - name: kubeflow-admin
    newName: registry.example.org/platform/kubeflow-admin
    digest: sha256:REPLACE_WITH_PUSHED_IMAGE_DIGEST
EOF
```

Edit `deploy/site/config.yaml` against [the shipped configuration](../deploy/base/config.yaml) and `internal/config/config.go`; unknown keys are rejected.

| Setting | Configure |
| --- | --- |
| `server.publicURL` | Dedicated HTTPS origin, without a path prefix |
| `auth` | HTTPS issuer, client ID, mounted secret file and exact `adminGroups` or `adminSubjects`; subjects are bound to the configured issuer |
| `policy` | Add site system namespaces to `protectedNamespaces`; restrict `allowedNamespaces` to the pilot initially |
| `storage` / `notebooks` | Approved classes, prepared templates, images and GPU presets |
| `features` | Keep every flag false initially |
| `notebooks.controllerContractConfirmed` | Keep false until the installed controller is verified |
| `database` | Durable path and journal mode appropriate to its filesystem |

An empty namespace allowlist removes that restriction but retains built-in `kube-*` and configured protections. It does not grant Kubernetes permissions.

The base PVC requests 5Gi from the default StorageClass. SQLite defaults to WAL on local/block storage. BeeGFS requires DELETE journaling, `allowUnverifiedFilesystem: true` and verified locking/fsync; follow [database requirements](database.md).

For node-local storage, use `../overlays/local-storage` instead of `../base`. Configure its exact node hostname label, dedicated path and capacity. On the selected node, prepare a new directory owned by UID/GID 65532, mode 0700, on durable local storage. Do not repoint an existing bound claim or reuse a user-data directory. Local storage means node failure causes app downtime; Retain is not a backup.

Keep one replica, `Recreate` and no autoscaler. Configuration loads at startup; Kustomize's generated ConfigMap name changes trigger a rollout. Keep site configuration and credentials outside Git.

### 2.1 Optional console footer text

`ui.footerText` defaults to empty and accepts up to 16,384 Unicode code points:

```yaml
ui:
  footerText: |
    <strong>Platform team</strong>
    <a href="https://support.example.org">Support</a>
    <img src="https://cdn.example.org/logo.png" alt="Company logo" width="120">
```

The sanitizer permits basic text formatting, lists, links and images; it removes scripts, frames, forms, event handlers, inline styles, SVG and unsafe URL schemes. Links open with `noopener noreferrer`; image dimensions are bounded to 1024 pixels and titles/alt text to 200 characters. Scheme-relative images become HTTPS. Empty or fully removed content produces no footer.

External images are fetched by the administrator's browser, not the server, and their origins are added to the page's image CSP. Use trusted image hosts. Re-render and apply configuration changes; a running process does not hot-reload the footer.

## 3. Register OIDC and install its secret

Register a separate confidential client with authorization-code flow, PKCE S256, callback `https://YOUR_ADMIN_HOST/auth/callback` and scopes `openid profile email groups`. Verify the provider releases the configured group claim or subject. Discovery/token/JWKS endpoints must be reachable with trusted TLS; for private roots, mount a CA bundle and configure `SSL_CERT_FILE` rather than disabling verification.

```sh
kubectl config get-contexts
CONTEXT=default # Example: select the intended context.
kubectl --context "$CONTEXT" apply -f deploy/base/namespace.yaml
kubectl --context "$CONTEXT" -n kubeflow-admin create secret generic kubeflow-admin-oidc \
  --from-file=client-secret=/secure/local/path/oidc-client-secret
```

The deployment mounts the secret read-only; the app has no Kubernetes Secret-read grant. Rotate through your secret manager and restart the deployment. The app does not manage LDAP passwords or users.

## 4. Verify, publish and render

Use an image verified for the exact source revision and pin its immutable registry digest. Follow [verification](verification.md) for tests, scans, browser checks and release evidence.

`make image` builds/loads the local architecture. `make image-push IMAGE=repository:VERSION` builds and pushes `VERSION` and `latest` together for amd64/arm64, with SBOM/provenance. It requires a clean tracked/untracked worktree and builds from archived `HEAD`, excluding ignored files. It rejects missing tags, digest-only references and placeholder versions. Because it rebuilds, it is **not** a way to promote a previously scanned OCI artifact; publish that exact artifact without rebuilding instead.

```sh
kustomize build deploy/site > rendered.yaml
```

Review the image digest, hostname/issuer/group, storage, namespace policy and RBAC. Replace all `REPLACE` and `.invalid` values. Repository manifest checks do not validate live site compatibility.

## 5. HTTPS and network review

- Adapt [the Istio example](../deploy/examples/istio-https.yaml) for the dedicated host and existing TLS credential, then include it in the site overlay. It supplies routing, not DNS or certificates. An HTTP-only Gateway cannot provide HTTPS by itself.
- The base NetworkPolicy allows TCP 8080 from Pods labelled `istio: ingressgateway` in `istio-system`. Adjust to your actual ingress and verify CNI enforcement and probes.
- Sidecar injection is disabled. Review inherited mTLS/auth policies; the dedicated OIDC callback must not enter an OAuth2 Proxy redirect loop. Test direct access and forged identity headers.
- Egress is unrestricted by the base. Adapt [the egress example](../deploy/examples/egress-networkpolicy.yaml) only after identifying DNS, Kubernetes API and issuer endpoints; test CNI/DNAT behavior.

Login limiting defaults to `auth.loginSourceLimit: global` (100 starts/minute). `peer` adds a 10/minute bucket per immediate peer; use it only without a shared proxy. `forwarded` requires `auth.trustedProxyCIDRs` and a proxy that appends or replaces untrusted forwarding headers. The app validates the chain right-to-left; unusable chains get only the global limit. Apply client-aware abuse controls at the ingress. Logout revokes the local session and redirects to `/auth/signed-out`; it is not upstream IdP logout.

## 6. Apply read-only, then a disposable write pilot

```sh
kubectl --context "$CONTEXT" apply -f rendered.yaml
kubectl --context "$CONTEXT" -n kubeflow-admin rollout status deployment/kubeflow-admin --timeout=180s
kubectl --context "$CONTEXT" -n kubeflow-admin get pods,pvc
```

Verify admin login, non-admin denial, logout, inventory, readiness and restart behavior. Take a verified [backup](backup.md) before enabling writes. Readiness is not a full RBAC/controller/storage acceptance test.

Every mutation requires `features.writesEnabled`. Additional gates and grants:

| Capability | Additional configuration | RBAC overlay |
| --- | --- | --- |
| Profile create/quota update; dynamic PVC create | Approved namespace/class policy | `writes` |
| Existing Notebook start/stop/edit | `notebooks.controllerContractConfirmed` | `writes` |
| Static PV create/reserve and template-bound PVC | `staticStorage`, approved templates; unreserved creation also requires isolated classes | `static-storage`; PVC creation also needs `writes` |
| PVC expansion | `pvcExpansion`, compatible bound CSI volume | `pvc-expansion` |
| Restricted PV/PVC editing | `storageEditing`; growth also needs `pvcExpansion` | `storage-editing` |
| Unused-only PV/PVC deletion | `storageDeletion` | `storage-deletion` |
| Standalone/post-delete PV reclaim | `pvReclaim`, `storageDeletion`, isolated class and per-plan confirmation | `pv-reclaim` |
| PVC-associated PodDefaults | `podDefaults`, applicable PVC workflow gates | `poddefaults` |
| Profile deletion | `profileDeletion` | `profile-deletion` |
| Notebook Pod restart | `podRestarts`, confirmed Notebook contract | `pod-restarts` |
| Node cordon/uncordon | `nodeCordon` | `node-cordon` |

Flags above are under `features` unless qualified. Copy each needed overlay's RBAC file into `deploy/site` and list it under `resources`; include the base only once rather than stacking overlays. Prefer namespace-scoped pilot grants where applicable. PodDefault reads are in the base so PVC deletion can detect references even when mutation is disabled.

Follow [workflow acceptance](ui-workflow-acceptance.md), [storage](storage.md), [Pods](pods.md) and [Nodes](nodes.md). Patch RBAC is not field-limited; admission and coordination must constrain other writers. Disabling flags does not revoke bindings, undo writes or release unresolved locks. Removing a binding from Kustomize does not delete the live binding; revoke the exact optional grants separately.

## 7. Profile deletion — separate opt-in, irreversible

Enable `writesEnabled` and `profileDeletion`, with the separate Profile `delete` grant. For a pilot, restrict the ClusterRole with `resourceNames: [PILOT_PROFILE]`; a namespaced Role cannot authorize a cluster-scoped Profile. This limits the Profile name, not its cascade.

Before enabling deletion, verify the installed controller owns the same-name namespace and performs normal cleanup. Coordinate external writers and enforce ownership rules: the Profile must not gain additional namespaces or cross-namespace dependents, and the workspace must not be recreated/reparented during cleanup. App locks do not constrain external writers, and reads plus DELETE are not atomic.

The action requires a reason and exact, untrimmed typed name. It sends one UID/resourceVersion-conditioned Profile deletion with Background propagation. Controllers remove the namespace and contents; the app never directly deletes a Namespace or bypasses finalizers. Success requires both reviewed objects to be absent. Replacements, read errors and unfinished cleanup cause `needs-review`, retaining Profile/namespace locks.

Namespace cleanup deletes PVCs and namespaced snapshots. Delete reclaim policies may destroy backing data; Retain may leave storage behind and is not a backup or verified wipe. Review actual storage outcomes separately. Use [offline reconciliation](operations.md#offline-reconciliation-of-a-profile-deletion) for an uncertain deletion; there is no force or automatic replay.
