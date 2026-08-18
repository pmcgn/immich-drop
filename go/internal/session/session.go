// Package session implements the signed-cookie session used for the Immich
// access token and invite password unlocks. It is the Go equivalent of
// Starlette's SessionMiddleware: a JSON payload signed with HMAC-SHA256.
// Existing Python cookies do not survive the migration (they fail the
// signature check and are treated as an empty session), which is acceptable
// per docs/rewrite-notes.md.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	cookieName = "session"
	// Same lifetime as Starlette's SessionMiddleware default.
	maxAge = 14 * 24 * 60 * 60
)

// Data is the session payload. Only these keys are ever stored.
type Data struct {
	AccessToken string          `json:"accessToken,omitempty"`
	UserEmail   string          `json:"userEmail,omitempty"`
	UserID      string          `json:"userId,omitempty"`
	Name        string          `json:"name,omitempty"`
	IsAdmin     bool            `json:"isAdmin,omitempty"`
	InviteAuth  map[string]bool `json:"inviteAuth,omitempty"`
}

// Manager signs and verifies session cookies.
type Manager struct {
	secret []byte
}

func NewManager(secret string) *Manager {
	return &Manager{secret: []byte(secret)}
}

func (m *Manager) sign(payload []byte) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Get returns the session for the request; an invalid or absent cookie yields
// an empty session.
func (m *Manager) Get(r *http.Request) *Data {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return &Data{}
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return &Data{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return &Data{}
	}
	if !hmac.Equal([]byte(m.sign(payload)), []byte(parts[1])) {
		return &Data{}
	}
	var d Data
	if err := json.Unmarshal(payload, &d); err != nil {
		return &Data{}
	}
	return &d
}

// Save writes the session cookie onto the response.
func (m *Manager) Save(w http.ResponseWriter, d *Data) {
	payload, err := json.Marshal(d)
	if err != nil {
		return
	}
	value := base64.RawURLEncoding.EncodeToString(payload) + "." + m.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear removes the session cookie.
func (m *Manager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
