package server

// Chunked uploads: the browser splits large files to stay under proxy body
// limits; parts are spooled to disk and reassembled on complete, then pushed
// through the same pipeline as whole-file uploads.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"immich-drop/internal/validate"
)

// chunkDir returns the spool directory for one upload. The ids are already
// validated by callers; they are re-checked here so no caller can ever place
// path separators or ".." segments under the chunk root.
func (s *Server) chunkDir(sessionID, itemID string) (string, error) {
	if !validate.IsValidClientID(sessionID) || !validate.IsValidClientID(itemID) {
		return "", errors.New("invalid session/item id")
	}
	return filepath.Join(s.cfg.ChunkDir, sessionID, itemID), nil
}

func partPath(dir string, index int64) string {
	return filepath.Join(dir, fmt.Sprintf("part_%06d", index))
}

func readMeta(dir string) map[string]any {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return map[string]any{}
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil || meta == nil {
		return map[string]any{}
	}
	return meta
}

func writeMeta(dir string, meta map[string]any) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

// extractIDs validates item_id/session_id from a JSON body. Writes the error
// response and returns ok=false when they are missing or malformed.
func extractIDs(w http.ResponseWriter, body map[string]any) (sessionID, itemID string, ok bool) {
	itemID, _ = body["item_id"].(string)
	sessionID, _ = body["session_id"].(string)
	if itemID == "" || sessionID == "" {
		errJSON(w, http.StatusBadRequest, "missing_ids")
		return "", "", false
	}
	if !validate.IsValidClientID(sessionID) || !validate.IsValidClientID(itemID) {
		errJSON(w, http.StatusBadRequest, "invalid_ids")
		return "", "", false
	}
	return sessionID, itemID, true
}

// extractInviteToken validates an optional invite_token in a JSON body.
// Returns ok=false (with the response written) when present but malformed.
func extractInviteToken(w http.ResponseWriter, body map[string]any) (string, bool) {
	v, present := body["invite_token"]
	if !present || v == nil {
		return "", true
	}
	token, isStr := v.(string)
	if !isStr || !validate.IsValidInviteToken(token) {
		errJSON(w, http.StatusForbidden, "invalid_invite")
		return "", false
	}
	return token, true
}

// extractSHA1 validates an optional client-computed sha1 in a JSON body.
// Absent, null, or empty means "no verification"; a malformed value is
// rejected (ok=false, response written).
func extractSHA1(w http.ResponseWriter, body map[string]any) (string, bool) {
	v, present := body["sha1"]
	if !present || v == nil {
		return "", true
	}
	str, isStr := v.(string)
	if !isStr {
		errJSON(w, http.StatusBadRequest, "invalid_sha1")
		return "", false
	}
	str = strings.ToLower(strings.TrimSpace(str))
	if str == "" {
		return "", true
	}
	if !validate.IsValidSHA1Hex(str) {
		errJSON(w, http.StatusBadRequest, "invalid_sha1")
		return "", false
	}
	return str, true
}

