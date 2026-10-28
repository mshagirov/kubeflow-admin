# Verification and release checks

Verify the exact source revision and image you intend to deploy. [Historical publication receipts](verification-history.md) retain commit IDs, image/platform digests, scan results and past test evidence; they do not certify later changes or current mutable tags.

## Local checks

Install the prerequisites in the [README](../README.md#local-development), activate the Python environment, then run:

```sh
make check build
openapi-spec-validator api/openapi.yaml
bash -n scripts/discover.sh
```

`make check` runs Go race tests, vet, API parity, Python tests and manifest invariants. Node.js must be available for the Go-invoked frontend tests; otherwise those tests skip. CI additionally validates rendered Kubernetes resources with kubeconform and builds both runtime architectures; see [the workflow](../.github/workflows/ci.yaml) for pinned tools and exact commands.

For real-browser checks against the local synthetic fixture:

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
CHROME_BIN=/path/to/chrome \
node internal/web/testdata/profile_browser_optional.cjs
```

The runner builds its fixture automatically unless `DASHBOARD_BASE_URL` supplies one. Missing Playwright/Chrome produces a skip, not a pass. These checks exercise browser behavior without proving cluster convergence.

## Release evidence

1. Record the clean source commit/tag, configuration compatibility and successful source CI.
2. Run vulnerability, secret and manifest scans (`make security` requires govulncheck and Trivy). Use the pinned CI tool versions for release evidence.
3. Build amd64/arm64 artifacts from the exact archived commit with version/revision labels, SBOM and provenance. Scan each runtime architecture and verify nonroot identity, LICENSE, labels and startup behavior.
4. Publish the verified artifact without rebuilding; record the registry index, platform and attestation digests. Verify release and `latest` tags resolve to the intended index. Pin the immutable digest in the site overlay.
5. Record the deployed digest and perform the applicable live checks below. A successful build or published image is not a rollout.

`make image-push` is a direct build-and-push helper, not offline artifact promotion. See [deployment](deployment.md#4-verify-publish-and-render).

## Live acceptance

Use [the workflow checklist](ui-workflow-acceptance.md) on disposable resources. Verify HTTPS/OIDC claims and callbacks, non-admin/CSRF denial, CNI/admission enforcement, controller ownership/culling, scheduling/GPU quotas, storage isolation/binding/mounts/resize and database locking/crash recovery. Rehearse [backup restoration](backup.md).

Keep a private record of source/image/config identity, environment/controller versions, tests performed, results, operation/object IDs, unresolved issues, reviewer and date. Never present fixture results as live acceptance or expose credentials, full manifests, backend paths or session data in public evidence.
