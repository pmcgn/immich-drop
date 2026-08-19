// Package server wires the HTTP API. Routes, request/response shapes, and
// error codes mirror the Python FastAPI app one-to-one (see docs/openapi.yaml);
// the frontend is unchanged and depends on them.
package server

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"immich-drop/internal/config"
	"immich-drop/internal/immich"
	"immich-drop/internal/session"
	"immich-drop/internal/store"
	"immich-drop/internal/ws"
)

type Server struct {
	cfg      *config.Settings
	store    *store.Store
	hub      *ws.Hub
	immich   *immich.Client
	sessions *session.Manager

	// Cached id of the default (env-configured) drop album. Reset via
	// POST /api/album/reset and whenever a WS connection arrives for an
	// unknown session, so an admin can rename the drop album and have a new
	// one auto-created without restarting.
	albumMu sync.Mutex
	albumID string

	// uploadSem enforces MAX_CONCURRENT server-side (the Python version only
	// loaded it). File content is spooled to disk (CHUNK_DIR), so this bounds
	// concurrent Immich transfers and spool disk churn, not memory.
	// Nil = unlimited.
	uploadSem chan struct{}
}

func New(cfg *config.Settings, st *store.Store) *Server {
	var sem chan struct{}
	if cfg.MaxConcurrent > 0 {
		sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return &Server{
		cfg:       cfg,
		store:     st,
		hub:       ws.NewHub(),
		immich:    immich.NewClient(cfg.NormalizedBaseURL(), cfg.ImmichAPIKey),
		sessions:  session.NewManager(cfg.SessionSecret),
		uploadSem: sem,
	}
}

// acquireUploadSlot blocks until an upload slot is free and returns the
// release function. Callers must defer the release.
func (s *Server) acquireUploadSlot() func() {
	if s.uploadSem == nil {
		return func() {}
	}
	s.uploadSem <- struct{}{}
	return func() { <-s.uploadSem }
}

// registerShared adds the routes every page depends on: static assets plus the
// config probe called by header.js/app.js on all pages.
func (s *Server) registerShared(mux *http.ServeMux) {
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.cfg.FrontendDir))))
	mux.HandleFunc("GET /api/config", s.handleConfig)
}

// registerUpload adds the routes needed by the upload pages (index.html and
// invite.html): the pages themselves, the progress WebSocket, the upload
// endpoints, and the public (no-login) invite info/password endpoints.
func (s *Server) registerUpload(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /invite/{token}", s.handleInvitePage)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("POST /api/upload/chunk/init", s.handleChunkInit)
	mux.HandleFunc("POST /api/upload/chunk", s.handleChunk)
	mux.HandleFunc("POST /api/upload/chunk/complete", s.handleChunkComplete)
	// Called by the upload page's "Clear" buttons (unauthenticated by design).
	mux.HandleFunc("POST /api/album/reset", s.handleAlbumReset)
	mux.HandleFunc("GET /api/invite/{token}", s.handleInviteInfo)
	mux.HandleFunc("POST /api/invite/{token}/auth", s.handleInviteAuth)
}

// registerAdmin adds the routes needed by the login and invite-management
// pages (login.html and menu.html). The health probe lives here so that in
// split-port mode it is served on the internal admin port, not the public one.
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealth)
	// The connection test ("Test connection" button) exists only on the
	// login/menu pages; its response reveals the Immich base URL, so in
	// split-port mode it stays off the public upload port.
	mux.HandleFunc("POST /api/ping", s.handlePing)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /menu", s.handleMenuPage)
	mux.HandleFunc("GET /logout", s.handleLogoutRedirect)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/albums", s.handleAlbumsList)
	mux.HandleFunc("POST /api/albums", s.handleAlbumsCreate)
	mux.HandleFunc("POST /api/invites", s.handleInvitesCreate)
	mux.HandleFunc("GET /api/invites", s.handleInvitesList)
	mux.HandleFunc("POST /api/invites/bulk", s.handleInvitesBulk)
	mux.HandleFunc("POST /api/invites/delete", s.handleInvitesDelete)
	mux.HandleFunc("PATCH /api/invite/{token}", s.handleInviteUpdate)
	mux.HandleFunc("GET /api/invite/{token}/uploads", s.handleInviteUploads)
	mux.HandleFunc("GET /api/qr", s.handleQR)
}

// Handler returns the fully-routed HTTP handler serving every endpoint
// (single-port mode, the default).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerShared(mux)
	s.registerUpload(mux)
	s.registerAdmin(mux)
	return corsMiddleware(mux)
}

// UploadHandler returns the handler for the public upload port in split-port
// mode (ADMIN_PORT set): upload endpoints plus shared assets/probes.
func (s *Server) UploadHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerShared(mux)
	s.registerUpload(mux)
	return corsMiddleware(mux)
}

