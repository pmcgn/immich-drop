package server

import (
	"log/slog"
	"net/http"

	"immich-drop/internal/validate"
)

// handleAlbumsList proxies the Immich album list for the logged-in user.
// Requires a login session: without this gate the API-key fallback would let
// anonymous visitors enumerate album names.
func (s *Server) handleAlbumsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r); !ok {
		return
	}
	sess := s.sessions.Get(r)
	status, body, err := s.immich.GetAlbums(sess.AccessToken)
	if err != nil {
		slog.Error("albums request failed", "err", err)
		errJSON(w, http.StatusBadGateway, "request_failed")
		return
	}
	switch {
	case status == http.StatusOK:
		writeJSONRaw(w, http.StatusOK, body)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		slog.Warn("album list not allowed", "status", status, "body", string(body))
		errJSON(w, http.StatusForbidden, "forbidden")
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "unexpected_status", "status": status})
	}
}

// handleAlbumsCreate creates a new Immich album on behalf of the user.
// Requires a login session so anonymous visitors cannot flood Immich with
// albums via the API-key fallback. (Albums tied to invite uploads are created
// server-side in the upload pipeline and are unaffected by this gate.)
func (s *Server) handleAlbumsCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r); !ok {
		return
	}
	body := decodeJSONBody(r)
	if body == nil {
		errJSON(w, http.StatusBadRequest, "invalid_json")
		return
	}
	name := validate.CleanText(body["name"], validate.MaxNameLen)
	if name == nil || *name == "" {
		errJSON(w, http.StatusBadRequest, "missing_name")
		return
	}
	sess := s.sessions.Get(r)
	status, respBody, err := s.immich.CreateAlbum(sess.AccessToken, *name, "")
	if err != nil {
		slog.Error("create album failed", "err", err)
		errJSON(w, http.StatusBadGateway, "request_failed")
		return
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		writeJSONRaw(w, http.StatusCreated, respBody)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		slog.Warn("create album forbidden", "status", status, "body", string(respBody))
		errJSON(w, http.StatusForbidden, "forbidden")
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "unexpected_status", "status": status, "body": string(respBody)})
	}
}
