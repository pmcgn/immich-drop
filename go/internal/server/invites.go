package server

// Invite links: creation, listing, editing, bulk enable/disable, deletion,
// per-invite upload logs, public invite info, and password authorization.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"immich-drop/internal/passhash"
	"immich-drop/internal/validate"
)

// requireAuth returns the user id when a login session exists, otherwise
// writes 401 and returns ok=false.
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (userID string, ok bool) {
	sess := s.sessions.Get(r)
	if sess.AccessToken == "" {
		errJSON(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return sess.UserID, true
}

// pyStr mimics Python's str(value or "") for the loosely-typed PATCH fields.
func pyStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if !truthy(v) {
		return ""
	}
	return fmt.Sprint(v)
}

func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// handleInvitesCreate creates an invite link with optional album, expiry,
// usage limit, and password.
func (s *Server) handleInvitesCreate(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.Get(r)
	if sess.AccessToken == "" {
		errJSON(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body := decodeJSONBody(r)
	if body == nil {
		errJSON(w, http.StatusBadRequest, "invalid_json")
		return
	}

	// albumId: optional, must look like an Immich album id when present.
	albumID := ""
	if v := body["albumId"]; v != nil {
		str, isStr := v.(string)
		if !isStr || !validate.IsValidAlbumID(str) {
			errJSON(w, http.StatusBadRequest, "invalid_album_id")
			return
		}
		albumID = str
	}
	// albumName: optional; distinguish "absent/null" from "empty string".
	var albumName *string
	if v := body["albumName"]; v != nil {
		albumName = validate.CleanText(v, validate.MaxNameLen)
		if albumName == nil {
			errJSON(w, http.StatusBadRequest, "invalid_album_name")
			return
		}
	}
	// password: optional string up to MaxPasswordLen.
	password := ""
	if v := body["password"]; v != nil {
		str, isStr := v.(string)
		if !isStr || len([]rune(str)) > validate.MaxPasswordLen {
			errJSON(w, http.StatusBadRequest, "invalid_password")
			return
		}
		password = str
	}
	// maxUses: default 1; -1 means unlimited; out-of-range values fall back to 1.
	maxUses := int64(1)
	if v, present := body["maxUses"]; present {
		if n, ok := validate.ToInt64(v); ok {
			maxUses = n
		}
	}
	if maxUses < -1 || maxUses > 1_000_000 {
		maxUses = 1
	}

	// An explicitly empty albumName falls back to the env default album; a
	// missing albumName means "no album association".
	if albumName != nil && *albumName == "" && s.cfg.AlbumName != "" && albumID == "" {
		name := s.cfg.AlbumName
		albumName = &name
	}
	// Resolve (or create) the album now so the invite is fixed to an id.
	var resolvedAlbumID *string
	if albumID == "" && albumName != nil && *albumName != "" {
		resolvedAlbumID = s.getOrCreateAlbum(sess.AccessToken, albumName)
	} else if albumID != "" {
		resolvedAlbumID = &albumID
	}

	// Expiry from expiresDays (0..3650), stored as naive UTC ISO.
	var expiresAt *string
	if v := body["expiresDays"]; v != nil {
		if days, ok := validate.ToInt64(v); ok && days >= 0 && days <= 3650 {
			e := time.Now().UTC().AddDate(0, 0, int(days)).Format(naiveISOLayout)
			expiresAt = &e
		}
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	token := hex.EncodeToString(tokenBytes)

	var pwHash *string
	if strings.TrimSpace(password) != "" {
		h, err := passhash.Hash(password)
		if err != nil {
			slog.Error("password hashing failed", "err", err)
			errJSON(w, http.StatusInternalServerError, "db_error")
			return
		}
		pwHash = &h
	}

	// Friendly default link name: album + creation timestamp.
	nameForTag := "NoAlbum"
	if albumName != nil && *albumName != "" {
		nameForTag = *albumName
	}
	linkName := fmt.Sprintf("%s-%s", nameForTag, time.Now().UTC().Format("20060102-1504"))

	if err := s.store.CreateInvite(token, resolvedAlbumID, albumName, maxUses, expiresAt, pwHash,
		sess.UserID, sess.UserEmail, sess.Name, linkName); err != nil {
		slog.Error("failed to create invite", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}

	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"token":       token,
		"url":         "/invite/" + token,
		"absoluteUrl": baseURL + "/invite/" + token,
		"albumId":     resolvedAlbumID,
		"albumName":   albumName,
		"maxUses":     maxUses,
		"expiresAt":   expiresAt,
		"name":        linkName,
	})
}

// handleInvitesList lists invites owned by the logged-in user with optional
// q/sort filters.
func (s *Server) handleInvitesList(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	q := capRunes(strings.TrimSpace(r.URL.Query().Get("q")), 100)
	sort := strings.TrimSpace(r.URL.Query().Get("sort"))
	if sort == "" {
		sort = "-created"
	}
	rows, err := s.store.ListInvites(userID, q, sort)
	if err != nil {
		slog.Error("list invites failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, it := range rows {
		maxUses := int64(-1)
		if it.MaxUses != nil {
			maxUses = *it.MaxUses
		}
		var remaining *int64
		if maxUses >= 0 {
			rem := maxUses - it.UsedCount
			remaining = &rem
		}
		expired := isExpired(it.ExpiresAt)

		active := true
		var inactiveReason *string
		setReason := func(reason string) { inactiveReason = &reason }
		if (maxUses == 1 && it.Claimed) || (remaining != nil && *remaining <= 0) {
			active = false
			if maxUses == 1 {
				setReason("claimed")
			} else {
				setReason("exhausted")
			}
		}
		if expired {
			active = false
			if inactiveReason == nil {
				setReason("expired")
			}
		}
		if it.Disabled {
			active = false
			setReason("disabled")
		}
		items = append(items, map[string]any{
			"token":          it.Token,
			"name":           it.Name,
			"albumId":        it.AlbumID,
			"albumName":      it.AlbumName,
			"maxUses":        it.MaxUses,
			"used":           it.UsedCount,
			"remaining":      remaining,
			"expiresAt":      it.ExpiresAt,
			"active":         active,
			"inactiveReason": inactiveReason,
			"createdAt":      it.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleInviteUpdate patches invite fields: name, disabled, maxUses,
// expiresAt/expiresDays, password, resetUsage.
func (s *Server) handleInviteUpdate(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	token := r.PathValue("token")
	if !validate.IsValidInviteToken(token) {
		errJSON(w, http.StatusBadRequest, "invalid_token")
		return
	}
	body := decodeJSONBody(r)
	if body == nil {
		body = map[string]any{}
	}

	var setClauses []string
	var params []any
	if v, present := body["name"]; present {
		setClauses = append(setClauses, "name = ?")
		params = append(params, capRunes(strings.TrimSpace(pyStr(v)), validate.MaxNameLen))
	}
	if v, present := body["disabled"]; present {
		disabled := 0
		if truthy(v) {
			disabled = 1
		}
		setClauses = append(setClauses, "disabled = ?")
		params = append(params, disabled)
	}
	if v, present := body["maxUses"]; present {
		mu, okInt := validate.ToInt64(v)
		if !okInt || mu < -1 || mu > 1_000_000 {
			mu = 1
		}
		setClauses = append(setClauses, "max_uses = ?")
		params = append(params, mu)
	}
	_, hasExpiresAt := body["expiresAt"]
	_, hasExpiresDays := body["expiresDays"]
	if hasExpiresAt || hasExpiresDays {
		var expiresAt *string
		if truthy(body["expiresAt"]) {
			if str, isStr := body["expiresAt"].(string); isStr {
				if t, parsed := parseISOFlexible(str); parsed {
					// Stored naive-UTC for consistency with existing rows.
					e := t.UTC().Format(naiveISOLayout)
					expiresAt = &e
				}
			}
		} else if days, okInt := validate.ToInt64(body["expiresDays"]); okInt && days >= 0 && days <= 3650 {
			e := time.Now().UTC().AddDate(0, 0, int(days)).Format(naiveISOLayout)
			expiresAt = &e
		}
		setClauses = append(setClauses, "expires_at = ?")
		params = append(params, expiresAt)
	}
	if v, present := body["password"]; present {
		pw := capRunes(strings.TrimSpace(pyStr(v)), validate.MaxPasswordLen)
		if pw != "" {
			h, err := passhash.Hash(pw)
			if err != nil {
				errJSON(w, http.StatusInternalServerError, "db_error")
				return
			}
			setClauses = append(setClauses, "password_hash = ?")
			params = append(params, h)
		} else {
			setClauses = append(setClauses, "password_hash = NULL")
		}
	}
	resetUsage := truthy(body["resetUsage"])

	updated, err := s.store.UpdateInvite(token, userID, setClauses, params, resetUsage)
	if err != nil {
		slog.Error("invite update failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	if updated == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "updated": 0})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated})
}

// collectTokens filters a JSON token list down to valid invite tokens.
func collectTokens(v any) []string {
	list, _ := v.([]any)
	var tokens []string
	for _, item := range list {
		if t, ok := item.(string); ok && validate.IsValidInviteToken(t) {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// handleInvitesBulk enables/disables invites owned by the current user.
func (s *Server) handleInvitesBulk(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	body := decodeJSONBody(r)
	if body == nil {
		body = map[string]any{}
	}
	tokens := collectTokens(body["tokens"])
	action := strings.TrimSpace(strings.ToLower(pyStr(body["action"])))
	if action == "" {
		action = "disable"
	}
	if len(tokens) == 0 || len(tokens) > validate.MaxTokensPerReq {
		errJSON(w, http.StatusBadRequest, "missing_tokens")
		return
	}
	if action != "disable" && action != "enable" {
		errJSON(w, http.StatusBadRequest, "invalid_action")
		return
	}
	val := 0
	if action == "disable" {
		val = 1
	}
	changed, err := s.store.SetInvitesDisabled(userID, tokens, val)
	if err != nil {
		slog.Error("bulk update failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": changed})
}

// handleInvitesDelete hard-deletes invites owned by the current user together
// with their upload logs.
func (s *Server) handleInvitesDelete(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	body := decodeJSONBody(r)
	if body == nil {
		body = map[string]any{}
	}
	tokens := collectTokens(body["tokens"])
	if len(tokens) == 0 || len(tokens) > validate.MaxTokensPerReq {
		errJSON(w, http.StatusBadRequest, "missing_tokens")
		return
	}
	deleted, err := s.store.DeleteInvites(userID, tokens)
	if err != nil {
		slog.Error("bulk delete failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

// handleInviteUploads returns the audit log for one invite (owner-only).
func (s *Server) handleInviteUploads(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	token := r.PathValue("token")
	if !validate.IsValidInviteToken(token) {
		errJSON(w, http.StatusNotFound, "not_found")
		return
	}
	owned, err := s.store.InviteOwnedBy(token, userID)
	if err != nil {
		slog.Error("fetch uploads failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	if !owned {
		errJSON(w, http.StatusForbidden, "forbidden")
		return
	}
	events, err := s.store.ListUploadEvents(token)
	if err != nil {
		slog.Error("fetch uploads failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	items := make([]map[string]any, 0, len(events))
	for _, e := range events {
		items = append(items, map[string]any{
			"uploadedAt":  e.UploadedAt,
			"ip":          e.IP,
			"userAgent":   e.UserAgent,
			"fingerprint": e.Fingerprint,
			"filename":    e.Filename,
			"size":        e.Size,
			"checksum":    e.Checksum,
			"assetId":     e.ImmichAssetID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleInviteInfo returns the public state of an invite (used by the invite
// landing page; no login required).
func (s *Server) handleInviteInfo(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !validate.IsValidInviteToken(token) {
		errJSON(w, http.StatusNotFound, "not_found")
		return
	}
	inv, err := s.store.GetInvite(token)
	if err != nil {
		slog.Error("invite info error", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	if inv == nil {
		errJSON(w, http.StatusNotFound, "not_found")
		return
	}

	var remaining *int64
	if inv.MaxUses != nil && *inv.MaxUses >= 0 {
		rem := *inv.MaxUses - inv.UsedCount
		remaining = &rem
	}
	oneTime := inv.MaxUses != nil && *inv.MaxUses == 1
	expired := isExpired(inv.ExpiresAt)

	deactivated := false
	var reason *string
	setReason := func(s string) { reason = &s }
	if oneTime && inv.Claimed {
		deactivated = true
		setReason("claimed")
	} else if remaining != nil && *remaining <= 0 {
		deactivated = true
		setReason("exhausted")
	}
	if expired {
		deactivated = true
		if reason == nil {
			setReason("expired")
		}
	}
	if inv.Disabled {
		deactivated = true
		setReason("disabled")
	}
	active := !deactivated

	var inactiveReason any
	if !active {
		if reason != nil {
			inactiveReason = *reason
		} else {
			inactiveReason = "inactive"
		}
	}
	sess := s.sessions.Get(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":            token,
		"albumId":          inv.AlbumID,
		"albumName":        inv.AlbumName,
		"name":             inv.Name,
		"maxUses":          inv.MaxUses,
		"used":             inv.UsedCount,
		"remaining":        remaining,
		"expiresAt":        inv.ExpiresAt,
		"oneTime":          oneTime,
		"claimed":          inv.Claimed,
		"claimedAt":        inv.ClaimedAt,
		"expired":          expired,
		"active":           active,
		"inactiveReason":   inactiveReason,
		"passwordRequired": strOr(inv.PasswordHash) != "",
		"authorized":       sess.InviteAuth[token],
	})
}

// handleInviteAuth validates a password for an invite and marks the session
// authorized when it matches.
func (s *Server) handleInviteAuth(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !validate.IsValidInviteToken(token) {
		errJSON(w, http.StatusNotFound, "not_found")
		return
	}
	body := decodeJSONBody(r) // nil (invalid JSON) is tolerated, like Python
	var provided string
	if body != nil {
		if v := body["password"]; v != nil {
			str, isStr := v.(string)
			if !isStr || len([]rune(str)) > validate.MaxPasswordLen {
				errJSON(w, http.StatusForbidden, "invalid_password")
				return
			}
			provided = str
		}
	}
	inv, err := s.store.GetInvite(token)
	if err != nil {
		slog.Error("invite auth lookup error", "err", err)
		errJSON(w, http.StatusInternalServerError, "db_error")
		return
	}
	if inv == nil {
		errJSON(w, http.StatusNotFound, "not_found")
		return
	}

	authorize := func() {
		sess := s.sessions.Get(r)
		if sess.InviteAuth == nil {
			sess.InviteAuth = map[string]bool{}
		}
		sess.InviteAuth[token] = true
		s.sessions.Save(w, sess)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authorized": true})
	}
	if strOr(inv.PasswordHash) == "" {
		// No password required; mark authorized to simplify the client flow.
		authorize()
		return
	}
	if !passhash.Verify(*inv.PasswordHash, provided) {
		errJSON(w, http.StatusForbidden, "invalid_password")
		return
	}
	authorize()
}
