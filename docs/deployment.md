# Deployment

All `kubectl` commands below are **operator-run instructions**, not commands executed during implementation. Use a reviewed context explicitly. Do not apply unconfigured manifests.

## 1. Compatibility and outstanding decisions

Record your own cluster's facts before deploying: Kubernetes/RKE2 version and architecture; installed Profile/Notebook controller digests; Dex/OIDC issuer and gateway ingress details; and the StorageClasses actually in use. Do not rely on another site's versions. Keep the Kubernetes client minor aligned with your installed server. A common setup has an existing Kubeflow gateway that serves **HTTP only**; that is not an HTTPS endpoint and does not satisfy the admin app's secure-cookie/OIDC requirements (see section 5).

For **user volumes**, note for each approved class its backend, reclaim policy, binding mode, access modes and whether expansion is actually supported. `allowVolumeExpansion: true` is not proof that the backend, driver, filesystem, quota and workflow support a successful resize — verify each before enabling the capability. Network filesystems are **not suitable for SQLite/WAL**. Existing Bound PVs are in use: never reuse their handles/export directories for a new user, and do not assume separate PVs isolate the same backend data. Keep static templates empty/disabled until reviewed. A CSI import needs a distinct approved prepared volume handle, exact driver and reviewed attributes; NFS needs a distinct prepared export/directory with reviewed ACLs and mount options. Do not infer filesystem ownership from LDAP identity.

Still required: dedicated admin hostname/DNS, public issuer URL and trusted CA, exact admin group or issuer-bound subject, dedicated confidential client secret, ingress workload selector, certificate provisioning, safe DB node/filesystem and backup destination. Run `scripts/discover.sh` yourself if more selected metadata is needed; it never requests Secrets, environment variables or full manifests. Names/images/hosts can still be sensitive.

## 2. Prepare a real site overlay and DB disk

Base uses the default StorageClass; local disks keep the WAL default, while BeeGFS requires `journalMode: delete` plus `allowUnverifiedFilesystem: true` — follow [database requirements](database.md), including the global-lock and remote-fsync verification, before setting either. Use the release image built, published and scanned from this source (section 4), pinned by its fresh registry digest in the site overlay. The historical 0.1.0 image is not this release. The group placeholder still requires configuration. For an optional **node-local** deployment instead:

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

Configure `deploy/site/config.yaml` strictly against `internal/config/config.go`, **not** the older design-plan schema. Set `server.publicURL` to the HTTPS origin (no path), issuer/client, exact admin groups or subjects, approved user classes/images/presets, protected namespaces and preferably a single pilot `allowedNamespaces` entry. `adminSubjects` contains exact `sub` strings and is bound to the configured issuer. Keep every `features` switch false (including `podRestarts`, `nodeCordon` and `profileDeletion`) and `controllerContractConfirmed` false initially. Pod restart additionally needs its separate, namespace-scoped eviction permission and a disposable pilot; see [Pods](pods.md). Node cordon/uncordon needs a separately reviewed `nodes/patch` grant: the supplied overlay covers all Nodes, and Kubernetes RBAC cannot restrict the patched fields (named Nodes can be restricted with `resourceNames`); see [Nodes](nodes.md). Profile deletion needs a separately reviewed `kubeflow.org/profiles` `delete` grant and the `profileDeletion` opt-in; see section 7. Add every site's system namespace to the protected list. Config is read at startup; the generated ConfigMap name changes trigger a restart. Never put credentials in it.

Profile ownership is **immutable** in the installed Profile controller: it refuses an owner change and guards the existing namespace before reconciling quota or role bindings (see the upstream owner-immutability issue). The console therefore edits **quotas only** in `profile.update`. The pre-filled owner is shown read-only, and any request whose owner differs from the current one is refused as an unsupported contract rather than applied as a broken write.

For reviewed all-profile administration, `policy.allowedNamespaces: []` removes
the allowlist restriction; built-in protections (including `kube-*`) and
`policy.protectedNamespaces` still apply. The namespace policy is independent of
RBAC: a namespaced pilot Role cannot authorize Notebook mutations elsewhere.
Use only the reviewed cluster-wide write grant when widening scope, with separate
Profile/storage deletion and storage-editing grants as required. There is no
need for `cluster-admin`, and destructive testing remains disposable-only.

