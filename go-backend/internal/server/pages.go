package server

import (
	"net/http"
	"os"
	"path/filepath"

	"immich-drop/internal/validate"
)

func (s *Server) serveFrontendFile(w http.ResponseWriter, r *http.Request, name string) {
	http.ServeFile(w, r, filepath.Join(s.cfg.FrontendDir, name))
}

// handleIndex serves the SPA, or redirects to login when the public upload
// page is disabled. In split-port mode /login lives on the admin port, so the
// redirect would just 404 — return 404 directly instead of leaking that an
// admin UI exists elsewhere.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PublicUploadPageEnabled {
		if s.cfg.SplitPorts() {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	s.serveFrontendFile(w, r, "index.html")
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.serveFrontendFile(w, r, "login.html")
}

// handleAdminRoot serves "/" on the admin port in split-port mode, where
// registerUpload (and its "/" -> index.html/login handling) isn't mounted.
// Without this, hitting the admin port's root 404s instead of landing on
// login/menu.
func (s *Server) handleAdminRoot(w http.ResponseWriter, r *http.Request) {
	if s.sessions.Get(r).AccessToken != "" {
		http.Redirect(w, r, "/menu", http.StatusTemporaryRedirect)
		return
	}
	http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
}

// handleMenuPage serves the invite-management page; requires a login session.
func (s *Server) handleMenuPage(w http.ResponseWriter, r *http.Request) {
	if s.sessions.Get(r).AccessToken == "" {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	s.serveFrontendFile(w, r, "menu.html")
}

// handleLogoutRedirect clears the session and returns to the login page.
func (s *Server) handleLogoutRedirect(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w)
	http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
}

// handleServiceWorker serves the admin PWA's service worker from the root
// path (not /static/) so its default scope covers /login and /menu.
func (s *Server) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	s.serveFrontendFile(w, r, "sw-admin.js")
}

// handleFavicon serves /static/favicon.png if present (avoids 404 noise).
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.cfg.FrontendDir, "favicon.png")
	data, err := os.ReadFile(path)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data)
}

// handleInvitePage serves the invite upload page for a valid-looking token.
func (s *Server) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	if !validate.IsValidInviteToken(r.PathValue("token")) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	s.serveFrontendFile(w, r, "invite.html")
}

// handleHealth is the liveness probe (Docker HEALTHCHECK via the binary's
// -healthcheck flag). It only asserts the HTTP layer is serving; Immich
// reachability is deliberately not part of liveness (see /api/ping for that).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePing is the Immich connectivity test behind the "Test connection"
// button on the login/menu pages.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	var albumName any
	if s.cfg.AlbumName != "" {
		albumName = s.cfg.AlbumName
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         s.immich.Ping(),
		"base_url":   s.cfg.NormalizedBaseURL(),
		"album_name": albumName,
	})
}

// handleConfig exposes minimal public configuration flags for the frontend.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"public_upload_page_enabled": s.cfg.PublicUploadPageEnabled,
		"chunked_uploads_enabled":    s.cfg.ChunkedUploadsEnabled,
		"chunk_size_mb":              s.cfg.ChunkSizeMB,
	})
}

// handleAlbumReset is an explicit trigger from the UI to clear the cached album id.
func (s *Server) handleAlbumReset(w http.ResponseWriter, r *http.Request) {
	s.ResetAlbumCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
