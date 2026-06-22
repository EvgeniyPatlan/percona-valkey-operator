# Operator (manager) image — multi-stage, distroless static nonroot, multi-arch.
#
# Builds cmd/manager -> /manager. Only the manager ships in percona/valkey-operator;
# the sidecars (cmd/valkey-backup, cmd/healthcheck, cmd/peer-list) ship in the DB
# image via Dockerfile.sidecar. (docs/architecture/02-repo-layout.md §6, 10 §2)
#
# Multi-arch: built with `docker buildx --platform linux/amd64,linux/arm64` (the
# default GA matrix; arch 10 §2.1). s390x/ppc64le are opt-in via PLATFORMS, NOT
# default. CGO is off so the cross-compile is pure-Go and the runtime can be the
# static distroless base. BUILDPLATFORM pins the builder to the native arch and
# Go cross-compiles to TARGETOS/TARGETARCH (no QEMU in the compile step).

# --- builder ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
# Version stamping (arch 10 GO-7.2). VERSION is the operator semver; GIT_COMMIT
# and BUILD_TIME enrich the startup log. The -X targets for GitCommit/BuildTime
# are silent no-ops until GO-7.2 adds pkg/version/build.go, so an un-stamped
# build degrades gracefully (matches the Makefile VERSION_LDFLAGS contract).
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /workspace

# Cache modules first (these layers only bust when go.mod/go.sum change).
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Source.
COPY cmd/ cmd/
COPY pkg/ pkg/

# Static, stripped, cross-compiled, reproducible build of the manager.
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X valkey.percona.com/percona-valkey-operator/pkg/version.gitVersion=${VERSION} \
        -X valkey.percona.com/percona-valkey-operator/pkg/version.GitCommit=${GIT_COMMIT} \
        -X valkey.percona.com/percona-valkey-operator/pkg/version.BuildTime=${BUILD_TIME}" \
      -o /manager ./cmd/manager

# --- runtime ---------------------------------------------------------------
# Distroless static nonroot: no shell, no package manager, runs as uid 65532.
# Pinned by digest-friendly tag; bump deliberately (Trivy/Grype gate in scan.yml).
FROM gcr.io/distroless/static:nonroot

# OCI image metadata (arch 10 §2). ${VERSION} is re-declared because ARGs do not
# cross FROM boundaries.
ARG VERSION=dev
LABEL org.opencontainers.image.title="percona-valkey-operator" \
      org.opencontainers.image.description="Percona Operator for Valkey — the manager binary." \
      org.opencontainers.image.vendor="Percona" \
      org.opencontainers.image.source="https://github.com/percona/percona-valkey-operator" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

WORKDIR /
COPY --from=builder /manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