| Capability | Configuration gates (all require writes) | Additional RBAC |
| --- | --- | --- |
| Profile create/quota update, dynamic PVC create | `features.writesEnabled` | `writes` overlay |
| Existing Notebook start/stop/edit | Writes + `notebooks.controllerContractConfirmed` after verification | `writes` overlay |
| Static PV registration/reservation | `features.staticStorage`, approved prepared template | `static-storage` overlay |
| PVC expansion | `features.pvcExpansion`, compatible bound CSI storage | `pvc-expansion` overlay for the standalone expansion action |
| Restricted PV/PVC Edit | `features.storageEditing`; request growth additionally needs `pvcExpansion` | `storage-editing` overlay |
| Unused-only PV/PVC Delete | `features.storageDeletion` | `storage-deletion` overlay |
| Profile Delete | `features.profileDeletion` | `profile-deletion` overlay |
| Pod restart | `features.podRestarts` + verified Notebook contract | `pod-restarts` overlay or pilot-scoped eviction grant |
| Node cordon/uncordon | `features.nodeCordon` | `node-cordon` overlay |

Overlay names above identify permission sets, not bases to stack together. Copy
their reviewed RBAC files into the site overlay and include the base exactly once.
Flags grant no permissions by themselves; grants do not enable flags. Review
[storage limits](storage.md), [Pods](pods.md) and [Nodes](nodes.md) before opt-in.

New Profiles (`profile.create`) require an **email-formatted** owner value, and the check is syntax only — the console performs **no** identity-provider or LDAP registration lookup, so a well-formed address may still be unregistered. Existing Profiles whose owner is a configured username keep working: `profile.update` accepts them **while the owner is left unmodified**, so there is no universal email-format rejection on update. Owner transfer, owner migration and custom owner controllers are a separate future workflow; do not delete or rewrite existing data to satisfy the email rule.

Profile creation refuses an observed existing Profile or namespace and checks
again immediately before creation. Namespace and Profile creation are separate
API calls, not an atomic reservation: coordinate external namespace writers.
A competing namespace can still prevent controller convergence and leave an
operation needing review; the app never rolls it back by deleting resources.

For an already operator-provisioned safe DB PVC, adapt the base PVC to that exact class/binding before apply instead. Existing PVC binding/class fields are generally immutable: do not overwrite a live claim to switch backend. Follow backup/migration recovery procedures.

### 2.1 Optional console footer text

The console footer is deployment configuration, not a shipped sentence. It is **empty by default**: without the key the console renders no footer element at all, so a deployment never shows a sentence it did not ask for.

A site that wants a footer sets exactly one key, `ui.footerText`, in its `config.yaml` — the file the `kubeflow-admin-config` ConfigMap is generated from (see the site overlay above). The value is plain text or a small amount of HTML for branding:

```yaml
ui:
  footerText: |
    <p><strong>Acme Platform</strong> · Changes are audited by the platform team · Support: platform-team@example.org</p>
    <a href="https://support.acme.example">Support portal</a>
    <img src="https://cdn.acme.example/logo.png" alt="Acme logo" width="120" height="40">
```

The value is **sanitized** against a strict allowlist before it is rendered (see `internal/footer`). Unsafe content is removed, not escaped into view:

- **Allowed elements:** `p`, `span`, `div`, `br`, `strong`, `em`, `b`, `i`, `ul`, `ol`, `li`, `a`, `img`.
- **Allowed attributes:** `href` and `title` on `a`; `src`, `alt`, `title`, `width` and `height` on `img`. An `alt`/`title` is truncated to 200 characters; a `width`/`height` is dropped unless it is a plain number of at most 1024 pixels. A company logo needs an `alt`: give the image the text a screen reader should announce.
- **Link destinations:** `http`, `https` or a relative path. Every other scheme (`javascript:`, `data:`, `vbscript:`, `mailto:`, …) is removed with the attribute that carried it.
- **Image sources:** `http`, `https` or a relative path. Scheme-relative sources (`//cdn.example/logo.png`) become HTTPS. Hosts must be valid ASCII DNS names (punycode is supported) or IP literals with a valid optional port; credentials, wildcards and policy metacharacters are refused. A `data:` image is removed.
- **Everything else is removed:** `script`, `style`, `iframe`, `object`, `embed` and `form` elements with their content, event handlers (`onclick`, `onerror`, …), inline CSS, raw SVG, `data:` URLs and every attribute that is not listed above.
- **Links are hardened:** every footer link is rendered as `target="_blank" rel="noopener noreferrer"`, so the opened page gets neither a handle on the console window nor a referrer.

