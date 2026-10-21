# syntax=docker/dockerfile:1.7
# Refresh this supported Go patch AND digest together after security review.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:5c3e9b43cfd3bb95c7937462e29d87ada57b481a96fa23ad0eb61778d7a00110 AS build
ARG TARGETOS=linux
ARG TARGETARCH
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags='-s -w -buildid=' -o /out/kubeflow-admin ./cmd/kubeflow-admin

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kubeflow-admin /kubeflow-admin
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/kubeflow-admin"]
CMD ["-config", "/etc/kubeflow-admin/config.yaml"]
