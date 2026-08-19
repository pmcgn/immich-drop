# Immich Drop Uploader — Go backend

A Go rewrite of the Python/FastAPI backend ([`app/app.py`](../app/app.py)). It serves the
**unchanged** frontend from [`../frontend`](../frontend) and preserves every route, request/response
shape, error code, and the WebSocket progress protocol (see [`../docs/openapi.yaml`](../docs/openapi.yaml),
[`../docs/websocket.md`](../docs/websocket.md), and [`../docs/rewrite-notes.md`](../docs/rewrite-notes.md)).
Existing `state.db` files, invite password hashes, and `.env` configurations keep working.

## Run

```sh
cd go
go build -o immich-drop .
./immich-drop            # reads .env / environment, same variables as the Python version
```

The frontend directory is auto-detected (`./frontend` or `../frontend`) and can be pinned
with `FRONTEND_DIR`. All other environment variables match the Python version
(`IMMICH_BASE_URL`, `IMMICH_API_KEY`, `IMMICH_ALBUM_NAME`, `PUBLIC_UPLOAD_PAGE_ENABLED`,
`PUBLIC_BASE_URL`, `STATE_DB`, `SESSION_SECRET`, `LOG_LEVEL`, `CHUNKED_UPLOADS_ENABLED`,
`CHUNK_SIZE_MB`, `HOST`, `PORT`). Additions: `CHUNK_DIR` (default `/data/chunks`, which
the Python version hardcoded) and `ADMIN_PORT` (optional, see below). `CHUNK_DIR` is the on-disk
upload cache — chunk parts and upload spool files live there, file content is never
buffered in memory — so mount it as a volume (like `/data` for `STATE_DB`) if you want
in-flight chunks to survive a container or pod recreation.

### Split-port mode (`ADMIN_PORT`)

When `ADMIN_PORT` is set, the server runs two listeners on the same host address:

- **`PORT` (public upload port):** upload pages and endpoints — `/`, `/invite/{token}`,
  `/ws`, `/api/upload*`, `/api/album/reset`, `GET /api/invite/{token}`,
  `POST /api/invite/{token}/auth`.
- **`ADMIN_PORT` (admin port):** login and invite management — `/login`, `/menu`,
  `/logout`, `/api/login`, `/api/logout`, `/api/albums`, `/api/invites*`,
  `PATCH /api/invite/{token}`, `GET /api/invite/{token}/uploads`, `/api/qr`,
  `POST /api/ping` (the "Test connection" button; its response reveals the Immich base
  URL, so it stays off the public port), and the `GET /healthz` liveness probe. In
  single-port mode all of these are served on `PORT` like everything else.

Static assets (`/static/`, `/favicon.ico`) and the config probe used by every page
(`GET /api/config`) are served on both ports. This lets you expose only
the upload port publicly (e.g. through a tunnel/reverse proxy) while keeping the admin UI
reachable only internally. Notes:

- Set `PUBLIC_BASE_URL` to the upload port's public address; otherwise invite links
  created via the admin port point at the admin address (the server logs a warning).
- Both listeners share one process (session secret, SQLite store, album cache), so
  behavior is identical to single-port mode apart from routing. Admin login sessions
  carry over to the upload port only when both are reached under the same hostname
  (cookies ignore ports); otherwise public-page uploads fall back to the API key.
- With `PUBLIC_UPLOAD_PAGE_ENABLED=false`, `/` on the upload port returns 404 instead
  of redirecting to `/login` (which only exists on the admin port).

When `ADMIN_PORT` is unset (the default), all endpoints are served on `PORT` exactly as
before.

### Health check

`GET /healthz` returns `{"ok":true}` when the HTTP layer is up (Immich reachability is
deliberately not part of liveness — `/api/ping` covers that). Running the binary with
`-healthcheck` probes the endpoint of an already-running server and exits 0/1; the
Docker image uses this as its `HEALTHCHECK` command since the distroless runtime has no
shell or curl. The probe reads the same environment as the server, so it targets
`ADMIN_PORT` automatically when split-port mode is active.

