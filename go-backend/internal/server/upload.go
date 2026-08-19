package server

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"immich-drop/internal/exifdate"
	"immich-drop/internal/immich"
	"immich-drop/internal/session"
	"immich-drop/internal/store"
	"immich-drop/internal/validate"
)

// uploadInput is one file to push through the shared upload pipeline. The
// Python version duplicated this flow between /api/upload and
// /api/upload/chunk/complete; here both handlers feed runUploadPipeline.
// File content lives in a disk spool file under CHUNK_DIR (never in memory);
// the pipeline seeks it as needed and the handler that created it removes it.
type uploadInput struct {
	spool        *os.File // read/write spool file holding the full content
	size         int64    // exact content length in bytes
	fileName     string   // original, unsanitized client filename
	contentType  string
	lastModified *int64 // epoch millis, already validated
	sessionID    string // already validated
	itemID       string // already validated
	inviteToken  string // "" when absent; format already validated
	fingerprint  string // already cleaned
	expectedSHA1 string // client-computed SHA-1 hex ("" = no verification), already validated
}

// removeSpool closes and deletes a spool file, then prunes its (now empty)
// item directory. Best-effort: the sweeper collects anything left behind.
func removeSpool(f *os.File, dir string) {
	_ = f.Close()
	_ = os.Remove(f.Name())
	_ = os.Remove(dir)
}

// handleUpload receives a whole file, checks duplicates, and forwards it to
// Immich while streaming progress over the session WebSocket.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
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
	if vals, ok := r.MultipartForm.Value["invite_token"]; ok && len(vals) > 0 && vals[0] != "" {
		if !validate.IsValidInviteToken(vals[0]) {
			s.sendProgress(sessionID, itemID, "error", 100, "Invalid invite token", nil)
			errJSON(w, http.StatusForbidden, "invalid_invite")
			return
		}
		inviteToken = vals[0]
	}
	// Optional client-computed SHA-1 for end-to-end integrity verification.
	expectedSHA1 := strings.ToLower(strings.TrimSpace(r.FormValue("sha1")))
	if expectedSHA1 != "" && !validate.IsValidSHA1Hex(expectedSHA1) {
		errJSON(w, http.StatusBadRequest, "invalid_sha1")
		return
	}
	// With the public upload page disabled, the invite token is the auth
	// credential; reject before spooling anything to disk.
	if !s.uploadAuthorized(w, r, sessionID, inviteToken) {
		return
	}

	// Take an upload slot before spooling the file to disk.
	release := s.acquireUploadSlot()
	defer release()

	file, header, err := r.FormFile("file")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "missing_file")
		return
	}
	defer file.Close()

	// Spool the content to disk under CHUNK_DIR (same {session}/{item} layout
	// as chunked uploads, so the TTL sweeper collects any leftovers).
	dir, err := s.chunkDir(sessionID, itemID)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid_ids")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("upload spool failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "spool_failed")
		return
	}
	spool, err := os.Create(filepath.Join(dir, "spool.bin"))
	if err != nil {
		slog.Error("upload spool failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "spool_failed")
		return
	}
	defer removeSpool(spool, dir)
	size, err := io.Copy(spool, file)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.runUploadPipeline(w, r, uploadInput{
		spool:        spool,
		size:         size,
		fileName:     header.Filename,
		contentType:  header.Header.Get("Content-Type"),
		lastModified: validate.ParseEpochMS(r.FormValue("last_modified")),
		sessionID:    sessionID,
		itemID:       itemID,
		inviteToken:  inviteToken,
		fingerprint:  validate.CleanFingerprint(r.FormValue("fingerprint")),
		expectedSHA1: expectedSHA1,
	})
}

