# syntax=docker/dockerfile:1

# Build on the native platform and cross-compile for the target (no QEMU).
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/pod-reaper .

# Minimal runtime: static binary, CA certs, tzdata, non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/pod-reaper /pod-reaper
USER nonroot:nonroot
ENTRYPOINT ["/pod-reaper"]
