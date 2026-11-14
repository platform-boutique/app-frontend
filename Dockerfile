# ---- Build stage ----
FROM golang:1.26-alpine AS builder

# Build identity, linked into internal/obs (read by the service at start)
ARG SERVICE_VERSION=dev
ARG SERVICE_COMMIT=unknown

ENV CGO_ENABLED=0
ENV GOOS=linux

RUN apk add --no-cache git

WORKDIR /src

# Download dependencies first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN go build \
    -ldflags="-s -w -X frontend/internal/obs.Version=${SERVICE_VERSION} -X frontend/internal/obs.Commit=${SERVICE_COMMIT}" \
    -o /bin/frontend ./cmd/server

# ---- Runtime stage ----
FROM alpine:3.24 AS runtime

ARG SERVICE_VERSION=dev
ARG SERVICE_COMMIT=unknown

LABEL org.opencontainers.image.title="frontend" \
      org.opencontainers.image.source="https://github.com/platform-boutique/app-frontend" \
      org.opencontainers.image.revision="${SERVICE_COMMIT}" \
      org.opencontainers.image.version="${SERVICE_VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 appgroup && adduser -S -u 10001 -G appgroup appuser

ENV SERVICE_VERSION=${SERVICE_VERSION} \
    SERVICE_COMMIT=${SERVICE_COMMIT}

WORKDIR /app

COPY --from=builder /bin/frontend .
COPY --chown=appuser:appgroup templates/ ./templates/
COPY --chown=appuser:appgroup static/ ./static/

# Numeric so Kubernetes runAsNonRoot can verify it (matches the charts' runAsUser).
USER 10001:10001

EXPOSE 8080

# Exec form, no shell and no wget: `frontend healthcheck` does GET /healthz on its own port (PORT, default 8080)
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/app/frontend", "healthcheck"]

ENTRYPOINT ["./frontend"]