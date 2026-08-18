# Architecture

Source of truth: `app/app.py` (~1780 lines, single module), `app/config.py`, `main.py`.

```
Browser (frontend/*.html, app.js)
   │  HTTP (multipart uploads, JSON APIs)          WebSocket /ws (progress push)
   ▼                                                ▼
FastAPI backend (app/app.py) ────────────────► SessionHub (in-memory, per session_id)
   │            │
   │            └── SQLite (STATE_DB): uploads, upload_events, invites
   ▼
Immich server (IMMICH_BASE_URL, e.g. http://immich:2283/api)
   auth: x-api-key (env) OR Bearer token (logged-in session cookie)
```

## Components

### HTTP layer (FastAPI + Starlette middleware)

- **CORS middleware**: allow-all (`*` origins, credentials, methods, headers).
- **SessionMiddleware** (Starlette): signed cookie session, `SameSite=lax`, secret =
  `SESSION_SECRET` (random per process start when unset — restarting logs everyone out).
  The session cookie stores: `accessToken` (Immich bearer token), `userEmail`, `userId`,
  `name`, `isAdmin`, and `inviteAuth` (a `{token: true}` map of invites this browser has
  unlocked with a password). Nothing is persisted server-side for sessions.
- **Static files**: `frontend/` mounted at `/static`. Several HTML pages are served directly
  by routes (see below).

### Page routes (serve static HTML, may redirect)

| Route | Behavior |
| --- | --- |
| `GET /` | `frontend/index.html`; if `PUBLIC_UPLOAD_PAGE_ENABLED` is false → redirect `/login` |
| `GET /login` | `frontend/login.html` |
| `GET /menu` | `frontend/menu.html`; redirect `/login` if session has no `accessToken` |
| `GET /invite/{token}` | `frontend/invite.html` (always served; token validity checked via API) |
| `GET /logout` | clears session, redirect `/login` |
| `GET /favicon.ico` | serves `frontend/favicon.png` or `204` |

### API layer

Fully specified in [openapi.yaml](openapi.yaml). Groups:

- **Status/config**: `POST /api/ping`, `GET /api/config`
- **Upload**: `POST /api/upload` (whole file), `POST /api/upload/chunk/init` +
  `POST /api/upload/chunk` + `POST /api/upload/chunk/complete` (chunked),
  `POST /api/album/reset`
- **Auth**: `POST /api/login`, `POST /api/logout` (proxy to Immich `/auth/login`)
- **Albums** (proxy to Immich): `GET /api/albums`, `POST /api/albums`
- **Invites**: create/list/update/bulk-toggle/delete, per-token info, password auth,
  per-token upload log
- **Misc**: `GET /api/qr` (QR code PNG generator)

### WebSocket hub

In-memory map `session_id → [WebSocket]`. The frontend generates a random `session_id`,
opens `/ws`, sends `{"session_id": ...}` once, then receives progress JSON for every upload
it performs (it passes the same `session_id` in upload requests). Full protocol in
[websocket.md](websocket.md).

### Persistence

Single SQLite database (`STATE_DB`). Three tables: `uploads` (local dedupe cache),
`upload_events` (audit log of uploads per invite), `invites`. Connections are opened and
closed per operation; no pool, no ORM, `sqlite3` stdlib. Schema and quirks in
[database.md](database.md).

Chunked uploads buffer parts on disk under `/data/chunks/{session_id}/{item_id}/`
(`part_000000`, `part_000001`, …, plus `meta.json`), assembled in memory on complete and
deleted immediately after.

## Key flows

### Whole-file upload (`POST /api/upload`)

1. Read entire file into memory; compute `size` and SHA-1 hex `checksum`.
2. Extract EXIF `DateTimeOriginal`/`ModifyDate` (Pillow) → `created_at`/`modified_at`;
   fall back to the browser-provided `last_modified` (epoch ms), then to "now" (UTC).
   Naive datetimes are serialized as ISO-8601 with a `Z` suffix (treated as UTC).
3. Build `device_asset_id = "{filename}-{last_modified or 0}-{size}"`.
4. **Local dedupe**: if `checksum` or `device_asset_id` already in the `uploads` table →
   respond `{"status": "duplicate", "id": null}` (200) and push a `duplicate` WS event.
5. **Server dedupe**: call Immich `POST /assets/bulk-upload-check` with
   `{assets:[{id: item_id, checksum}]}`. If the result action is `reject` with reason
   `duplicate` → record in local cache and respond duplicate with the existing Immich
   `assetId`.
6. **Invite gating** (only when `invite_token` was sent): load the invite row and reject with
   403 + error code when the invite is unknown (`invalid_invite`), disabled
   (`invite_disabled`), password-protected but the session isn't in `inviteAuth`
   (`invite_password_required`), expired (`invite_expired`), one-time and claimed by a
   different session (`invite_claimed`), or multi-use and exhausted (`invite_exhausted`).
   A one-time invite (max_uses == 1) is **claimed atomically** on first upload
   (`UPDATE ... WHERE claimed IS NULL OR claimed = 0`, then re-check owner on race) and bound
   to the uploader's `session_id`, allowing that session to upload multiple files.
