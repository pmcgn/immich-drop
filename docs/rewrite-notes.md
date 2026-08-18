# Rewrite notes (Python → Go)

Things the Go implementation must preserve, plus quirks and outright bugs in the current
code where the rewrite should consciously decide whether to replicate or fix.

## Hard compatibility requirements

The frontend stays unchanged, so these are non-negotiable:

- All routes, methods, request/response shapes in [openapi.yaml](openapi.yaml), including
  the exact `{"error": "<code>"}` codes the frontend matches on (e.g.
  `invite_password_required`, `invalid_password`).
- The WebSocket protocol in [websocket.md](websocket.md): first-frame session registration,
  progress message field names (`item_id`, `status`, `progress`, `message`, `responseId`),
  status values, and the `{"type":"ping"}` keepalive.
- Duplicate uploads must return HTTP **200** with `status: "duplicate"` (the frontend
  treats any status containing "duplicate", case-insensitive, as a duplicate).
- The `" (added to album '<name>')"` suffix on `status` is user-visible text shown in the UI.
- Cookie-session semantics: `/menu` redirect and 401s hinge on the presence of
  `accessToken` in the session; invite password unlocks live in the session as
  `inviteAuth`. Go should use an equivalent signed-cookie session (e.g. gorilla/sessions);
  existing Python cookies don't need to survive the migration, but `SESSION_SECRET`
  should keep meaning "stable secret across restarts".
- SQLite file compatibility: existing deployments have `state.db` files with the schemas in
  [database.md](database.md), including partially-migrated `invites` tables and either
  variant of `upload_events`. The Go version must run the same idempotent migrations (or
  better ones) against pre-existing files.
- Invite password hashes must keep verifying: format
  `pbkdf2_sha256$<iterations>$<salt_hex>$<hash_hex>` with PBKDF2-HMAC-SHA256
  (creation used 200 000 iterations; verification reads the iteration count from the string).
- Dedupe identity: SHA-1 hex checksum and `deviceAssetId = "{filename}-{lastModified|0}-{size}"`.
  Changing either invalidates the existing local cache and changes Immich-side dedupe.
- `deviceId` sent to Immich is `"python-" + session_id`. Renaming the prefix (e.g. `go-`)
  is visible in Immich metadata; it does not affect dedupe (which keys on checksum and
  deviceAssetId), but keep it in mind.
