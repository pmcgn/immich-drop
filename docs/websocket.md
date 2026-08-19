# WebSocket protocol (`/ws`)

Upload progress is pushed from backend to browser over a single WebSocket per page.
(OpenAPI cannot describe this; the HTTP API is in [openapi.yaml](openapi.yaml).)

## Connection lifecycle

1. Frontend generates a random `session_id` (opaque string; it is also sent with every
   upload request so the backend knows where to route progress).
2. Frontend opens `ws(s)://{host}/ws` and immediately sends **one** text frame:

   ```json
   {"session_id": "<session id>", "invite_token": "<invite token or empty>"}
   ```

   If the first frame is missing/invalid JSON, the backend falls back to session id
   `"default"`. The frame must arrive within 10 s of the upgrade, otherwise the
   socket is closed.
3. The registration is subject to the same auth rule as the upload endpoints: with
   `PUBLIC_UPLOAD_PAGE_ENABLED=false`, a login session or a valid, active invite token
   is required — unauthorized sockets are closed with code 1008 (policy violation).
   The backend then registers the socket in an in-memory hub under that session id.
   Multiple sockets may share one session id (all receive every message).
4. Side effect: if this is the first socket for a session id not currently in the hub, the
   backend **resets its cached default-album id** (so a freshly opened page re-resolves the
   album by name).
5. Keepalive: if the client sends nothing for 30 s, the server sends
   `{"type":"ping"}` (client ignores it; it exists to defeat proxy idle timeouts). Any
   client frame resets the timer; client frames after the first are otherwise ignored.
6. On disconnect the socket is removed from the hub. The frontend reconnects after 2 s.

## Server → client messages

### Progress event (per queue item)

```json
{
  "item_id": "<id the client sent with the upload request>",
  "status": "checking | uploading | duplicate | done | error",
  "progress": 0,
  "message": "human-readable status text",
  "responseId": "<Immich asset id or null>"
}
```

| Field | Notes |
| --- | --- |
| `item_id` | Correlates with the `item_id` form/JSON field of the upload request |
| `status` | See sequences below; `duplicate`, `done`, `error` are terminal |
| `progress` | Integer 0–100. The client ignores decreases and treats terminal states as 100 |
| `message` | e.g. `"Checking duplicates…"`, `"Uploading…"`, `"Duplicate (server)"`, `"created (added to album 'X')"`, or an error string |
| `responseId` | Immich asset id when known (server-duplicate or successful upload), else `null` |

### Keepalive

```json
{"type":"ping"}
```

## Typical sequences for one upload

Successful upload:
```
checking(2) → uploading(0) → uploading(1..99, deduped by percent) → done(100, message=status, responseId=assetId)
```

Local-cache duplicate (no Immich call):
```
duplicate(100, "Duplicate (by checksum - local cache)")          # or "Already uploaded from this device (local cache)"
```

Server-side duplicate:
```
checking(2) → duplicate(100, "Duplicate (server)", responseId=assetId)
```

Invite rejection (also mirrored as an HTTP 403 with an error code):
```
error(100, "Invalid invite token" | "Invite disabled" | "Password required" | "Invite expired" | "Invite already used" | "Invite already used up")
```

Immich/HTTP failure:
```
... → error(100, "<message from Immich or exception text>")
```

Notes:
- Progress percentages during `uploading` reflect bytes written **to Immich** (multipart
  monitor callback), emitted only when the integer percent changes.
- The HTTP response of the upload request carries the same final outcome; the client uses
  whichever arrives and ignores regressive WS updates after finalizing an item.
- Client-side statuses `queued` (before request starts) exist only in the frontend and are
  never sent by the backend.