A plain-text value keeps working exactly as before: it is escaped and shown as text. A value that sanitizes to nothing — an empty value, whitespace only, or markup that is entirely removed — renders **no footer element at all**, never an empty placeholder.

The value is **bounded**: at most **16384 characters**, counted in runes (Unicode code points), not bytes. A longer value is refused at startup.

An external image is a **browser request**. The console renders the reference and nothing else: it never fetches the image, so there is no server-side fetch and no server-side request forgery surface. The operator's browser requests the URL the deployment configured, which means that image host sees the request and the administrator's address. Use a company-controlled URL, and treat the image host as a third party that learns when an administrator opens the console. The page's `Content-Security-Policy` names exactly the image hosts the configured footer references — a deployment without a footer keeps the shipped policy, and a deployment with one opens only the host it configured.

The footer carries no capability, no permission and no secret, so it is not a place for credentials.

The configuration is read **once at startup**. Changing the ConfigMap alone does **not** reload the application: the running Pod keeps the previous footer until it is restarted. Re-apply the rendered manifests (the generated ConfigMap name changes with its content) and restart the Pods — the generated ConfigMap name change rolls the Deployment, but a Pod that is not replaced keeps the old value. Wait for the rollout and confirm the new footer in the browser:

```sh
kubectl --context YOUR_CONTEXT apply -f rendered.yaml
kubectl --context YOUR_CONTEXT -n kubeflow-admin rollout restart deployment/kubeflow-admin
kubectl --context YOUR_CONTEXT -n kubeflow-admin rollout status deployment/kubeflow-admin --timeout=180s
```

If the deployment does not roll (for example because the ConfigMap content and the Pod template are unchanged), the explicit `rollout restart` is what applies the new footer.

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

`make image-push` publishes the explicit version tag **and `latest`** from the
same multi-platform build, with provenance and SBOM on both. The helper rejects
untagged, digest-only or `latest`-only input so a version tag is not accidentally
omitted. Use this target for future releases; if publishing with Buildx directly,
include both `-t repository:VERSION` and `-t repository:latest`. Promote `latest`
only with the reviewed current release, not when rebuilding historical versions.
Verify that both registry tags resolve to the recorded release index digest.
Tags are mutable; continue pinning the immutable digest in deployment overlays.

Documentation-only corrections need no new image or deployment and, by operator
policy, no CI run. They can be committed/pushed with `[skip ci]` in the commit
message. Do not use that exemption for source, tests, dependency, configuration,
workflow or build/publishing-script changes. Application releases still require
the full checks/scans and successful source CI before promotion; a later docs-only
receipt commit does not change their binaries or image source label.

