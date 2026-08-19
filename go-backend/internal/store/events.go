package store

// Audit log of uploads performed through invite links (and the public page).

// InsertUploadEvent records who uploaded what. Failures must never fail the
// upload itself; callers log and continue.
func (s *Store) InsertUploadEvent(token, ip, userAgent, fingerprint, filename string, size int64, checksum string, immichAssetID *string) error {
	_, err := s.db.Exec(
		"INSERT INTO upload_events (token, ip, user_agent, fingerprint, filename, size, checksum, immich_asset_id) VALUES (?,?,?,?,?,?,?,?)",
		token, ip, userAgent, fingerprint, filename, size, checksum, immichAssetID,
	)
	return err
}

// UploadEvent is one audit-log row for an invite token.
type UploadEvent struct {
	UploadedAt    *string
	IP            *string
	UserAgent     *string
	Fingerprint   *string
	Filename      *string
	Size          *int64
	Checksum      *string
	ImmichAssetID *string
}

// ListUploadEvents returns the newest 500 events for one invite token.
func (s *Store) ListUploadEvents(token string) ([]UploadEvent, error) {
	rows, err := s.db.Query(
		"SELECT uploaded_at, ip, user_agent, fingerprint, filename, size, checksum, immich_asset_id FROM upload_events WHERE token = ? ORDER BY uploaded_at DESC LIMIT 500",
		token,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []UploadEvent
	for rows.Next() {
		var e UploadEvent
		if err := rows.Scan(&e.UploadedAt, &e.IP, &e.UserAgent, &e.Fingerprint, &e.Filename, &e.Size, &e.Checksum, &e.ImmichAssetID); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