// AdminHandler returns the handler for the admin port in split-port mode:
// login and invite-management endpoints plus shared assets/probes.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerShared(mux)
	s.registerAdmin(mux)
	return corsMiddleware(mux)
}

// corsMiddleware mirrors the Python allow-all CORS configuration. The
// frontend is same-origin, so this only matters for external API consumers.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "*")
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- Shared helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func errJSON(w http.ResponseWriter, status int, code any) {
	writeJSON(w, status, map[string]any{"error": code})
}

// decodeJSONBody parses the request body into a generic map; returns nil on failure.
func decodeJSONBody(r *http.Request) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil
	}
	return body
}

// sendProgress pushes a progress update over WebSocket for one queue item.
// Field names and status values are the frontend's protocol; do not rename.
func (s *Server) sendProgress(sessionID, itemID, status string, progress int, message any, responseID any) {
	s.hub.Send(sessionID, map[string]any{
		"item_id":    itemID,
		"status":     status,
		"progress":   progress,
		"message":    message,
		"responseId": responseID,
	})
}

// clientIP returns the peer address (falling back to X-Forwarded-For).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		host = r.Header.Get("X-Forwarded-For")
	}
	return host
}

// strOr returns *p, or "" when p is nil.
func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// truthy approximates Python bool() for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	default:
		return true
	}
}

// ---------- Time helpers ----------

const naiveISOLayout = "2006-01-02T15:04:05"

// immichISO formats a timestamp as timezone-qualified ISO 8601 (UTC) for Immich.
func immichISO(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// nowNaiveUTC returns the current UTC time formatted the way the Python
// version stored expiry/creation timestamps (naive ISO, no zone suffix).
func nowNaiveUTC() string {
	return time.Now().UTC().Format(naiveISOLayout)
}

// parseISOFlexible parses the ISO-8601 variants found in existing databases
// (naive UTC strings) as well as offset-qualified ones. All naive values are
// interpreted as UTC, matching how they were written.
func parseISOFlexible(s string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999",
		naiveISOLayout,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// isExpired reports whether a stored expiry timestamp lies in the past.
// Unparseable values count as not expired (matching the Python try/except).
func isExpired(expiresAt *string) bool {
	if expiresAt == nil || *expiresAt == "" {
		return false
	}
	t, ok := parseISOFlexible(*expiresAt)
	if !ok {
		return false
	}
	return time.Now().UTC().After(t)
}

// ---------- Album cache & helpers ----------

// ResetAlbumCache invalidates the cached default album id.
func (s *Server) ResetAlbumCache() {
	s.albumMu.Lock()
	s.albumID = ""
	s.albumMu.Unlock()
}

// getOrCreateAlbum resolves an album name to an id, creating the album when it
// does not exist. With nameOverride == nil the env-configured default album is
// used and its id cached; overrides bypass the cache entirely.
func (s *Server) getOrCreateAlbum(accessToken string, nameOverride *string) *string {
	albumName := s.cfg.AlbumName
	isOverride := nameOverride != nil
	if isOverride {
		albumName = *nameOverride
	}
	if albumName == "" {
		return nil
	}
	if !isOverride {
		s.albumMu.Lock()
		cached := s.albumID
		s.albumMu.Unlock()
		if cached != "" {
			return &cached
		}
	}

	id, err := s.immich.FindAlbumIDByName(accessToken, albumName)
	if err != nil {
		slog.Error("error managing album", "err", err)
		return nil
	}
	if id != "" {
		if !isOverride {
			s.albumMu.Lock()
			s.albumID = id
			s.albumMu.Unlock()
			slog.Info("found existing album", "name", albumName, "id", id)
		}
		return &id
	}

	status, body, err := s.immich.CreateAlbum(accessToken, albumName, "Auto-created album for Immich Drop uploads")
	if err != nil {
		slog.Error("error managing album", "err", err)
		return nil
	}
	if status == http.StatusOK || status == http.StatusCreated {
		var data map[string]any
		_ = json.Unmarshal(body, &data)
		newID, _ := data["id"].(string)
		if newID == "" {
			return nil
		}
		if !isOverride {
			s.albumMu.Lock()
			s.albumID = newID
			s.albumMu.Unlock()
		}
		slog.Info("created new album", "name", albumName, "id", newID)
		return &newID
	}
	slog.Warn("failed to create album", "status", status, "body", strings.TrimSpace(string(body)))
	return nil
}

// addAssetToAlbum adds an asset to the configured (or overridden) album.
func (s *Server) addAssetToAlbum(accessToken, assetID string, albumIDOverride, albumNameOverride *string) bool {
	albumID := strOr(albumIDOverride)
	if albumID == "" {
		albumID = strOr(s.getOrCreateAlbum(accessToken, albumNameOverride))
	}
	if albumID == "" || assetID == "" {
		return false
	}
	return s.immich.AddAssetToAlbum(accessToken, albumID, assetID)
}
