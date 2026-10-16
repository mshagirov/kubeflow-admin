GO ?= go
KUSTOMIZE ?= kustomize
IMAGE ?= kubeflow-admin:local
PLATFORMS ?= linux/amd64,linux/arm64
OVERLAY ?= deploy/base

.PHONY: test vet build render manifests api-parity check image image-push security
test:
	$(GO) test -mod=readonly -race ./...
vet:
	$(GO) vet -mod=readonly ./...
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o bin/kubeflow-admin ./cmd/kubeflow-admin
render:
	$(KUSTOMIZE) build $(OVERLAY)
api-parity:
	python3 scripts/check-api-parity.py
	python3 -m unittest discover -s scripts -p 'test_*.py'
manifests: api-parity
	KUSTOMIZE=$(KUSTOMIZE) python3 scripts/check-manifests.py
check: test vet manifests
image:
	docker buildx build --platform linux/$(shell $(GO) env GOARCH) --load -t $(IMAGE) .
image-push:
	docker buildx build --platform $(PLATFORMS) --provenance=true --sbom=true --push -t $(IMAGE) .
security:
	govulncheck ./...
	trivy fs --scanners vuln,secret,misconfig --severity HIGH,CRITICAL --exit-code 1 .
