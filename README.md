# Immich Drop Uploader

> [!NOTE]
> **Fork of [nasogaa/immich-drop](https://github.com/nasogaa/immich-drop).** The Python
> backend has been rewritten in Go and hardened for internet-facing use: uploads and the
> WebSocket now require a valid invite, admin pages can run on a separate port, and large
> files are chunked to disk with end-to-end checksum verification.
> See [About this fork](#about-this-fork) for the full list.

A tiny web app for collecting photos/videos into your **Immich** server.
Admin users log in to create public invite links; invite links are always public-by-URL. A public uploader page is optional and disabled by default.

![Immich Drop Uploader Dark Mode UI](./screenshot.png)

---

## About this fork

This is a fork of [nasogaa/immich-drop](https://github.com/nasogaa/immich-drop). The
upstream project is a Python/FastAPI app; this fork **replaces the backend with a Go
implementation** and hardens it for running on the public internet. The frontend, the
HTTP/WebSocket API and the on-disk SQLite schema stayed compatible during the rewrite
(see [`docs/rewrite-notes.md`](docs/rewrite-notes.md)), so an existing `state.db` keeps working.

Images are published from this fork as `ghcr.io/pmcgn/immich-drop`.

### Go backend instead of Python

- Complete rewrite of Backend in Go 
- Pure-Go SQLite (`modernc.org/sqlite`, no cgo) → a single static binary, no Python
  runtime, no interpreter dependencies.
- Structured logging (`slog`) with `LOG_LEVEL`, and the build version stamped into the
  startup log.

### Security hardening

- **Uploads require auth.** With `PUBLIC_UPLOAD_PAGE_ENABLED=false`, every upload and
  chunk request must carry a valid, active invite token or come from a logged-in
  session. Requests are rejected *before* anything is written to disk or forwarded to
  Immich.
- **WebSocket is authenticated too.** The `/ws` registration frame carries the invite
  token and is subject to the same rule; unauthorized sockets are closed (code 1008).
  The first frame must arrive within 10 s, and per-session socket limits prevent a
  client from growing the hub unboundedly.
- **Strict input validation** on invite tokens, session/item ids, chunk indices and
  album ids, plus WebSocket origin checking.
- **Admin/public port split.** Optional `ADMIN_PORT` serves login, menu and invite
  management on a separate port, so only the upload port needs to be exposed to the
  internet.
- **Information leaks removed.** The ping banner used to reveal the Immich base URL and
  the default album name; it is gone.
- Session cookies are automatically marked `Secure` when `PUBLIC_BASE_URL` uses `https://`.
- **Distroless Base Container**. Reduction of attack surface. No packet manager or tools are part of the container.

### Uploads & reliability

- **Chunked uploads spool to disk** (`CHUNK_DIR`, default `/data/chunks`) instead of
  RAM, so files larger than the server's memory can be uploaded.
- **End-to-end checksum verification**: the hash is compared before and after the
  transfer to catch corruption introduced by chunking.
- Upload pipeline no longer blocks indefinitely on stalled TCP connections.
- **Album fix**: files detected as duplicates are still added to the album the invite
  points at, instead of being silently dropped; album failures are now logged with the
  album/asset id and the Immich response.

### Container & release

- **Multi-stage Dockerfile** producing a distroless, non-root image with a built-in
  `HEALTHCHECK` (the binary probes its own `/healthz`, and knows about split-port mode).
- **Multi-arch releases** (amd64 + arm64) via a GitHub Actions workflow triggered by
  `v*.*.*` tags, publishing to GHCR with semver tags.
- **`GET /healthz` health endpoint** returning `{"ok": true}` — usable as a Kubernetes
  liveness/readiness probe (or for any other orchestrator / load balancer). It needs no
  authentication and does not touch Immich, so it stays green while Immich is down. In
  split-port mode it is served on `ADMIN_PORT`, so point the probe at the admin port.


## Features

- **Invite Links:** public-by-URL links for uploads; one-time or multi-use
- **Manage Links:** search/sort, enable/disable, delete, edit name/expiry
- **Row Actions:** icon-only actions with tooltips (Open, Copy, Details, QR, Save)
- **Passwords (optional):** protect invites with a password gate
- **Albums (optional):** upload into a specific album (auto-create supported)
- **Duplicate Prevention:** local SHA‑1 cache (+ optional Immich bulk-check)
- **Progress Queue:** WebSocket updates; retry failed items
- **Chunked Uploads (optional):** large-file support with configurable chunk size
- **Privacy-first:** never lists server media; session-local uploads only
- **Mobile + Dark Mode:** responsive UI, safe-area padding, persistent theme

---

## Table of contents
- [Quick start](#quick-start)
- [New Features](#new-features)
- [Chunked Uploads](#chunked-uploads)
- [Architecture](#architecture)
- [Folder structure](#folder-structure)
- [Requirements](#requirements)
- [Configuration (.env)](#configuration-env)
- [How it works](#how-it-works)
- [Mobile notes](#mobile-notes)
- [Troubleshooting](#troubleshooting)
- [Security notes](#security-notes)
- [Development](#development)
- [License](#license)

---
## Quick start
You can run without a `.env` file by putting all settings in `docker-compose.yml` (recommended for deploys).
Use a `.env` file only for local development.

### docker-compose.yml (deploy without .env)
```yaml
version: "3.9"

services:
  immich-drop:
    image: ghcr.io/nasogaa/immich-drop:latest
    pull_policy: always
    container_name: immich-drop
    restart: unless-stopped

    # Configure all settings here (no .env required)
    environment:

      # Immich connection (must include /api)
      IMMICH_BASE_URL: https://immich.example.com/api
      IMMICH_API_KEY: ${IMMICH_API_KEY}

      # Optional behavior
      IMMICH_ALBUM_NAME: dead-drop
      PUBLIC_UPLOAD_PAGE_ENABLED: "false"   # keep disabled by default
      PUBLIC_BASE_URL: https://drop.example.com

      # Large files: chunked uploads (bypass 100MB proxy limits)
      CHUNKED_UPLOADS_ENABLED: "false"      # enable chunked uploads
      CHUNK_SIZE_MB: "95"                  # per-chunk size (MB)

      # App internals
      SESSION_SECRET: ${SESSION_SECRET}

      # Ports: PORT serves the public upload endpoints, the optional
      # ADMIN_PORT serves login/menu/invite management. Remove ADMIN_PORT
      # to serve everything on PORT.
      PORT: 8080
      ADMIN_PORT: 8081

    # Expose the app on the host
    ports:
      - 8080:8080   # same as PORT
      - 8081:8081   # same as ADMIN_PORT. Remove if not used

    # Persist local dedupe cache (state.db) across restarts
    volumes:
      - immich_drop_data:/data

    # No healthcheck block needed: the image ships its own HEALTHCHECK
    # (the binary probes its /healthz endpoint).

volumes:
  immich_drop_data:
```

```
### CLI
```bash
docker compose pull
docker compose up -d
```

---

## Chunked Uploads

- Enable chunked uploads by setting `CHUNKED_UPLOADS_ENABLED=true`.
- Configure chunk size with `CHUNK_SIZE_MB` (default: `95`). The client only uses chunked mode for files larger than this.
- Intended to bypass upstream limits (e.g., 100MB) while preserving duplicate checks, EXIF timestamps, album add, and per‑item progress via WebSocket.

---

## Architecture

- **Frontend:** static HTML/JS (Tailwind). Drag & drop or "Choose files", queue UI with progress and status chips.  
- **Backend:** Go (see [`go-backend/README.md`](go-backend/README.md)).  
  - Proxies uploads to Immich `/assets`  
  - Computes SHA‑1 and checks a local SQLite cache (`state.db`)  
  - Optional Immich de‑dupe via `/assets/bulk-upload-check`  
  - WebSocket `/ws` pushes per‑item progress to the current browser session only  
  - Optional split-port mode (`ADMIN_PORT`) to keep the admin UI off the public port  
- **Persistence:** local SQLite (`state.db`) prevents re‑uploads across sessions/runs.

---

## Folder structure

```
immich_drop/
├─ go-backend/              # Go backend (see go-backend/README.md)
│  ├─ main.go               # Entrypoint
│  └─ internal/             # config, server, store, immich client, ...
├─ frontend/                # Static UI (served at /static)
│  ├─ index.html            # Public uploader (optional)
│  ├─ login.html            # Login page (admin)
│  ├─ menu.html             # Admin menu (create invites)
│  ├─ invite.html           # Public invite upload page
│  ├─ app.js                # Uploader logic (drop/queue/upload/ws)
│  ├─ header.js             # Shared header (theme + ping + banner)
│  └─ favicon.png           # Tab icon (optional)
├─ docs/                    # API/behavior specification (openapi.yaml, ...)
├─ data/                    # Local dev data dir (bind to /data in Docker)
├─ Dockerfile               # Multi-stage build -> distroless image
├─ docker-compose.yml
├─ .env.example             # Example dev environment (optional)
├─ README.md
└─ screenshot.png           # UI screenshot for README
```

---

## Requirements

- **Go** 1.24+ (or just Docker)
- An **Immich** server + **API key**

---
# Local dev quickstart

## Development

```bash
cd go-backend
go build -o immich-drop .
./immich-drop            # reads .env / environment
```

See [`go-backend/README.md`](go-backend/README.md) for backend details.

---

## Dev Configuration (.env)

```ini
# Server (dev only)
HOST=0.0.0.0
PORT=8080

# Optional: split-port mode. When set, admin endpoints
# (login, menu, invite management) are served on this port and PORT serves only
# the public upload endpoints — useful to expose only the upload port publicly.
# Leave unset (default) to serve everything on PORT.
# Set PUBLIC_BASE_URL when enabling this, so invite links point at the upload port.
#ADMIN_PORT=8081

# Immich connection (include /api)
IMMICH_BASE_URL=http://REPLACE_ME:2283/api
IMMICH_API_KEY=ADD-YOUR-API-KEY   # needs: asset.upload; for albums also: album.create, album.read, albumAsset.create
MAX_CONCURRENT=3

# Public uploader page (optional) — disabled by default
PUBLIC_UPLOAD_PAGE_ENABLED=TRUE

# Album (optional): auto-add uploads from public uploader to this album (creates if needed)
IMMICH_ALBUM_NAME=dead-drop

# Local dedupe cache (SQLite)
STATE_DB=./data/state.db

# Base URL for generating absolute invite links (recommended for production)
# e.g., PUBLIC_BASE_URL=https://photos.example.com
#PUBLIC_BASE_URL=

# Session and security
SESSION_SECRET=SET-A-STRONG-RANDOM-VALUE
LOG_LEVEL=DEBUG

# Chunked uploads (optional)
CHUNKED_UPLOADS_ENABLED=true
CHUNK_SIZE_MB=95

```


You can keep a checked‑in `/.env.example` with the keys above for onboarding.

---

## How it works

1. **Queue** – Files selected in the browser are queued; each gets a client‑side ID.  
2. **De‑dupe (local)** – Server computes **SHA‑1** and checks `state.db`. If seen, marks as **duplicate**.  
3. **De‑dupe (server)** – Attempts Immich `/assets/bulk-upload-check`; if Immich reports duplicate, marks accordingly.  
4. **Upload** – Multipart POST to `${IMMICH_BASE_URL}/assets` with:
   - `assetData`, `deviceAssetId`, `deviceId`,  
   - `fileCreatedAt`, `fileModifiedAt` (from EXIF when available; else `lastModified`),  
   - `isFavorite=false`, `filename`, and header `x-immich-checksum`.  
5. **Album** – If `IMMICH_ALBUM_NAME` is configured, adds the uploaded asset to the album (creates album if it doesn't exist).  
6. **Progress** – Backend streams progress via WebSocket to the same session.  
7. **Privacy** – UI shows only the current session's items. It never lists server media.

---

## Security notes

- The menu and invite creation are behind login. Logout clears the session.  
- Invite links are public by URL; share only with intended recipients.  
- The default uploader page at `/` is disabled unless `PUBLIC_UPLOAD_PAGE_ENABLED=true`.  
- With `PUBLIC_UPLOAD_PAGE_ENABLED=false`, the upload API itself also requires
  authentication: every upload/chunk request must carry a valid, active invite
  token (or come from a logged-in session). Requests without one are rejected
  before anything is written to disk or forwarded to Immich.  
- Session cookies are marked `Secure` automatically when `PUBLIC_BASE_URL`
  starts with `https://`.  
- The Immich API key remains **server‑side**; the browser never sees it.  
- No browsing of uploaded media; only ephemeral session state is shown.  
- Run behind HTTPS with a reverse proxy and restrict CORS to your domain(s).

## Usage flow

- Admin: Login → Menu → Create invite link (optionally one‑time / expiry / album) → Share link or QR.  
- Guest: Open invite link → Drop files → Upload progress and results shown.  
- Optional: Enable public uploader and set `IMMICH_ALBUM_NAME` for a default landing page.

---

## License

MIT.