// handleChunkInit creates the spool directory and records the file metadata.
func (s *Server) handleChunkInit(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONBody(r)
	if body == nil {
		errJSON(w, http.StatusBadRequest, "invalid_json")
		return
	}
	sessionID, itemID, ok := extractIDs(w, body)
	if !ok {
		return
	}
	inviteToken, ok := extractInviteToken(w, body)
	if !ok {
		return
	}
	// Optional whole-file SHA-1 computed by the client; verified after assembly.
	sha1Hex, ok := extractSHA1(w, body)
	if !ok {
		return
	}
	dir, err := s.chunkDir(sessionID, itemID)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid_ids")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("chunk init failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "init_failed")
		return
	}
	contentType := validate.CleanText(body["content_type"], 100)
	if contentType == nil || *contentType == "" {
		ct := "application/octet-stream"
		contentType = &ct
	}
	meta := map[string]any{
		"name":          validate.CleanText(body["name"], validate.MaxNameLen),
		"size":          validate.ParseNonNegInt(body["size"], 1<<40),
		"last_modified": validate.ParseEpochMS(body["last_modified"]),
		"invite_token":  nullable(inviteToken),
		"sha1":          nullable(sha1Hex),
		"content_type":  contentType,
		"created_at":    nowNaiveUTC(),
	}
	if err := writeMeta(dir, meta); err != nil {
		slog.Error("chunk init failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "init_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleChunk receives a single chunk and writes it into the spool directory.
func (s *Server) handleChunk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid_form")
		return
	}
	sessionID := r.FormValue("session_id")
	itemID := r.FormValue("item_id")
	if !validate.IsValidClientID(sessionID) || !validate.IsValidClientID(itemID) {
		errJSON(w, http.StatusBadRequest, "invalid_ids")
		return
	}
	inviteToken := ""
	if vals, ok := r.MultipartForm.Value["invite_token"]; ok && len(vals) > 0 {
		if !validate.IsValidInviteToken(vals[0]) {
			errJSON(w, http.StatusForbidden, "invalid_invite")
			return
		}
		inviteToken = vals[0]
	}
	chunkIndex, err1 := strconv.ParseInt(r.FormValue("chunk_index"), 10, 64)
	totalChunks, err2 := strconv.ParseInt(r.FormValue("total_chunks"), 10, 64)
	if err1 != nil || err2 != nil ||
		!(totalChunks > 0 && totalChunks <= validate.MaxChunks) ||
		!(chunkIndex >= 0 && chunkIndex < totalChunks) {
		errJSON(w, http.StatusBadRequest, "invalid_chunk_index")
		return
	}

	dir, err := s.chunkDir(sessionID, itemID)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid_ids")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("chunk write failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "chunk_write_failed")
		return
	}
	// Persist invite token and total in meta for consistency with init.
	meta := readMeta(dir)
	if inviteToken != "" {
		meta["invite_token"] = inviteToken
	}
	meta["total_chunks"] = totalChunks
	if err := writeMeta(dir, meta); err != nil {
		slog.Error("chunk write failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "chunk_write_failed")
		return
	}

	file, _, err := r.FormFile("chunk")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "missing_chunk")
		return
	}
	defer file.Close()
	// Stream the part straight to disk; chunk content is never held in memory.
	dst, err := os.Create(partPath(dir, chunkIndex))
	if err == nil {
		_, err = io.Copy(dst, file)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		slog.Error("chunk write failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "chunk_write_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleChunkComplete assembles all parts and runs the regular upload flow.
func (s *Server) handleChunkComplete(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONBody(r)
	if body == nil {
		errJSON(w, http.StatusBadRequest, "invalid_json")
		return
	}
	sessionID, itemID, ok := extractIDs(w, body)
	if !ok {
		return
	}
	// The invite token is taken from this request's body (matching the Python
	// behavior; a token sent only at init/chunk time is not used).
	inviteToken, ok := extractInviteToken(w, body)
	if !ok {
		return
	}
	// The expected SHA-1 may arrive in this body or (normally) in the meta
	// recorded at init; the body value wins so verification still works if the
	// init request was lost.
	expectedSHA1, ok := extractSHA1(w, body)
	if !ok {
		return
	}
	name := "upload.bin"
	if cleaned := validate.CleanText(body["name"], validate.MaxNameLen); cleaned != nil && *cleaned != "" {
		name = *cleaned
	}
	contentType := "application/octet-stream"
	if ct := validate.CleanText(body["content_type"], 100); ct != nil && *ct != "" {
		contentType = *ct
	}

	dir, err := s.chunkDir(sessionID, itemID)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid_ids")
		return
	}
	meta := readMeta(dir)
	if expectedSHA1 == "" {
		if fromMeta, _ := meta["sha1"].(string); validate.IsValidSHA1Hex(fromMeta) {
			expectedSHA1 = fromMeta
		}
	}
	totalAny := meta["total_chunks"]
	if totalAny == nil {
		totalAny = body["total_chunks"]
	}
	var totalChunks int64
	if n := validate.ParseNonNegInt(totalAny, validate.MaxChunks); n != nil {
		totalChunks = *n
	}
	if totalChunks <= 0 {
		errJSON(w, http.StatusBadRequest, "missing_total")
		return
	}

	// Verify all parts exist before taking an upload slot.
	for i := int64(0); i < totalChunks; i++ {
		if _, err := os.Stat(partPath(dir, i)); err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing_part", "index": i})
				return
			}
			slog.Error("assemble failed", "err", err)
			errJSON(w, http.StatusInternalServerError, "assemble_failed")
			return
		}
	}

	release := s.acquireUploadSlot()
	defer release()

	// Concatenate the parts into one on-disk spool file; nothing is buffered
	// in memory and the upload later streams straight from this file.
	spool, err := os.Create(filepath.Join(dir, "assembled.bin"))
	if err != nil {
		slog.Error("assemble failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "assemble_failed")
		return
	}
	defer removeSpool(spool, dir)
	var totalSize int64
	for i := int64(0); i < totalChunks; i++ {
		part, err := os.Open(partPath(dir, i))
		if err == nil {
			var n int64
			n, err = io.Copy(spool, part)
			totalSize += n
			_ = part.Close()
		}
		if err != nil {
			slog.Error("assemble failed", "err", err)
			errJSON(w, http.StatusInternalServerError, "assemble_failed")
			return
		}
	}
	// Cleanup parts promptly (best-effort); the spool file is removed by the
	// deferred removeSpool once the pipeline has finished with it.
	for i := int64(0); i < totalChunks; i++ {
		_ = os.Remove(partPath(dir, i))
	}
	_ = os.Remove(filepath.Join(dir, "meta.json"))

	s.runUploadPipeline(w, r, uploadInput{
		spool:        spool,
		size:         totalSize,
		fileName:     name,
		contentType:  contentType,
		lastModified: validate.ParseEpochMS(body["last_modified"]),
		sessionID:    sessionID,
		itemID:       itemID,
		inviteToken:  inviteToken,
		fingerprint:  cleanFingerprintAny(body["fingerprint"]),
		expectedSHA1: expectedSHA1,
	})
}

// cleanFingerprintAny applies CleanFingerprint to a decoded JSON value.
func cleanFingerprintAny(v any) string {
	s, _ := v.(string)
	return validate.CleanFingerprint(s)
}