7. **Forward to Immich**: `POST {base}/assets` as multipart with fields `assetData` (the
   bytes, sanitized filename), `deviceAssetId`, `deviceId` (= `"python-" + session_id`),
   `fileCreatedAt`, `fileModifiedAt`, `isFavorite: "false"`, `filename`, `originalFileName`,
   plus header `x-immich-checksum: <sha1>`. Upload progress percent is streamed to the
   WebSocket as the request body is written (via a multipart-encoder monitor callback).
8. On Immich 200/201: insert into local `uploads` cache; **album assignment**:
   - with invite: add asset to the invite's album (by id, else by name → resolved or
     created) — no fallback to the env default album;
   - without invite: add to `IMMICH_ALBUM_NAME` album if configured (id cached in a
     process-global, reset when a new WS session appears or via `POST /api/album/reset`).
   Then increment invite `used_count` (one-time invites stay at 1) and append a row to
   `upload_events` (token, ip, user-agent, fingerprint, filename, size, checksum, asset id).
   Push `done` (or `duplicate` if Immich reported status `duplicate`) over WS; respond
   `{"id": ..., "status": ...}` where `status` may have `" (added to album '...')"` appended.
9. On Immich error status: respond 400 `{"error": <immich message>}` + WS `error`.
   On exception: 500 `{"error": ...}` + WS `error`.

### Chunked upload

Used by the frontend only when `GET /api/config` reports `chunked_uploads_enabled` and the
file is larger than `chunk_size_mb`.

1. `init` (JSON): creates the chunk dir and writes `meta.json` (name, size, last_modified,
   invite_token, content_type, created_at).
2. `chunk` (multipart, repeated): writes `part_{index:06d}`; updates `total_chunks` (and
   `invite_token`) in `meta.json`.
3. `complete` (JSON): reads all parts in order (400 `missing_part` if a gap), concatenates
   in memory, deletes the chunk dir, then runs **exactly the same pipeline as whole-file
   upload** (steps 1–9 above; the code is duplicated, not shared). Metadata (name,
   last_modified, invite_token, fingerprint, content_type) comes from the `complete` request
   body; `total_chunks` and fallback `name` come from `meta.json`.

### Login / auth model

- `POST /api/login` proxies email+password to Immich `POST /auth/login`; on success stores
  the returned `accessToken` and user info in the cookie session.
- Every Immich call made on behalf of a request uses `immich_headers(request)`:
  `Authorization: Bearer <session accessToken>` when a session token exists, else
  `x-api-key: IMMICH_API_KEY`. Anonymous uploads therefore run under the API key identity.
- Invite management endpoints require a session `accessToken` (401 otherwise) and scope all
  DB queries to `owner_user_id = session.userId`.
- There is **no server-side session store and no token refresh/expiry handling**; an expired
  Immich token simply makes proxied calls fail (typically surfaced as 403/502).

### Albums

`get_or_create_album(name)`: `GET {base}/albums`, match on `albumName`, else
`POST {base}/albums` with `{albumName, description}`. The resolved id for the **default**
(env) album name is cached in the process-global `ALBUM_ID`; per-invite album lookups are
never cached. `add_asset_to_album` does `PUT {base}/albums/{id}/assets` with `{ids:[assetId]}`
and treats a per-asset `duplicate` error as success.

## Configuration

Loaded once at import time (`app/config.py`, `load_settings()`), from environment /
`.env` via python-dotenv. No runtime mutation.

| Env var | Default | Meaning |
| --- | --- | --- |
| `IMMICH_BASE_URL` | `http://127.0.0.1:2283/api` | Immich API base (trailing `/` stripped) |
| `IMMICH_API_KEY` | `""` | API key for anonymous uploads; also gates `/api/ping` (`ok:false` when empty) |
| `IMMICH_ALBUM_NAME` | `""` | Default album for non-invite uploads (empty = none) |
| `PUBLIC_UPLOAD_PAGE_ENABLED` | `false` | If false, `GET /` redirects to `/login`; invite pages still work |
| `PUBLIC_BASE_URL` | `""` | Absolute base for generated invite links (falls back to request base URL) |
| `MAX_CONCURRENT` | `3` | **Loaded but unused** by the backend (client throttles itself to 3) |
| `STATE_DB` | `/data/state.db` | SQLite file path |
| `SESSION_SECRET` | random per start | Cookie-signing secret |
| `LOG_LEVEL` | `INFO` | Python logging level |
| `CHUNKED_UPLOADS_ENABLED` | `false` | Advertised to the frontend via `/api/config` |
| `CHUNK_SIZE_MB` | `95` | Chunk size advertised to the frontend |
| `HOST` / `PORT` | `0.0.0.0` / `8080` | Listen address (read in `main.py` only) |

Boolean parsing accepts `1/true/yes/on` (case-insensitive).

## External API surface consumed (Immich)

The backend calls these Immich endpoints (relative to `IMMICH_BASE_URL`):

- `POST /auth/login` — email/password → `accessToken`
- `GET /albums`, `POST /albums`, `PUT /albums/{id}/assets`
- `POST /assets` — multipart asset upload (with `x-immich-checksum`)
- `POST /assets/bulk-upload-check` — duplicate pre-check by SHA-1
- `GET /server-info` | `/server/version` | `/users/me` — reachability probe for `/api/ping`
  (first 2xx/3xx wins)