Buildx publishes `linux/amd64,linux/arm64` (cluster currently amd64), provenance and SBOM. Use a Buildx `docker-container` builder for multi-platform publication (`docker buildx create --driver docker-container --use` if needed). Docker uses digest-pinned Go 1.27.3 (module minimum remains 1.26), pure-Go SQLite, `CGO_ENABLED=0`, read-only module resolution, trimmed paths, no VCS/build ID; runtime is digest-pinned distroless static nonroot with CA roots and UID/GID 65532. Identical pinned inputs produce reproducible binary inputs; registry attestations/image metadata can differ between builds. Maintain the toolchain/image digests for security updates, scan before promotion, and configure registry pull credentials externally if needed. `make image` builds/loads only the local host architecture. Do not commit rendered site manifests/discovery output.

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
2. Restrict `policy.allowedNamespaces` to the pilot. Copy `deploy/overlays/writes/rbac.yaml` to `deploy/site/writes-rbac.yaml` and add `writes-rbac.yaml` under the site's `resources`. Set `features.writesEnabled: true`, `notebooks.controllerContractConfirmed: true` only after verification. Keep the base supplied exactly once by whichever base or local-storage resource the site uses. For static/expansion also copy those overlays' exact RBAC files under distinct site filenames and enable only the corresponding feature after policy/backend review; do not stack bases or duplicate resources.
3. Render/review/apply; preview stop/start, inspect warnings, execute once, poll the durable operation and confirm real Pod disappearance/readiness plus audit attribution. Test expired/stale previews, double submission, non-admin/CSRF denials and crash recovery without automatic replay.
4. Pilot dynamic storage/profile quota/edits separately, then approved NFS/static CSI and expansion only when supported. The implemented expansion action requires a verified bound CSI volume with a driver matching the StorageClass; a plain NFS PV remains unsupported even if its class reports expansion. Verify non-app fields and user data remain intact. There is no arbitrary cleanup workflow: separately gated `profile.delete` deletes a Profile and cascades to its namespace (section 7); separately gated storage deletion is unused-only and never forces cleanup. Other workflows can also irreversibly lose volatile state. Review the [storage editing/deletion permissions and caveats](storage.md#restricted-yaml-editing-and-unused-only-deletion) before enabling them. Backend cleanup always needs a separate reviewed data/reclaim plan.
5. Widen namespace scope deliberately only after evidence is recorded. RBAC patch rights are not field-limited; enforce privileged workload/service-account/volume and PV-source restrictions through existing admission policy. Static PV backend uniqueness cannot be enforced atomically by this app (or by a CEL admission rule alone): keep exactly one application writer and a site-owned cluster-wide registration/coordination process covering every privileged writer, as described in [storage](storage.md).

Turning feature flags off stops new UI/API writes but does not revoke RBAC or undo mutations. Removing overlays from a Kustomization and applying does **not** remove existing ClusterRoleBindings; revoke the exact optional bindings separately through the platform operator after reviewing in-flight work. Never use broad prune/delete to downgrade.

## 7. Profile deletion — separate opt-in, irreversible

`profile.delete` is the only action that deletes a Profile and cascades to its namespace. Other workflows can also irreversibly lose volatile state. Profile deletion is deliberately narrow: one typed action, one directly deleted resource (the `kubeflow.org` Profile), one permission (`delete`), and two explicit gates. There is no HTTP `DELETE` endpoint, no namespace-delete action and no generic cleanup route. Separate unused-only `pv.delete`/`pvc.delete` actions require their own storage-deletion opt-in and reviewed RBAC; see [storage management](storage.md#restricted-yaml-editing-and-unused-only-deletion).

### 7.1 The two gates

| Gate | Where | Effect when off |
| --- | --- | --- |
| `features.writesEnabled: true` | `deploy/site/config.yaml` | Every write is refused; the deletion control is not offered |
| `features.profileDeletion: true` | `deploy/site/config.yaml` | `profile.delete` is refused by the API with a policy error before any plan is created |

Both must be true. The flag is **not** implied by `writesEnabled`, and the shipped base config keeps `profileDeletion: false`. The console also projects the capability to the browser only when both are enabled, so the browser never offers a capability the API would refuse.

### 7.2 Installing the permission without stacking bases

`deploy/overlays/profile-deletion/` grants exactly one verb on one resource:

```yaml
  - apiGroups: [kubeflow.org]
    resources: [profiles]
    verbs: [delete]
```

It grants **no** `namespaces`, `persistentvolumes`, `persistentvolumeclaims` or `pods` delete, and **no** finalizer update or patch. The read-only base is unchanged, and `deploy/overlays/all-features/` does **not** include this overlay — the delete permission is never granted by a broad feature bundle.

Because the base must appear exactly once in a rendered site overlay, copy the RBAC file into the site directory instead of stacking overlays:

```sh
cp deploy/overlays/profile-deletion/rbac.yaml deploy/site/profile-deletion-rbac.yaml
```

Then add `profile-deletion-rbac.yaml` under the site's `resources` **once**, next to the other copied RBAC files, and do not list `deploy/overlays/profile-deletion/` itself (it would add the base a second time). The same rule as the other overlays: the base is supplied once, and no resource is duplicated.

### 7.3 Restricting the grant to a disposable pilot

A Profile is a **cluster-scoped** object, so a namespaced `Role` cannot grant access to it and a `RoleBinding` grants nothing for it. The way to narrow the grant is `resourceNames` on the **ClusterRole**, bound by a ClusterRoleBinding:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeflow-admin-profile-deletion
rules:
  - apiGroups: [kubeflow.org]
    resources: [profiles]
    resourceNames: [REPLACE_WITH_DISPOSABLE_PILOT_PROFILE]
    verbs: [delete]
```

`resourceNames` **does** restrict which Profiles may be deleted: a delete request for any other name is denied by the API server. Edit the copied `deploy/site/profile-deletion-rbac.yaml` this way for the pilot, and keep the unrestricted list only for a reviewed, fully disposable scope.

What `resourceNames` does **not** do is bound the work that follows the deletion. It restricts no field, and it does not stop the namespace cascade that the Profile deletion triggers. Bound those with cluster-scoped namespace protection and existing admission policy, not with RBAC.

### 7.4 Verify the installed controller and ownership first

Before enabling the opt-in, independently confirm with your own tooling:

1. The installed Profile controller owns the namespace it created for a Profile (owner reference plus the `owner` annotation) and performs the normal cleanup when the Profile is deleted.
2. Kubernetes garbage collection removes the Profile's owned namespace and the namespaced objects inside it.
3. The pilot Profile is disposable, its namespace contains no valuable data, and the owner has been told.

A deletion that the controller does not clean up leaves a namespace in `Terminating` with no automated recovery path.

### 7.5 What the deletion actually does — and does not do

**Namespace ownership is a deployment prerequisite, not an atomic transaction.**
Before enabling deletion, restrict namespace creation/updates and Profile owner
reference assignment to the reviewed Profile controller and coordinated
platform operators. Admission must prevent a Profile from owning another
namespace, prevent cross-namespace Profile owner references on resources, and
prevent replacement/reparenting during deletion (including stale controller
reconciliation). Do not recreate a Profile under the same name during cleanup.
Use an enforced maintenance window if other privileged writers cannot be fenced.

The app checks the namespace UID/ownership and refuses additional namespaces
owned by the Profile, but Kubernetes has no multi-object transaction tying those
reads to the Profile DELETE. App database locks do **not** constrain `kubectl`,
GitOps, other controllers or privileged users. Keep `profileDeletion: false`
until these controls and the installed controller have been reviewed; UID
preconditions on the Profile alone cannot guarantee the namespace blast radius.

- It requests deletion of **one** Profile, identified by an exact name, with a **nonempty reason** and a `confirmName` that must equal the target Profile name **exactly** (raw comparison, no trimming or case folding).
- The delete uses **background propagation**, matching `kubectl delete` semantics: the API server accepts the request and the objects disappear asynchronously.
- The installed Profile controller and Kubernetes garbage collection perform the normal cleanup of the owned namespace and its namespaced objects. The console has **no** namespace-delete permission and issues **no** Namespace `DELETE`.
- The Profile deletion action never directly deletes a PV/PVC. Namespace cleanup deletes PVCs and applies each bound PV's reclaim policy: **Delete can irreversibly destroy the backing volume and data**, while Retain-policy PVs and external data may remain. The separate storage-deletion workflow is unused-only and does not bypass finalizers. Retain does not protect against backend lifecycle or later operator deletion. Namespaced VolumeSnapshots also disappear; underlying snapshot content follows its own deletion policy. This is not a verified data wipe.
- The action succeeds only when the Profile **and** its owned namespace are both observed **gone**. A replacement object with the same name, or a read error, is **not** gone.
- If the namespace stays `Terminating` (a finalizer that never clears), the operation becomes `needs-review`. There is no force, no finalizer removal and no retry loop; reconcile it as an operator.
- While deletion is unresolved, an exclusive namespace fence blocks other app mutations there. After successful deletion the fence is released, but missing Profile/namespace scope still refuses child mutations. A later recreated workspace requires fresh observations and plans.

Because the operation is irreversible and the app has no undo, test it only on a disposable pilot and keep a verified backup beforehand.
