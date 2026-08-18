# Database (SQLite)

Single file at `STATE_DB` (default `/data/state.db`), accessed with the Python `sqlite3`
module. One short-lived connection per operation, autocommit off with explicit `commit()`,
no WAL configuration, no foreign keys. All schema creation is idempotent
(`CREATE TABLE IF NOT EXISTS` + best-effort `ALTER TABLE ADD COLUMN` migrations that ignore
"duplicate column" errors), executed at import time.

## Table: `uploads` — local dedupe cache

Every asset this service has successfully forwarded (or confirmed as a server-side
duplicate) is recorded here so repeat uploads short-circuit without hitting Immich.

```sql
CREATE TABLE IF NOT EXISTS uploads (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    checksum TEXT UNIQUE,          -- SHA-1 hex of file bytes
    filename TEXT,                 -- original (unsanitized) filename
    size INTEGER,                  -- bytes
    device_asset_id TEXT,          -- "{filename}-{lastModified|0}-{size}"
    immich_asset_id TEXT,          -- id returned by Immich (nullable)
    created_at TEXT,               -- ISO capture date sent to Immich
    inserted_at TEXT DEFAULT CURRENT_TIMESTAMP
);
```

Access patterns:
- `SELECT ... WHERE checksum = ?` (dedupe check 1)
- `SELECT 1 ... WHERE device_asset_id = ?` (dedupe check 2 — note: **no index**, full scan)
- `INSERT OR IGNORE` after successful upload / confirmed server duplicate

## Table: `invites`

```sql
CREATE TABLE IF NOT EXISTS invites (
    token TEXT PRIMARY KEY,        -- uuid4().hex (32 lowercase hex chars)
    album_id TEXT,                 -- resolved Immich album id (nullable)
    album_name TEXT,               -- album display name (nullable)
    max_uses INTEGER DEFAULT 1,    -- 1 = one-time; <0 = unlimited; N>1 = N uploads
    used_count INTEGER DEFAULT 0,
    expires_at TEXT,               -- naive-UTC ISO string, no microseconds (nullable)
    created_at TEXT DEFAULT CURRENT_TIMESTAMP,
    -- added via best-effort migrations:
    claimed INTEGER DEFAULT 0,     -- one-time invites: set on first upload
    claimed_at TEXT,
    claimed_by_session TEXT,       -- frontend session_id that claimed it
    password_hash TEXT,            -- "pbkdf2_sha256$<iter>$<salt_hex>$<hash_hex>" (nullable)
    owner_user_id TEXT,            -- Immich userId of creator (ownership scope)
    owner_email TEXT,
    owner_name TEXT,
    name TEXT,                     -- friendly label, default "{album|NoAlbum}-YYYYMMDD-HHMM"
    disabled INTEGER DEFAULT 0     -- admin kill-switch
);
```

Semantics:
- **One-time (`max_uses = 1`)**: first upload atomically claims the token
  (`UPDATE invites SET claimed=1, claimed_at=CURRENT_TIMESTAMP, claimed_by_session=? WHERE
  token=? AND (claimed IS NULL OR claimed=0)`) and binds it to that browser session; the
  same session may keep uploading, other sessions get `invite_claimed`. `used_count` is
  pinned to 1.
- **Multi-use (`max_uses > 1`)**: `used_count` increments per successful upload; rejected
  with `invite_exhausted` once `used_count >= max_uses`.
- **Unlimited (`max_uses < 0`)**: never exhausted.
- Expiry comparison: `datetime.utcnow() > fromisoformat(expires_at)` (naive UTC).
- Passwords: PBKDF2-HMAC-SHA256, 200 000 iterations, 16-byte random salt, stored as
  `pbkdf2_sha256$200000$<salt hex>$<derived key hex>`.
- All management queries (list/update/bulk/delete/uploads-log) are scoped by
  `owner_user_id`; public read of a single invite (`GET /api/invite/{token}`) is not.

## Table: `upload_events` — audit log

⚠️ **Two conflicting schemas exist in the code.** `db_init()` (runs first, at import)
creates:

```sql
CREATE TABLE IF NOT EXISTS upload_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT,
    uploadedat TEXT DEFAULT CURRENT_TIMESTAMP,   -- no underscore!
    ip TEXT,
    useragent TEXT,                              -- no underscore!
    fingerprint TEXT,
    filename TEXT,
    size INTEGER,
    checksum TEXT,
    immichassetid TEXT                           -- no underscore!
);
```

…while the upload handlers (and the reader `GET /api/invite/{token}/uploads`) use the
snake_case variant:

```sql
CREATE TABLE IF NOT EXISTS upload_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token TEXT,
    uploaded_at TEXT DEFAULT CURRENT_TIMESTAMP,
    ip TEXT,
    user_agent TEXT,
    fingerprint TEXT,
    filename TEXT,
    size INTEGER,
    checksum TEXT,
    immich_asset_id TEXT
);
```

On a **fresh** database, `db_init()` wins (the handler's `CREATE TABLE IF NOT EXISTS` is a
no-op), the snake_case `INSERT` then fails and is **silently swallowed** — i.e. upload-event
logging is broken unless the DB predates `db_init()`'s variant. See
[rewrite-notes.md](rewrite-notes.md); the Go rewrite should standardize on the snake_case
schema (which the read API expects) and migrate the other if found.

Writes: one row per successful upload — `token` (empty string when no invite), `ip`
(client address, falling back to `X-Forwarded-For`), `user_agent`, `fingerprint`
(client-generated browser fingerprint string), `filename`, `size`, `checksum`,
`immich_asset_id`. Failures to log are deliberately ignored.

Reads: `GET /api/invite/{token}/uploads` — latest 500 rows for a token, owner-only.
Deletes: `POST /api/invites/delete` removes events for the deleted tokens (before deleting
the invites, not owner-scoped on the events table).