- Environment variable names and defaults (see architecture.md#configuration) — deployments
  configure via `.env` / docker-compose environment.

## Known bugs — decide: replicate or fix (recommendation: fix)

1. **`upload_events` schema conflict** (see [database.md](database.md)): `db_init()` creates
   the table with `uploadedat`/`useragent`/`immichassetid`, the insert + read paths use
   `uploaded_at`/`user_agent`/`immich_asset_id`. On fresh databases every audit-log insert
   fails silently, and `GET /api/invite/{token}/uploads` errors with `db_error`
   (`{"items": []}`-style empty results only work on old DBs). Fix: standardize on the
   snake_case schema; if a legacy no-underscore table exists, migrate/rename it.
2. **Chunk root is hardcoded** to `/data/chunks` (ignores `STATE_DB` location, breaks on
   Windows / non-Docker runs; the `makedirs` failure is swallowed and chunk endpoints then
   500). Fix: make it configurable (e.g. `CHUNK_DIR`) with the current path as default.
3. **`meta.json` invite_token is never used**: `chunk/complete` takes `invite_token` from
   its own request body, so a token sent only at `init`/`chunk` time is ignored. The
   current frontend sends it in `complete` too, so this is latent. Fix or keep; document
   either way.
4. **Naive-UTC timestamps for invite expiry** (`datetime.utcnow()`, no timezone suffix) —
   works because both write and compare sides are naive UTC. In Go, keep storing/parsing
   these existing values but consider writing RFC 3339 UTC going forward (must still parse
   old naive values).
5. **`MAX_CONCURRENT` is dead config** — loaded, never enforced (the client throttles
   itself to 3 parallel uploads). Either enforce it server-side or drop it from docs.
6. **Orphaned chunk directories** are never garbage-collected if a client abandons an
   upload between `init` and `complete`. Consider a TTL sweep.
7. **`upload_events` deletes are not owner-scoped**: `POST /api/invites/delete` deletes
   events for the given tokens *before* checking invite ownership (the invites delete is
   owner-scoped, the events delete is not). Practical impact is low (tokens are
   unguessable), but the Go version should scope both.

## Behavior worth knowing (not bugs, but easy to miss)

- **Whole file in memory**: both upload paths read the entire file (and all chunks) into
  RAM before forwarding. Go can stream (tee to SHA-1 while spooling), but the SHA-1 must be
  known *before* the Immich call (`x-immich-checksum` header and bulk-check), so a temp
  file or full buffer is still needed — just avoid the multi-copy the Python version does.
- **EXIF dates**: only images Pillow can parse yield EXIF dates; videos always fall back to
  `last_modified`/now. EXIF format parsed: `"%Y:%m:%d %H:%M:%S"`, naive → serialized with
  `Z` (UTC) suffix. The Go rewrite needs an equivalent EXIF reader (e.g. goexif) with the
  same fallback chain: DateTimeOriginal → CreateDate; ModifyDate → DateTimeOriginal.
- **Filename sanitation**: strip control chars (0x00–0x1F, 0x7F), replace `/` and `\` with
  `_`, trim whitespace, fall back to `"file"`. The *unsanitized* name is stored in the local
  DB and audit log; the sanitized one goes to Immich.
- **Album-id cache invalidation**: the process-global default-album id is reset whenever a
  WS connection arrives for an unknown session id, and via `POST /api/album/reset` (called
  by the frontend on page load/unload). Rationale: lets an admin rename the drop album and
  have a new one auto-created without restarting.
- **One-time invite claim race**: claim is an atomic conditional UPDATE; on `changed == 0`
  the code re-reads `claimed_by_session` and only rejects if it belongs to someone else.
  Keep this exact semantic (same session may retry/upload multiple files on a one-time link).
- **`/api/ping` returns `ok: false` when `IMMICH_API_KEY` is empty**, even if a session
  token would work — the probe only ever uses the API key.
- **Album add tolerance**: `PUT /albums/{id}/assets` per-asset result `error: "duplicate"`
  counts as success.
- **Immich upload**: multipart fields must include `assetData`, `deviceAssetId`, `deviceId`,
  `fileCreatedAt`, `fileModifiedAt`, `isFavorite`, `filename`, `originalFileName`; header
  `x-immich-checksum` enables server-side duplicate detection with status `duplicate` in a
  200/201 response.
- **CORS is allow-all** with credentials. (Technically an invalid combination per the CORS
  spec — browsers reject `Access-Control-Allow-Origin: *` with credentials — but same-origin
  requests, which is all the frontend does, never hit CORS at all. The Go version could
  simply drop CORS or restrict it.)
- **Errors are deliberately swallowed** in many places (audit logging, chunk cleanup, album
  assignment). Upload success must not fail because a secondary step failed; keep that
  property, but log the failures.
- **Timeouts** used against Immich: login 15 s, uploads 120 s, everything else 4–10 s.
- **Logging**: Python `logging` with level from `LOG_LEVEL`; useful signals to keep: login
  events, album create/find, invite claim failures, Immich error bodies.

## Suggested Go shape (non-binding)

- stdlib `net/http` + `gorilla/websocket` (or `nhooyr.io/websocket`) + `mattn/go-sqlite3`
  or `modernc.org/sqlite` (CGO-free helps cross-compiled Docker images).
- Share one upload pipeline function between whole-file and chunked paths (the Python
  version duplicates ~250 lines; don't repeat that).
- The WS hub is a simple `map[string][]*conn` guarded by a mutex; progress sends must
  never block or fail an upload.
- Serve `frontend/` via `embed.FS` or a bind-mounted directory (keep `/static` mount path).
