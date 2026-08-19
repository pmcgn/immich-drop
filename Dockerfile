# syntax=docker/dockerfile:1.7

# ---- Stage 1: build the Go backend ----
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so they cache independently of source changes.
COPY go/go.mod go/go.sum ./
RUN go mod download

COPY go/ ./
# Pure Go (modernc.org/sqlite, no cgo) -> a fully static binary that runs on
# distroless/static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/immich-drop .

# Distroless has no shell, so the writable data directory (SQLite state.db +
# chunk spool) must be prepared here and copied in with the right owner.
RUN mkdir -p /out/data

# ---- Stage 2: minimal runtime image ----
# distroless/static: no shell, no package manager, no libc; ships CA certs and
# tzdata. The :nonroot tag runs as uid 65532 by default.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/immich-drop /app/immich-drop
COPY frontend/ /app/frontend/
# 65532 = nonroot; numeric so the chown doesn't depend on /etc/passwd lookups.
COPY --from=build --chown=65532:65532 /out/data /data

# Persist dedupe cache (state.db) and in-flight chunk spools across restarts.
VOLUME ["/data"]

ENV HOST=0.0.0.0 \
    PORT=8080 \
    STATE_DB=/data/state.db \
    CHUNK_DIR=/data/chunks \
    FRONTEND_DIR=/app/frontend

# Public upload port. When split-port mode is enabled (optional ADMIN_PORT env,
# e.g. ADMIN_PORT=8081), publish that port as well.
EXPOSE 8080

# The binary probes its own GET /healthz (exec form: distroless has no shell).
# It reads the same env as the server, so with ADMIN_PORT set it automatically
# probes the admin port, where /healthz lives in split-port mode.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/immich-drop", "-healthcheck"]

ENTRYPOINT ["/app/immich-drop"]