The build is pure Go (no cgo): `modernc.org/sqlite` is used for SQLite, so
cross-compilation for Docker images is a plain `GOOS=linux go build`.

## Architecture

The single 1 900-line `app.py` is split into focused packages:

```
go/
├── main.go                     entrypoint: config, logging, DB open, HTTP server
└── internal/
    ├── config/                 .env loading, defaults (mirrors app/config.py)
    ├── validate/               input validation: regexes, limits, filename sanitation
    ├── session/                signed-cookie session (accessToken, inviteAuth)
    ├── passhash/               pbkdf2_sha256 invite-password hashes (format-compatible)
    ├── exifdate/               EXIF DateTimeOriginal/ModifyDate extraction
    ├── store/                  SQLite state DB
    │   ├── store.go            open + idempotent migrations (incl. legacy repair)
    │   ├── uploads.go          local dedupe cache
    │   ├── invites.go          invite CRUD, claim/usage accounting
    │   └── events.go           upload audit log
    ├── ws/                     WebSocket hub: per-session buckets, broadcasts
    ├── immich/                 Immich API client (login, albums, bulk-check, upload)
    └── server/                 HTTP layer
        ├── server.go           routing, CORS, album cache, shared helpers
        ├── pages.go            HTML pages, /api/ping, /api/config, /api/album/reset
        ├── auth.go             /api/login, /api/logout
        ├── albums.go           /api/albums (list/create proxy)
        ├── invites.go          invite create/list/update/bulk/delete/info/auth/uploads
        ├── upload.go           shared upload pipeline + /api/upload
        ├── chunks.go           chunked upload endpoints (init/chunk/complete)
        ├── websocket.go        /ws handshake, keepalive, origin check
        └── qr.go               /api/qr
```

The two upload paths (whole-file and chunked) share one pipeline
(`server.runUploadPipeline`) instead of the ~250 duplicated lines in the Python version.

## Deliberate fixes (behavior-neutral for the frontend)

Per the recommendations in [`../docs/rewrite-notes.md`](../docs/rewrite-notes.md):

1. **`upload_events` schema conflict fixed** — the table is standardized on snake_case
   columns; legacy databases created by the buggy Python `db_init` are migrated in place
   (column renames), so audit logging and `GET /api/invite/{token}/uploads` work on fresh
   databases too.
2. **Chunk directory configurable** — `CHUNK_DIR` env var (default unchanged:
   `/data/chunks`), so chunked uploads work outside Docker and the cache can be
   volume-mounted to survive pod recreation.
3. **Owner-scoped deletes** — `POST /api/invites/delete` scopes the `upload_events`
   delete to the invite owner, like the invites delete already was.
4. **`MAX_CONCURRENT` is enforced** — a server-side semaphore limits how many uploads
   are spooled/forwarded at once (the Python version loaded the setting but never used
   it).
5. **Abandoned chunk uploads are garbage-collected** — a background sweep removes chunk
   spool directories with no writes for 24 h (previously they accumulated forever, and
   the unauthenticated chunk endpoints made that a disk-exhaustion vector).
6. **`/api/albums` requires login** — listing/creating albums previously fell back to
   the server API key, letting anonymous visitors enumerate album names or flood Immich
   with empty albums. Albums tied to invite uploads are unaffected (they are resolved
   server-side inside the upload pipeline).
7. **Large uploads** — file content is never held in memory: whole-file uploads and
   assembled chunk uploads are spooled to disk under `CHUNK_DIR` and streamed to Immich
   from there (the Python version buffered the entire file, and all chunks, in RAM). The
   upload timeout scales with file size instead of the flat 120 s that capped transfers
   at ~1 GB. There is deliberately **no upload size limit**; multi-GB videos are expected.

Everything else — including quirks like the `"python-"` deviceId prefix, the
`chunk/complete` invite token coming from the request body, naive-UTC invite expiry
strings, and duplicate uploads returning HTTP 200 — is preserved as documented.
