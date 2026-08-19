# Immich Drop – Backend Documentation

This folder documents the original Python/FastAPI backend (`app/app.py`, `app/config.py`,
`main.py` — since removed from the repo). It served as the **specification for the Go
rewrite**, which now lives in [`../go-backend/`](../go-backend/) and implements the HTTP +
WebSocket contract described here. The frontend (static files in `frontend/`) is shared,
so this contract remains the reference.

| Document | Contents |
| --- | --- |
| [architecture.md](architecture.md) | System overview, components, request flows, configuration, security model |
| [openapi.yaml](openapi.yaml) | OpenAPI 3.0 specification of the HTTP API between frontend and backend |
| [websocket.md](websocket.md) | The `/ws` progress protocol (not expressible in OpenAPI) |
| [database.md](database.md) | SQLite schema, migrations, and access patterns |
| [rewrite-notes.md](rewrite-notes.md) | Quirks, known bugs, and decisions the Go rewrite must make |

## What this application does

Immich Drop is a small self-hosted "drop box" for an [Immich](https://immich.app) photo server.
It lets anyone with access to the page (or an invite link) upload photos/videos **without an Immich
account**. The backend:

1. Serves the static frontend (upload page, login, invite management menu, invite landing page).
2. Accepts uploads (whole-file or chunked), deduplicates them (local SQLite cache + Immich
   bulk-check), and forwards them to the Immich API using either a server-side API key or a
   logged-in user's bearer token.
3. Pushes per-file upload progress to the browser over a WebSocket.
4. Manages **invite links**: tokens with optional album target, max-use count, expiry,
   password protection, and enable/disable, owned by a logged-in Immich user.
5. Optionally sorts uploads into an Immich album (a global default from `.env`, or the album an
   invite points at).

## Runtime shape

- Single process, single service. Entry point: `python main.py` → uvicorn serving `app.app:app`
  on `HOST:PORT` (default `0.0.0.0:8080`).
- State: one SQLite file (`STATE_DB`, default `/data/state.db`) plus a temp directory for chunked
  uploads (`/data/chunks`, hardcoded).
- All configuration comes from environment variables / `.env` at startup; there is no runtime
  settings mutation (see [architecture.md](architecture.md#configuration)).
- Typically deployed via Docker (`Dockerfile`, `docker-compose.yml`) behind a reverse proxy.
