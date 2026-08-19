package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Invite is one row of the invites table.
type Invite struct {
	Token            string
	AlbumID          *string
	AlbumName        *string
	Name             *string
	MaxUses          *int64
	UsedCount        int64
	ExpiresAt        *string
	Claimed          bool
	ClaimedAt        *string
	ClaimedBySession *string
	PasswordHash     *string
	Disabled         bool
	CreatedAt        *string
}

// MaxUsesOrDefault mirrors the Python "int(max_uses) if not None else -1" coercion.
func (i *Invite) MaxUsesOrDefault() int64 {
	if i.MaxUses == nil {
		return -1
	}
	return *i.MaxUses
}

// GetInvite returns the invite for token, or nil when it does not exist.
func (s *Store) GetInvite(token string) (*Invite, error) {
	var inv Invite
	err := s.db.QueryRow(`
		SELECT token, album_id, album_name, name, max_uses, COALESCE(used_count,0),
		       expires_at, COALESCE(claimed,0), claimed_at, claimed_by_session,
		       password_hash, COALESCE(disabled,0), created_at
		FROM invites WHERE token = ?`, token).
		Scan(&inv.Token, &inv.AlbumID, &inv.AlbumName, &inv.Name, &inv.MaxUses, &inv.UsedCount,
			&inv.ExpiresAt, &inv.Claimed, &inv.ClaimedAt, &inv.ClaimedBySession,
			&inv.PasswordHash, &inv.Disabled, &inv.CreatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// ClaimInvite atomically claims a one-time invite for sessionID. Returns true
// when this call performed the claim (false when it was already claimed).
func (s *Store) ClaimInvite(token, sessionID string) (bool, error) {
	res, err := s.db.Exec(
		"UPDATE invites SET claimed = 1, claimed_at = CURRENT_TIMESTAMP, claimed_by_session = ? WHERE token = ? AND (claimed IS NULL OR claimed = 0)",
		sessionID, token,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// InviteClaimOwner returns claimed_by_session for token (nil if unset/missing).
func (s *Store) InviteClaimOwner(token string) (*string, error) {
	var owner *string
	err := s.db.QueryRow("SELECT claimed_by_session FROM invites WHERE token = ?", token).Scan(&owner)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return owner, nil
}

// IncrementInviteUsage bumps used_count after a successful upload. One-time
// invites are pinned to used_count = 1; multi-use invites increment per asset.
func (s *Store) IncrementInviteUsage(token string) error {
	var maxUses *int64
	err := s.db.QueryRow("SELECT max_uses FROM invites WHERE token = ?", token).Scan(&maxUses)
	if err != nil && !isNoRows(err) {
		return err
	}
	if maxUses != nil && *maxUses == 1 {
		_, err = s.db.Exec("UPDATE invites SET used_count = 1 WHERE token = ?", token)
	} else {
		_, err = s.db.Exec("UPDATE invites SET used_count = used_count + 1 WHERE token = ?", token)
	}
	return err
}

// CreateInvite inserts a new invite row.
func (s *Store) CreateInvite(token string, albumID, albumName *string, maxUses int64, expiresAt, passwordHash *string, ownerUserID, ownerEmail, ownerName, name string) error {
	_, err := s.db.Exec(
		"INSERT INTO invites (token, album_id, album_name, max_uses, expires_at, password_hash, owner_user_id, owner_email, owner_name, name) VALUES (?,?,?,?,?,?,?,?,?,?)",
		token, albumID, albumName, maxUses, expiresAt, passwordHash, ownerUserID, ownerEmail, ownerName, name,
	)
	return err
}

// InviteListItem is one row returned by ListInvites.
type InviteListItem struct {
	Token     string
	Name      *string
	AlbumID   *string
	AlbumName *string
	MaxUses   *int64
	UsedCount int64
	ExpiresAt *string
	Claimed   bool
	Disabled  bool
	CreatedAt *string
}

// sortSQL maps the frontend sort tokens to ORDER BY clauses (default -created).
func sortSQL(sort string) string {
	switch sort {
	case "created", "+created":
		return "created_at ASC"
	case "expires", "+expires":
		return "expires_at ASC"
	case "-expires":
		return "expires_at DESC"
	case "name", "+name":
		return "name ASC"
	case "-name":
		return "name DESC"
	default:
		return "created_at DESC"
	}
}

// ListInvites returns invites owned by ownerUserID, optionally filtered by q
// (substring on name, album name, or token) and sorted by the given token.
func (s *Store) ListInvites(ownerUserID, q, sort string) ([]InviteListItem, error) {
	base := `SELECT token, name, album_id, album_name, max_uses, COALESCE(used_count,0), expires_at,
	                COALESCE(claimed,0), COALESCE(disabled,0), created_at
	         FROM invites WHERE owner_user_id = ?`
	args := []any{ownerUserID}
	if q != "" {
		base += ` AND (COALESCE(name,'') LIKE ? OR COALESCE(album_name,'') LIKE ? OR token LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	base += " ORDER BY " + sortSQL(sort)
	rows, err := s.db.Query(base, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []InviteListItem
	for rows.Next() {
		var it InviteListItem
		if err := rows.Scan(&it.Token, &it.Name, &it.AlbumID, &it.AlbumName, &it.MaxUses,
			&it.UsedCount, &it.ExpiresAt, &it.Claimed, &it.Disabled, &it.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// UpdateInvite applies the given SET clauses (owner-scoped) and optionally
// resets usage counters. Returns the total number of changed rows.
func (s *Store) UpdateInvite(token, ownerUserID string, setClauses []string, params []any, resetUsage bool) (int64, error) {
	if len(setClauses) == 0 {
		return 0, nil
	}
	query := fmt.Sprintf("UPDATE invites SET %s WHERE token = ? AND owner_user_id = ?", strings.Join(setClauses, ", "))
	res, err := s.db.Exec(query, append(params, token, ownerUserID)...)
	if err != nil {
		return 0, err
	}
	changed, _ := res.RowsAffected()
	if resetUsage {
		res2, err := s.db.Exec(
			"UPDATE invites SET used_count = 0, claimed = 0, claimed_at = NULL, claimed_by_session = NULL WHERE token = ? AND owner_user_id = ?",
			token, ownerUserID,
		)
		if err != nil {
			return changed, err
		}
		n2, _ := res2.RowsAffected()
		changed += n2
	}
	return changed, nil
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

// SetInvitesDisabled bulk enables/disables invites owned by ownerUserID.
func (s *Store) SetInvitesDisabled(ownerUserID string, tokens []string, disabled int) (int64, error) {
	args := []any{disabled, ownerUserID}
	for _, t := range tokens {
		args = append(args, t)
	}
	res, err := s.db.Exec(
		fmt.Sprintf("UPDATE invites SET disabled = ? WHERE owner_user_id = ? AND token IN (%s)", placeholders(len(tokens))),
		args...,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteInvites hard-deletes invites owned by ownerUserID together with their
// upload events. Both deletes are owner-scoped (the Python version scoped only
// the invites delete). Returns total rows removed.
func (s *Store) DeleteInvites(ownerUserID string, tokens []string) (int64, error) {
	ph := placeholders(len(tokens))
	args := []any{ownerUserID}
	for _, t := range tokens {
		args = append(args, t)
	}
	res1, err := s.db.Exec(
		fmt.Sprintf("DELETE FROM upload_events WHERE token IN (SELECT token FROM invites WHERE owner_user_id = ? AND token IN (%s))", ph),
		args...,
	)
	if err != nil {
		return 0, err
	}
	n1, _ := res1.RowsAffected()
	res2, err := s.db.Exec(
		fmt.Sprintf("DELETE FROM invites WHERE owner_user_id = ? AND token IN (%s)", ph),
		args...,
	)
	if err != nil {
		return n1, err
	}
	n2, _ := res2.RowsAffected()
	return n1 + n2, nil
}

// InviteOwnedBy reports whether token exists and belongs to ownerUserID.
func (s *Store) InviteOwnedBy(token, ownerUserID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM invites WHERE token = ? AND owner_user_id = ?", token, ownerUserID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if isNoRows(err) {
		return false, nil
	}
	return false, err
}
