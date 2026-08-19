// Package ws implements the per-session WebSocket hub used to push upload
// progress to the browser. Protocol (docs/websocket.md): the client sends a
// first frame {"session_id": "..."}; the server then broadcasts progress
// objects and a {"type":"ping"} keepalive every 30 s of silence.
package ws

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	MaxSessions          = 1000
	MaxSocketsPerSession = 8
	// writeTimeout bounds every frame write so a stalled client socket (full
	// send buffer, frozen peer) cannot block progress sends — and with them the
	// upload goroutine — indefinitely. Writes that miss the deadline fail, and
	// the hub then drops the socket like any other failed one.
	writeTimeout = 5 * time.Second
)

// Conn wraps a websocket connection with a write mutex (gorilla/websocket
// allows only one concurrent writer).
type Conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func NewConn(ws *websocket.Conn) *Conn { return &Conn{ws: ws} }

// WriteText sends one text frame, serialized with the connection's write lock
// and bounded by writeTimeout.
func (c *Conn) WriteText(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// ReadMessage proxies to the underlying connection (single reader per conn).
func (c *Conn) ReadMessage() (int, []byte, error) { return c.ws.ReadMessage() }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.ws.Close() }

// CloseWithCode sends a close frame with the given code, then closes.
func (c *Conn) CloseWithCode(code int) {
	c.mu.Lock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = c.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""))
	c.mu.Unlock()
	_ = c.ws.Close()
}

// Hub holds WebSocket connections per session and broadcasts progress updates.
type Hub struct {
	mu       sync.Mutex
	sessions map[string][]*Conn
}

func NewHub() *Hub {
	return &Hub{sessions: make(map[string][]*Conn)}
}

// HasSession reports whether any socket is currently registered for sessionID.
func (h *Hub) HasSession(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions[sessionID]) > 0
}

// Connect registers a socket under sessionID. Returns false (without
// registering) when the session or per-session socket limit is exceeded, so a
// client cannot grow the hub unboundedly.
func (h *Hub) Connect(sessionID string, c *Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	bucket, exists := h.sessions[sessionID]
	if !exists && len(h.sessions) >= MaxSessions {
		return false
	}
	if len(bucket) >= MaxSocketsPerSession {
		return false
	}
	h.sessions[sessionID] = append(bucket, c)
	return true
}

// Disconnect removes a socket from the hub and closes it (best-effort).
func (h *Hub) Disconnect(sessionID string, c *Conn) {
	h.mu.Lock()
	h.remove(sessionID, c)
	h.mu.Unlock()
	_ = c.Close()
}

// remove drops c from the session bucket; caller must hold h.mu.
func (h *Hub) remove(sessionID string, c *Conn) {
	bucket := h.sessions[sessionID]
	for i, other := range bucket {
		if other == c {
			bucket = append(bucket[:i], bucket[i+1:]...)
			break
		}
	}
	if len(bucket) == 0 {
		delete(h.sessions, sessionID)
	} else {
		h.sessions[sessionID] = bucket
	}
}

// Send broadcasts a JSON payload to all sockets of one session. Failed sockets
// are closed and dropped; sends never propagate errors to the upload path.
func (h *Hub) Send(sessionID string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	conns := append([]*Conn(nil), h.sessions[sessionID]...)
	h.mu.Unlock()

	var failed []*Conn
	for _, c := range conns {
		if err := c.WriteText(data); err != nil {
			_ = c.Close()
			failed = append(failed, c)
		}
	}
	if len(failed) > 0 {
		h.mu.Lock()
		for _, c := range failed {
			h.remove(sessionID, c)
		}
		h.mu.Unlock()
	}
}
