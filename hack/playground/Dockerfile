# Multi-stage build for the playground binaries. Parameterized by CMD so a
# single Dockerfile produces both the playground controller-manager and the
# capability-provider adapter:
#
#   docker build -f hack/playground/Dockerfile --build-arg CMD=playground-manager  -t ghcr.io/milo-os/service-catalog-playground-manager:dev .
#   docker build -f hack/playground/Dockerfile --build-arg CMD=capability-provider -t ghcr.io/milo-os/capability-provider:dev .
#
# Build context is the repo root. Mirrors the repo's top-level Dockerfile
# (static CGO_ENABLED=0 binary, distroless nonroot base).
FROM --platform=$BUILDPLATFORM golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG CMD=playground-manager

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

ENV GOCACHE=/root/.cache/go-build
ENV GOTMPDIR=/root/.cache/go-build
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static:nonroot
ARG CMD=playground-manager
WORKDIR /
COPY --from=builder /out/app /${CMD}
USER 65532:65532
# ENTRYPOINT is set per-image by the Deployment's `command:` so the same layer
# layout serves both binaries; the copied binary is named /${CMD}.
