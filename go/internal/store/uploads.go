package store

// Dedupe-cache queries over the uploads table.

// HasChecksum reports whether this checksum was uploaded before by this service.
func (s *Store) HasChecksum(checksum string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM uploads WHERE checksum = ?", checksum).Scan(&one)
	if err == nil {
		return true, nil
	}
	if isNoRows(err) {
		return false, nil
	}
	return false, err
}

// HasDeviceAsset reports whether a deviceAssetId was uploaded by this service before.
func (s *Store) HasDeviceAsset(deviceAssetID string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM uploads WHERE device_asset_id = ?", deviceAssetID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if isNoRows(err) {
		return false, nil
	}
	return false, err
}

// InsertUpload records a newly-uploaded asset in the local cache (ignoring duplicates).
func (s *Store) InsertUpload(checksum, filename string, size int64, deviceAssetID string, immichAssetID *string, createdAt string) error {
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO uploads (checksum, filename, size, device_asset_id, immich_asset_id, created_at) VALUES (?,?,?,?,?,?)",
		checksum, filename, size, deviceAssetID, immichAssetID, createdAt,
	)
	return err
}
