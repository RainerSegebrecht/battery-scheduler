# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy module files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary (no CGO needed — modernc.org/sqlite is pure Go)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o battery-scheduler ./cmd/battery-scheduler

# ──────────────────────────────────────────────────────────────────────────────
# Runtime stage — minimal image
FROM alpine:3.19

RUN apk add --no-cache tzdata ca-certificates

WORKDIR /app
COPY --from=builder /build/battery-scheduler /app/battery-scheduler

# /config  → mount your config.yaml here (read-only)
# /data    → mount persistent volume for SQLite DB here
VOLUME ["/config", "/data"]

ENTRYPOINT ["/app/battery-scheduler", "-config", "/config/config.yaml"]