// runUploadPipeline performs dedupe checks, invite gating, the Immich upload,
// album assignment, usage accounting, and audit logging, writing the HTTP
// response and WebSocket progress along the way.
func (s *Server) runUploadPipeline(w http.ResponseWriter, r *http.Request, in uploadInput) {
	sess := s.sessions.Get(r)
	size := in.size

	// SHA-1 of the spooled content; must be known before the Immich call
	// (x-immich-checksum header and bulk-check).
	h := sha1.New()
	if _, err := in.spool.Seek(0, io.SeekStart); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.Copy(h, in.spool); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	checksum := hex.EncodeToString(h.Sum(nil))

	// Integrity check against the client-computed hash (sent at chunk/init):
	// a mismatch means bytes were lost or corrupted between browser and server,
	// so reject before any Immich traffic. Absent hash = no verification
	// (older clients, whole-file uploads).
	if in.expectedSHA1 != "" && in.expectedSHA1 != checksum {
		slog.Warn("upload checksum mismatch", "file", in.fileName,
			"expected", in.expectedSHA1, "actual", checksum, "size", size)
		s.sendProgress(in.sessionID, in.itemID, "error", 100,
			"Checksum mismatch — the upload arrived corrupted, please retry", nil)
		errJSON(w, http.StatusBadRequest, "checksum_mismatch")
		return
	}

	// Timestamps: EXIF wins, then the client's last-modified, then now.
	// exifdate only consumes the image header, so a plain re-seek suffices.
	_, _ = in.spool.Seek(0, io.SeekStart)
	exifCreated, exifModified := exifdate.Read(in.spool)
	createdAt := time.Now().UTC()
	if exifCreated != nil {
		createdAt = *exifCreated
	} else if in.lastModified != nil {
		createdAt = time.UnixMilli(*in.lastModified).UTC()
	}
	modifiedAt := createdAt
	if exifModified != nil {
		modifiedAt = *exifModified
	}
	createdISO := immichISO(createdAt)
	modifiedISO := immichISO(modifiedAt)

	// Dedupe identity — must not change (see docs/rewrite-notes.md).
	var lm int64
	if in.lastModified != nil {
		lm = *in.lastModified
	}
	deviceAssetID := fmt.Sprintf("%s-%d-%d", in.fileName, lm, size)

	// Local duplicate checks.
	if found, err := s.store.HasChecksum(checksum); err == nil && found {
		s.sendProgress(in.sessionID, in.itemID, "duplicate", 100, "Duplicate (by checksum - local cache)", nil)
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "id": nil})
		return
	}
	if found, err := s.store.HasDeviceAsset(deviceAssetID); err == nil && found {
		s.sendProgress(in.sessionID, in.itemID, "duplicate", 100, "Already uploaded from this device (local cache)", nil)
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "id": nil})
		return
	}

	// Server-side duplicate check via Immich bulk-upload-check.
	s.sendProgress(in.sessionID, in.itemID, "checking", 2, "Checking duplicates…", nil)
	bulk := s.immich.BulkUploadCheck([]map[string]string{{"id": in.itemID, "checksum": checksum}})
	if res, ok := bulk[in.itemID]; ok && res.Action == "reject" && res.Reason == "duplicate" {
		assetID := res.AssetID
		var assetIDPtr *string
		if assetID != "" {
			assetIDPtr = &assetID
		}
		if err := s.store.InsertUpload(checksum, in.fileName, size, deviceAssetID, assetIDPtr, createdISO); err != nil {
			slog.Error("dedupe cache insert failed", "err", err)
		}
		s.sendProgress(in.sessionID, in.itemID, "duplicate", 100, "Duplicate (server)", assetID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "id": nullable(assetID)})
		return
	}

	// Invite gating (lookup, disabled/password/expiry checks, claim/usage limits).
	var targetAlbumID, targetAlbumName *string
	if in.inviteToken != "" {
		var ok bool
		targetAlbumID, targetAlbumName, ok = s.gateInvite(w, sess, in.sessionID, in.itemID, in.inviteToken)
		if !ok {
			return // response already written
		}
	}

	// Upload to Immich, streaming progress.
	s.sendProgress(in.sessionID, in.itemID, "uploading", 0, "Uploading…", nil)
	safeName := validate.SanitizeFilename(in.fileName)
	contentType := in.contentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if _, err := in.spool.Seek(0, io.SeekStart); err != nil {
		s.sendProgress(in.sessionID, in.itemID, "error", 100, err.Error(), nil)
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	result, err := s.immich.UploadAsset(immich.UploadParams{
		AccessToken:   sess.AccessToken,
		FileName:      safeName,
		ContentType:   contentType,
		Data:          in.spool,
		Size:          size,
		DeviceAssetID: deviceAssetID,
		// The "python-" prefix is kept for continuity with assets already
		// uploaded by the Python version (visible in Immich metadata only).
		DeviceID:    "python-" + in.sessionID,
		CreatedISO:  createdISO,
		ModifiedISO: modifiedISO,
		Checksum:    checksum,
		OnProgress: func(pct int) {
			s.sendProgress(in.sessionID, in.itemID, "uploading", pct, "", nil)
		},
	})
	if err != nil {
		s.sendProgress(in.sessionID, in.itemID, "error", 100, err.Error(), nil)
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result.StatusCode != http.StatusOK && result.StatusCode != http.StatusCreated {
		s.sendProgress(in.sessionID, in.itemID, "error", 100, result.ErrMessage, nil)
		errJSON(w, http.StatusBadRequest, result.ErrMessage)
		return
	}

	assetID := result.AssetID
	var assetIDPtr *string
	if assetID != "" {
		assetIDPtr = &assetID
	}
	if err := s.store.InsertUpload(checksum, in.fileName, size, deviceAssetID, assetIDPtr, createdISO); err != nil {
		slog.Error("dedupe cache insert failed", "err", err)
	}
	status := result.Status

	// Album assignment: an invite's album overrides the env default; an invite
	// without an album deliberately does not fall back to the env default.
	if assetID != "" {
		if in.inviteToken != "" {
			if strOr(targetAlbumID) != "" || strOr(targetAlbumName) != "" {
				if s.addAssetToAlbum(sess.AccessToken, assetID, targetAlbumID, targetAlbumName) {
					name := strOr(targetAlbumName)
					if name == "" {
						name = strOr(targetAlbumID)
					}
					status += fmt.Sprintf(" (added to album '%s')", name)
				}
			}
		} else if s.cfg.AlbumName != "" {
			if s.addAssetToAlbum(sess.AccessToken, assetID, nil, nil) {
				status += fmt.Sprintf(" (added to album '%s')", s.cfg.AlbumName)
			}
		}
	}

	wsStatus := "done"
	if result.Status == "duplicate" {
		wsStatus = "duplicate"
	}
	s.sendProgress(in.sessionID, in.itemID, wsStatus, 100, status, nullable(assetID))

	// Secondary bookkeeping must never fail the upload: log and continue.
	if in.inviteToken != "" {
		if err := s.store.IncrementInviteUsage(in.inviteToken); err != nil {
			slog.Error("failed to increment invite usage", "err", err)
		}
	}
	if err := s.store.InsertUploadEvent(in.inviteToken, clientIP(r), r.Header.Get("User-Agent"),
		in.fingerprint, in.fileName, size, checksum, assetIDPtr); err != nil {
		slog.Error("failed to record upload event", "err", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": nullable(assetID), "status": status})
}

// nullable maps "" to JSON null, keeping response shapes identical to Python.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// inviteErrText maps invite rejection codes to the progress messages shown in
// the upload queue UI.
var inviteErrText = map[string]string{
	"invalid_invite":           "Invalid invite token",
	"invite_disabled":          "Invite disabled",
	"invite_password_required": "Password required",
	"invite_expired":           "Invite expired",
	"invite_claimed":           "Invite already used",
	"invite_exhausted":         "Invite already used up",
}

// inviteUsable runs the passive (non-claiming) invite checks: existence,
// disabled, password authorization, expiry, and usage limits. Returns an
// error code, or "" when the invite is usable by sessionID.
func (s *Server) inviteUsable(inv *store.Invite, sess *session.Data, sessionID, token string) string {
	if inv == nil {
		return "invalid_invite"
	}
	if inv.Disabled {
		return "invite_disabled"
	}
	if strOr(inv.PasswordHash) != "" && !sess.InviteAuth[token] {
		return "invite_password_required"
	}
	if isExpired(inv.ExpiresAt) {
		return "invite_expired"
	}
	maxUses := inv.MaxUsesOrDefault()
	if maxUses == 1 {
		// Allow the claiming session to continue; block different sessions.
		if inv.Claimed && inv.ClaimedBySession != nil && *inv.ClaimedBySession != sessionID {
			return "invite_claimed"
		}
	} else if maxUses >= 0 && inv.UsedCount >= maxUses {
		// Negative max_uses means unlimited.
		return "invite_exhausted"
	}
	return ""
}

// uploadAuthCode implements the auth rule shared by the upload endpoints and
// the progress WebSocket. When the public upload page is disabled, the invite
// token acts as the auth credential (a logged-in session also passes).
// Returns "" when authorized, else an error code.
func (s *Server) uploadAuthCode(r *http.Request, sessionID, inviteToken string) string {
	if s.cfg.PublicUploadPageEnabled {
		return ""
	}
	sess := s.sessions.Get(r)
	if sess.AccessToken != "" {
		return ""
	}
	if inviteToken == "" {
		return "invite_required"
	}
	inv, err := s.store.GetInvite(inviteToken)
	if err != nil {
		slog.Error("invite lookup error", "err", err)
		return "db_error"
	}
	return s.inviteUsable(inv, sess, sessionID, inviteToken)
}

// uploadAuthorized gates every upload endpoint via uploadAuthCode. It runs
// before anything is spooled to disk and before any Immich traffic, so
// anonymous requests cannot fill the chunk directory or probe checksums. With
// PUBLIC_UPLOAD_PAGE_ENABLED=true all uploads are allowed (public drop box,
// the previous behavior). On rejection the error response has been written
// and false is returned.
func (s *Server) uploadAuthorized(w http.ResponseWriter, r *http.Request, sessionID, inviteToken string) bool {
	switch code := s.uploadAuthCode(r, sessionID, inviteToken); code {
	case "":
		return true
	case "invite_required":
		errJSON(w, http.StatusUnauthorized, code)
	case "db_error":
		errJSON(w, http.StatusInternalServerError, code)
	default:
		errJSON(w, http.StatusForbidden, code)
	}
	return false
}

// gateInvite enforces all invite restrictions for an upload. On failure it
// writes the error response (and progress message) and returns ok=false; on
// success it returns the invite's target album id/name.
func (s *Server) gateInvite(w http.ResponseWriter, sess *session.Data, sessionID, itemID, token string) (albumID, albumName *string, ok bool) {
	reject := func(httpStatus int, code string) (*string, *string, bool) {
		if message := inviteErrText[code]; message != "" {
			s.sendProgress(sessionID, itemID, "error", 100, message, nil)
		}
		errJSON(w, httpStatus, code)
		return nil, nil, false
	}

	inv, err := s.store.GetInvite(token)
	if err != nil {
		slog.Error("invite lookup error", "err", err)
		inv = nil
	}
	if code := s.inviteUsable(inv, sess, sessionID, token); code != "" {
		return reject(http.StatusForbidden, code)
	}

	if inv.MaxUsesOrDefault() == 1 && !inv.Claimed {
		// Atomically claim the one-time invite to prevent concurrent use.
		claimed, err := s.store.ClaimInvite(token, sessionID)
		if err != nil {
			slog.Error("invite claim failed", "err", err)
			errJSON(w, http.StatusInternalServerError, "invite_claim_failed")
			return nil, nil, false
		}
		if !claimed {
			// Someone else just claimed; re-check the owner.
			owner, err := s.store.InviteClaimOwner(token)
			if err != nil {
				owner = nil
			}
			if owner == nil || *owner != sessionID {
				return reject(http.StatusForbidden, "invite_claimed")
			}
		}
	}
	return inv.AlbumID, inv.AlbumName, true
}
