package server

import (
	"log/slog"
	"net/http"

	"immich-drop/internal/session"
)

// handleLogin authenticates against Immich with email/password and stores the
// access token in the cookie session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONBody(r)
	if body == nil {
		errJSON(w, http.StatusBadRequest, "invalid_json")
		return
	}
	email, emailOK := body["email"].(string)
	password, passOK := body["password"].(string)
	if !emailOK || !passOK || email == "" || password == "" {
		errJSON(w, http.StatusBadRequest, "missing_credentials")
		return
	}
	if len(email) > 320 || len(password) > 1024 {
		errJSON(w, http.StatusBadRequest, "invalid_credentials")
		return
	}

	status, data, err := s.immich.Login(email, password)
	if err != nil {
		slog.Error("login request failed", "err", err)
		errJSON(w, http.StatusBadGateway, "login_failed")
		return
	}
	if status != http.StatusOK && status != http.StatusCreated {
		slog.Warn("auth rejected", "status", status)
		errJSON(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token, _ := data["accessToken"].(string)
	if token == "" {
		slog.Warn("auth response missing accessToken")
		errJSON(w, http.StatusBadGateway, "invalid_response")
		return
	}

	userEmail, _ := data["userEmail"].(string)
	userID, _ := data["userId"].(string)
	name, _ := data["name"].(string)
	isAdmin, _ := data["isAdmin"].(bool)
	s.sessions.Save(w, &session.Data{
		AccessToken: token,
		UserEmail:   userEmail,
		UserID:      userID,
		Name:        name,
		IsAdmin:     isAdmin,
	})
	slog.Info("user logged in", "email", userEmail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"userEmail": data["userEmail"],
		"userId":    data["userId"],
		"name":      data["name"],
		"isAdmin":   data["isAdmin"],
	})
}

// handleLogout clears the session (API variant; the GET /logout page redirect
// lives in pages.go).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
