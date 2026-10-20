# Deployment

All `kubectl` commands below are **operator-run instructions**, not commands executed during implementation. Use a reviewed context explicitly. Do not apply unconfigured manifests.

## 1. Compatibility and outstanding decisions

Record your own cluster's facts before deploying: Kubernetes/RKE2 version and architecture; installed Profile/Notebook controller digests; Dex/OIDC issuer and gateway ingress details; and the StorageClasses actually in use. Do not rely on another site's versions. Keep the Kubernetes client minor aligned with your installed server. A common setup has an existing Kubeflow gateway that serves **HTTP only**; that is not an HTTPS endpoint and does not satisfy the admin app's secure-cookie/OIDC requirements (see section 5).

For **user volumes**, note for each approved class its backend, reclaim policy, binding mode, access modes and whether expansion is actually supported. `allowVolumeExpansion: true` is not proof that the backend, driver, filesystem, quota and workflow support a successful resize — verify each before enabling the capability. Network filesystems are **not suitable for SQLite/WAL**. Existing Bound PVs are in use: never reuse their handles/export directories for a new user, and do not assume separate PVs isolate the same backend data. Keep static templates empty/disabled until reviewed. A CSI import needs a distinct approved prepared volume handle, exact driver and reviewed attributes; NFS needs a distinct prepared export/directory with reviewed ACLs and mount options. Do not infer filesystem ownership from LDAP identity.

Still required: dedicated admin hostname/DNS, public issuer URL and trusted CA, exact admin group or issuer-bound subject, dedicated confidential client secret, ingress workload selector, certificate provisioning, safe DB node/filesystem and backup destination. Run `scripts/discover.sh` yourself if more selected metadata is needed; it never requests Secrets, environment variables or full manifests. Names/images/hosts can still be sensitive.

## 2. Prepare a real site overlay and DB disk

Base uses the default StorageClass; local disks keep the WAL default, while BeeGFS requires `journalMode: delete` plus `allowUnverifiedFilesystem: true` — follow [database requirements](database.md), including the global-lock and remote-fsync verification, before setting either. Use the republished 0.1.0 image pinned below. The group placeholder still requires configuration. For an optional **node-local** deployment instead:

1. Select a durable node and record its exact `kubernetes.io/hostname` label (not an assumed DNS name). On that node, select an existing local ext4/XFS filesystem with sufficient capacity, proper locking and durable fsync; do not use a network mount, symlink, ephemeral boot disk or notebook data directory.
2. As the node operator, prepare a **new dedicated** `/var/lib/kubeflow-admin-db` directory. After confirming it is absent and the parent is the intended filesystem, `sudo install -d -o 65532 -g 65532 -m 0700 /var/lib/kubeflow-admin-db`. Do not recursively chown existing data. Verify permissions, free space, backup and node-loss recovery.
3. Edit `deploy/overlays/local-storage/storage.yaml`: exact node hostname label, chosen prepared path and size. This creates a static `local` PV with required node affinity, a `kubernetes.io/no-provisioner` StorageClass `kubeflow-admin-local`, and a specifically prebound DB PVC. No external provisioner is needed. Capacity is a declaration, not a directory quota.

One active process only: one replica, `Recreate`, no autoscaler. RWO is not a substitute for that constraint. Local storage pins the Pod to that node; node outage means app outage. A Retain PV prevents automatic cleanup, **not data loss**; it provides no replication or HA. Local PV provisioning here is application infrastructure, not an enabled arbitrary user-volume/hostPath action.

Create the site overlay inside this checkout (so relative Kustomize paths resolve):

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

The historical `docker.io/muratshagirov/kubeflow-admin:0.1.0` image (linux/amd64) does **not** include these review fixes. Its old digest is recorded only in [historical verification](verification.md), not recommended as the fixed release. Build, publish and scan the reviewed source (section 4), then replace the overlay's placeholder with the fresh registry digest. Do not assume a tag or a previous digest represents current source.

Configure `deploy/site/config.yaml` strictly against `internal/config/config.go`, **not** the older design-plan schema. Set `server.publicURL` to the HTTPS origin (no path), issuer/client, exact admin groups or subjects, approved user classes/images/presets, protected namespaces and preferably a single pilot `allowedNamespaces` entry. `adminSubjects` contains exact `sub` strings and is bound to the configured issuer. Keep every `features` switch false (including `podRestarts`) and `controllerContractConfirmed` false initially. Pod restart additionally needs its separate, namespace-scoped eviction permission and a disposable pilot; see [Pods](pods.md). Add every site's system namespace to the protected list. Config is read at startup; the generated ConfigMap name changes trigger a restart. Never put credentials in it.

