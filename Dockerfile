# Operator (manager) image — multi-stage, distroless static nonroot, multi-arch.
# Builds cmd/manager -> /manager. Only the manager ships in percona/valkey-operator;
# the sidecars (cmd/valkey-backup, cmd/healthcheck, cmd/peer-list) ship in the DB
# image via Dockerfile.sidecar. (docs/architecture/02-repo-layout.md §6, 10 §2)

# --- builder ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /workspace

# Cache modules first.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Source.
COPY cmd/ cmd/
COPY pkg/ pkg/

# Static, stripped, cross-compiled build of the manager.
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X valkey.percona.com/percona-valkey-operator/pkg/version.gitVersion=${VERSION}" \
      -o /manager ./cmd/manager

# --- runtime ---------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
