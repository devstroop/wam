# WAM — WhatsApp Marketing server (scaffold)
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /wam ./cmd/wam

FROM gcr.io/distroless/static-debian12:nonroot AS runner
WORKDIR /
COPY --from=builder /wam /wam
# api/openapi.yaml + web assets are embedded via go:embed; no extra COPY needed.
EXPOSE 8080
ENV WAM_ADDR=:8080
USER nonroot:nonroot
ENTRYPOINT ["/wam"]