For an already operator-provisioned safe DB PVC, adapt the base PVC to that exact class/binding before apply instead. Existing PVC binding/class fields are generally immutable: do not overwrite a live claim to switch backend. Follow backup/migration recovery procedures.

## 3. Register OIDC and install its secret

Register a **separate confidential** Dex/OIDC client `kubeflow-admin` with authorization-code flow and PKCE S256, exact callback `https://YOUR_ADMIN_HOST/auth/callback`, and scopes `openid profile email groups`. Confirm the provider actually releases the exact administrator group claim. The application does not handle LDAP credentials. Avoid wildcard callback URLs. Match issuer discovery's `issuer` exactly; token/discovery/JWKS endpoints must be reachable from the app and validate with a trusted CA. For private roots, prepare a reviewed CA bundle mount and `SSL_CERT_FILE` environment entry via a site Deployment patch; do not disable TLS verification.

Create the namespace first using only its manifest, then a separate Secret from a local restrictive file containing just the client secret (not YAML). Use your secret manager where possible. Do not paste the secret into shell arguments/history or commit/export it:

```sh
kubectl --context YOUR_CONTEXT apply -f deploy/base/namespace.yaml
kubectl --context YOUR_CONTEXT -n kubeflow-admin create secret generic kubeflow-admin-oidc --from-file=client-secret=/secure/local/path/oidc-client-secret
```

The Pod receives a read-only Secret file; the app has no Kubernetes Secret-read permission. For rotation, update through the site's secret manager and restart the deployment; verify the old client secret is revoked upstream. Protect both Secret and SQLite backups; DB contains administrative identity/session/login state.

## 4. Build, push and render

```sh
make test vet manifests
make image-push IMAGE=registry.example.org/platform/kubeflow-admin:YOUR_RELEASE
# Record registry output digest and insert it in deploy/site/kustomization.yaml.
kustomize build deploy/site > rendered.yaml
```

Buildx publishes `linux/amd64,linux/arm64` (cluster currently amd64), provenance and SBOM. Use a Buildx `docker-container` builder for multi-platform publication (`docker buildx create --driver docker-container --use` if needed). Docker uses digest-pinned Go 1.27.2 (module minimum remains 1.26), pure-Go SQLite, `CGO_ENABLED=0`, read-only module resolution, trimmed paths, no VCS/build ID; runtime is digest-pinned distroless static nonroot with CA roots and UID/GID 65532. Identical pinned inputs produce reproducible binary inputs; registry attestations/image metadata can differ between builds. Maintain the toolchain/image digests for security updates, scan before promotion, and configure registry pull credentials externally if needed. `make image` builds/loads only the local host architecture. Do not commit rendered site manifests/discovery output.

Review rendered resources for the real image digest, real group/host/issuer, node affinity, storage and RBAC. Unconfigured `.invalid` URLs and `REPLACE` values must be gone. `make manifests` checks repository invariants, **not** live site compatibility.

## 5. HTTPS and network review

Copy `deploy/examples/istio-https.yaml` into the site overlay and add it under `resources` only after replacing the dedicated host and existing TLS credential. It creates a separate explicitly named Gateway/VirtualService; it does **not** modify `kubeflow/kubeflow-gateway`, create DNS, obtain a certificate, or inherit TLS from that HTTP-only Gateway. With Istio 1.28, confirm selector/cross-namespace gateway rules and credential location (normally ingress workload namespace). Provision a trusted exact-host certificate, never a placeholder/wildcard route. If site policy requires reusing the existing Gateway, first add a reviewed exact-host HTTPS server there and bind the VirtualService to `kubeflow/kubeflow-gateway` instead; avoid competing host routes. Alternatively, a separately confirmed upstream TLS terminator may forward a dedicated-host route, but its trusted TLS/DNS/auth/network configuration must be explicitly designed and tested; the existing HTTP Gateway alone is insufficient. No path-prefix rewrite.

Default ingress policy only permits TCP 8080 from Pods matching `istio: ingressgateway` **in** `istio-system`; discover actual labels/namespace and adjust before apply. Namespace injection is disabled: gateway-to-app is HTTP within the permitted network, and app verifies its own OIDC. Review inherited strict mTLS, PeerAuthentication, RequestAuthentication, AuthorizationPolicy and external auth rules. Avoid applying Kubeflow's OAuth2 Proxy redirects to the dedicated app login/callback; do not globally weaken Kubeflow authentication. Test forged identity headers (ignored), non-admin login, direct Pod access and callback routing. NetworkPolicy enforcement requires a working CNI implementation; verify probe behavior too.

