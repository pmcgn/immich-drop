package server

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"immich-drop/internal/exifdate"
	"immich-drop/internal/immich"
	"immich-drop/internal/session"
	"immich-drop/internal/validate"
)

// uploadInput is one file to push through the shared upload pipeline. The
// Python version duplicated this flow between /api/upload and
// /api/upload/chunk/complete; here both handlers feed runUploadPipeline.
type uploadInput struct {
	raw          []byte
	fileName     string // original, unsanitized client filename
	contentType  string
	lastModified *int64 // epoch millis, already validated
	sessionID    string // already validated
	itemID       string // already validated
	inviteToken  string // "" when absent; format already validated
	fingerprint  string // already cleaned
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
	if vals, ok := r.MultipartForm.Value["invite_token"]; ok && len(vals) > 0 {
		if !validate.IsValidInviteToken(vals[0]) {
			s.sendProgress(sessionID, itemID, "error", 100, "Invalid invite token", nil)
			errJSON(w, http.StatusForbidden, "invalid_invite")
			return
		}
		inviteToken = vals[0]
	}

	// Take an upload slot before buffering the file into memory.
	release := s.acquireUploadSlot()
	defer release()

	file, header, err := r.FormFile("file")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "missing_file")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.runUploadPipeline(w, r, uploadInput{
		raw:          raw,
		fileName:     header.Filename,
		contentType:  header.Header.Get("Content-Type"),
		lastModified: validate.ParseEpochMS(r.FormValue("last_modified")),
		sessionID:    sessionID,
		itemID:       itemID,
		inviteToken:  inviteToken,
		fingerprint:  validate.CleanFingerprint(r.FormValue("fingerprint")),
	})
}

// runUploadPipeline performs dedupe checks, invite gating, the Immich upload,
// album assignment, usage accounting, and audit logging, writing the HTTP
// response and WebSocket progress along the way.
func (s *Server) runUploadPipeline(w http.ResponseWriter, r *http.Request, in uploadInput) {
	sess := s.sessions.Get(r)
	size := int64(len(in.raw))
	sum := sha1.Sum(in.raw)
	checksum := hex.EncodeToString(sum[:])

	// Timestamps: EXIF wins, then the client's last-modified, then now.
	exifCreated, exifModified := exifdate.Read(in.raw)
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
	result, err := s.immich.UploadAsset(immich.UploadParams{
		AccessToken:   sess.AccessToken,
		FileName:      safeName,
		ContentType:   contentType,
		Data:          in.raw,
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

// gateInvite enforces all invite restrictions for an upload. On failure it
// writes the error response (and progress message) and returns ok=false; on
// success it returns the invite's target album id/name.
func (s *Server) gateInvite(w http.ResponseWriter, sess *session.Data, sessionID, itemID, token string) (albumID, albumName *string, ok bool) {
	reject := func(httpStatus int, code, message string) (*string, *string, bool) {
		if message != "" {
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
	if inv == nil {
		return reject(http.StatusForbidden, "invalid_invite", "Invalid invite token")
	}
	if inv.Disabled {
		return reject(http.StatusForbidden, "invite_disabled", "Invite disabled")
	}
	if strOr(inv.PasswordHash) != "" && !sess.InviteAuth[token] {
		return reject(http.StatusForbidden, "invite_password_required", "Password required")
	}
	if isExpired(inv.ExpiresAt) {
		return reject(http.StatusForbidden, "invite_expired", "Invite expired")
	}

	maxUses := inv.MaxUsesOrDefault()
	if maxUses == 1 {
		if inv.Claimed {
			// Allow the claiming session to continue; block different sessions.
			if inv.ClaimedBySession != nil && *inv.ClaimedBySession != sessionID {
				return reject(http.StatusForbidden, "invite_claimed", "Invite already used")
			}
		} else {
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
					return reject(http.StatusForbidden, "invite_claimed", "Invite already used")
				}
			}
		}
	} else {
		limit := maxUses
		if limit < 0 {
			limit = 1_000_000_000 // negative max_uses means unlimited
		}
		if inv.UsedCount >= limit {
			return reject(http.StatusForbidden, "invite_exhausted", "Invite already used up")
		}
	}
	return inv.AlbumID, inv.AlbumName, true
}
