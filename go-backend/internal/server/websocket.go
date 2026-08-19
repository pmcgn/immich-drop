package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"immich-drop/internal/validate"
	"immich-drop/internal/ws"
)

// wsOriginAllowed rejects cross-site WebSocket handshakes (browsers always
// send Origin). Non-browser clients without an Origin header are allowed. The
// origin must match the request's Host or the configured PUBLIC_BASE_URL host.
func (s *Server) wsOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := strings.ToLower(u.Host)
	if originHost == "" {
		return false
	}
	allowed := map[string]bool{strings.ToLower(r.Host): true}
	if s.cfg.PublicBaseURL != "" {
		if pub, err := url.Parse(s.cfg.PublicBaseURL); err == nil && pub.Host != "" {
			allowed[strings.ToLower(pub.Host)] = true
		}
	}
	delete(allowed, "")
	return allowed[originHost]
}

// handleWS is the WebSocket endpoint for pushing per-item upload progress.
// Protocol: first client frame registers the session
// ({"session_id": "...", "invite_token": "..."}), then the server broadcasts
// progress and sends {"type":"ping"} after 30 s of client silence to defeat
// proxy idle timeouts. The registration is subject to the same auth rule as
// the upload endpoints (uploadAuthCode): with the public upload page disabled,
// a login session or a valid invite token is required.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return s.wsOriginAllowed(r) },
	}
	rawConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already replied (403 on origin rejection)
	}
	conn := ws.NewConn(rawConn)

	// The registration frame must arrive promptly: sockets that never register
	// are not in the hub, so without a deadline they would tie up goroutines
	// and file descriptors while bypassing the hub's connection limits.
	sessionID := "default"
	_ = rawConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, initFrame, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = rawConn.SetReadDeadline(time.Time{})
	var init struct {
		SessionID   string `json:"session_id"`
		InviteToken string `json:"invite_token"`
	}
	if json.Unmarshal(initFrame, &init) == nil && validate.IsValidClientID(init.SessionID) {
		sessionID = init.SessionID
	}
	inviteToken := ""
	if validate.IsValidInviteToken(init.InviteToken) {
		inviteToken = init.InviteToken
	}
	if s.uploadAuthCode(r, sessionID, inviteToken) != "" {
		conn.CloseWithCode(websocket.ClosePolicyViolation) // 1008: not authorized
		return
	}

	// First socket of a (possibly new) session: reset the album cache so a
	// freshly opened page can rotate the drop album by renaming the old one.
	if !s.hub.HasSession(sessionID) {
		s.ResetAlbumCache()
	}
	if !s.hub.Connect(sessionID, conn) {
		conn.CloseWithCode(websocket.CloseTryAgainLater) // 1013: connection limit reached
		return
	}

	// Reader goroutine: client messages only reset the keepalive timer.
	msgs := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				errCh <- err
				return
			}
			select {
			case msgs <- struct{}{}:
			default:
			}
		}
	}()

	keepalive := time.NewTimer(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-msgs:
			keepalive.Reset(30 * time.Second)
		case <-errCh:
			s.hub.Disconnect(sessionID, conn)
			return
		case <-keepalive.C:
			if err := conn.WriteText([]byte(`{"type":"ping"}`)); err != nil {
				s.hub.Disconnect(sessionID, conn)
				return
			}
			keepalive.Reset(30 * time.Second)
		}
	}
}
