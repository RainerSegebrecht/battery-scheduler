# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /build

# GOTOOLCHAIN=auto allows Go to download the toolchain version required by
# go.mod if the bundled version is too old. This is needed because go.mod
# may require a newer Go than the base image ships.
ENV GOTOOLCHAIN=auto

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

# Web status UI
EXPOSE 8080

ENTRYPOINT ["/app/battery-scheduler", "-config", "/config/config.yaml"]
