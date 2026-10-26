# kontra-gate, the relay gate (spec/relay.md). A static binary on distroless, running as nonroot (65532).
#
#   docker build --build-arg VERSION=v4.0.0-m1 -t kontra-gate:v4.0.0-m1 .
#   docker run --rm kontra-gate:v4.0.0-m1 version
#
# The image holds the binary only: kontra.yaml is mounted (charts/kontra mounts it at /etc/kontra), the
# migrations are embedded (kontra-gate migrate), and the ACME directory cache, when built-in ACME is on, is a
# volume (charts/kontra mounts one at /var/lib/kontra-gate). The root filesystem can be read-only.

# The module's toolchain (go.mod: toolchain go1.26.10). Pinned by version; a digest is not required.
ARG GO_VERSION=1.26.10

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
# The image's own Go, never a downloaded one; no cgo, so the binary runs on distroless/static.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Written to gate_replicas.version and the tunnel hello (internal/version). Without it the binary says "dev".
ARG VERSION=dev
RUN go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X kontra.sh/kontra/internal/version.Version=${VERSION}" \
      -o /out/kontra-gate ./cmd/kontra-gate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kontra-gate /usr/local/bin/kontra-gate
USER nonroot:nonroot
# The public listener (listen:, 8443 behind a TCP passthrough) and the metrics listener (metrics.listen).
EXPOSE 8443 9090
ENTRYPOINT ["/usr/local/bin/kontra-gate"]
