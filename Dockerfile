# WAM — WhatsApp Marketing server
# syntax=docker/dockerfile:1.6
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 because modernc.org/sqlite is pure Go (no cgo).
# Respect TARGETOS/TARGETARCH so `docker buildx build --platform linux/arm64`
# on x86_64 (or native on aarch64) produces the correct binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /wam ./cmd/wam
# prepare an empty data dir to copy with correct ownership (see runner stage)
RUN mkdir -p /tmp/data && chmod 755 /tmp/data

FROM gcr.io/distroless/static-debian12:nonroot AS runner
# gcr.io/distroless/static-debian12:nonroot is multi-arch (amd64/arm64/arm/v7/ppc64le/s390x)
# and is the correct minimal base for a CGO_ENABLED=0 static binary.
WORKDIR /
COPY --from=builder /wam /wam
# Create /data in-image as nonroot-owned 755 so that a *new* named volume
# inherits correct ownership on first mount. Existing volumes that are
# root-owned still need a one-time chown (compose init does it — see
# docker-compose.yml). Without this, nonroot cannot MkdirAll(/data) and
# the container panics  with  "mkdir /data: permission denied"  or
# "unable to open database file" on aarch64 (and any host where the
# volume was first created as root).
COPY --from=builder --chown=nonroot:nonroot --chmod=755 /tmp/data /data
# api/openapi.yaml + web assets are embedded via go:embed; no extra COPY needed.
EXPOSE 8080
ENV WAM_ADDR=:8080
USER nonroot:nonroot
ENTRYPOINT ["/wam"]