Login rate limiting is keyed by the `auth.loginSourceLimit` field (`global`, also the empty default; `peer`; or `forwarded`). Because every client reaches the shipped route through one reverse proxy with a shared peer address, the default `global` mode applies only the bounded global login budget and deliberately no per-source bucket — a per-peer bucket would let one client exhaust the shared limit for everyone. Set `peer` only when the process is directly exposed with no proxy. To key the limiter on a real client address behind a proxy, set `forwarded` and list the proxy networks in `auth.trustedProxyCIDRs` (valid CIDR ranges, required in that mode); the app trusts `X-Forwarded-For` only when the immediate peer is in that list and validates the chain right-to-left, so never list client-reachable or untrusted networks. The gateway must append the actual client address or overwrite untrusted incoming forwarding headers. Unusable chains receive only the global limit, not a trusted source bucket. The process-wide budget is 100 login starts per minute; enabled source buckets allow 10 each. A public client can still exhaust a global budget: configure client-aware rate limiting and abuse controls at the trusted ingress rather than treating the application limiter as denial-of-service protection. Logout now redirects to the public, script-free `/auth/signed-out` page (`/signed-out` is an alias) instead of silently restarting the OIDC flow. These names match `internal/config/config.go`.

No egress policy is silently imposed: DNS, Kubernetes API, issuer discovery/token/JWKS and redirects must work. `deploy/examples/egress-networkpolicy.yaml` is a site-specific isolation template, deliberately not applied. Resolve actual API/IdP CIDRs, ports, DNS/node-local DNS and CNI DNAT semantics before use; plain policies cannot allow FQDNs. Test in staging before restricting egress.

## 6. Apply read-only, then a disposable write pilot

```sh
kubectl --context YOUR_CONTEXT apply -f rendered.yaml
kubectl --context YOUR_CONTEXT -n kubeflow-admin rollout status deployment/kubeflow-admin --timeout=180s
kubectl --context YOUR_CONTEXT -n kubeflow-admin get pods,pvc
```

Use HTTPS to test admin login, non-admin denial, inventory including existing notebooks, secure cookies, logout, readiness and a restart. Confirm no write permissions on the base ServiceAccount using your platform's RBAC review (do not use app credentials to administer RBAC). Do not treat readiness as proof of all permissions or controller/storage compatibility.

Then, on a disposable **pre-existing test profile/notebook/PVC** with backups and no valuable running computation:

1. Independently verify controller 1.10 start/stop annotation and culling behavior, installed schema, child UID ownership, admission restrictions and driver behavior. A CR dry-run cannot validate the generated Pod or controller convergence.
2. Restrict `policy.allowedNamespaces` to the pilot. Copy `deploy/overlays/writes/rbac.yaml` to `deploy/site/writes-rbac.yaml` and add `writes-rbac.yaml` under the site's `resources`. Set `features.writesEnabled: true`, `notebooks.controllerContractConfirmed: true` only after verification. Base remains supplied once by local-storage. For static/expansion also copy those overlays' exact RBAC files under distinct site filenames and enable only the corresponding feature after policy/backend review; do not stack bases or duplicate resources.
3. Render/review/apply; preview stop/start, inspect warnings, execute once, poll the durable operation and confirm real Pod disappearance/readiness plus audit attribution. Test expired/stale previews, double submission, non-admin/CSRF denials and crash recovery without automatic replay.
4. Pilot dynamic storage/profile quota/edits separately, then approved NFS/static CSI and expansion only when supported. The implemented expansion action requires a verified bound CSI volume with a driver matching the StorageClass; a plain NFS PV remains unsupported even if its class reports expansion. Verify non-app fields and user data remain intact. No app cleanup/delete workflow exists; operator cleanup needs a separate reviewed data/reclaim plan.
5. Widen namespace scope deliberately only after evidence is recorded. RBAC patch rights are not field-limited; enforce privileged workload/service-account/volume and PV-source restrictions through existing admission policy. Static PV backend uniqueness cannot be enforced atomically by this app (or by a CEL admission rule alone): keep exactly one application writer and a site-owned cluster-wide registration/coordination process covering every privileged writer, as described in [storage](storage.md).

Turning feature flags off stops new UI/API writes but does not revoke RBAC or undo mutations. Removing overlays from a Kustomization and applying does **not** remove existing ClusterRoleBindings; revoke the exact optional bindings separately through the platform operator after reviewing in-flight work. Never use broad prune/delete to downgrade.
