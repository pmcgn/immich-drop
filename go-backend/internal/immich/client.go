// Package immich is the HTTP client for the Immich server API. Per-call
// timeouts mirror the Python version: login 15 s, asset upload 120 s,
// reachability probes 4 s, everything else 10 s.
package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

type Client struct {
	baseURL string // normalized, no trailing slash
	apiKey  string
	hc      *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, hc: &http.Client{}}
}

// applyAuth sets auth headers: a session access token wins over the API key.
func (c *Client) applyAuth(req *http.Request, accessToken string) {
	req.Header.Set("Accept", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	} else if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
}

func (c *Client) doJSON(method, path, accessToken string, body any, timeout time.Duration) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	c.applyAuth(req, accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// Ping is a best-effort reachability check. It only ever uses the API key
// (never a session token) and returns false when no API key is configured.
func (c *Client) Ping() bool {
	if c.apiKey == "" {
		return false
	}
	for _, path := range []string{"/server-info", "/server/version", "/users/me"} {
		status, _, err := c.doJSON(http.MethodGet, path, "", nil, 4*time.Second)
		if err == nil && status >= 200 && status < 400 {
			return true
		}
	}
	return false
}

// Login authenticates with email/password and returns the Immich response body.
func (c *Client) Login(email, password string) (int, map[string]any, error) {
	status, body, err := c.doJSON(http.MethodPost, "/auth/login", "",
		map[string]string{"email": email, "password": password}, 15*time.Second)
	if err != nil {
		return 0, nil, err
	}
	var data map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &data)
	}
	if data == nil {
		data = map[string]any{}
	}
	return status, data, nil
}

// GetAlbums returns the raw /albums response (status + body).
func (c *Client) GetAlbums(accessToken string) (int, []byte, error) {
	return c.doJSON(http.MethodGet, "/albums", accessToken, nil, 10*time.Second)
}

// CreateAlbum creates an album; description may be empty.
func (c *Client) CreateAlbum(accessToken, name, description string) (int, []byte, error) {
	payload := map[string]string{"albumName": name}
	if description != "" {
		payload["description"] = description
	}
	return c.doJSON(http.MethodPost, "/albums", accessToken, payload, 10*time.Second)
}

// FindAlbumIDByName scans /albums for an exact albumName match.
func (c *Client) FindAlbumIDByName(accessToken, name string) (string, error) {
	status, body, err := c.GetAlbums(accessToken)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", nil
	}
	var albums []map[string]any
	if err := json.Unmarshal(body, &albums); err != nil {
		return "", err
	}
	for _, album := range albums {
		if albumName, _ := album["albumName"].(string); albumName == name {
			if id, _ := album["id"].(string); id != "" {
				return id, nil
			}
		}
	}
	return "", nil
}

// AddAssetToAlbum puts one asset into an album. A per-asset result of
// error == "duplicate" (already in album) counts as success.
func (c *Client) AddAssetToAlbum(accessToken, albumID, assetID string) bool {
	status, body, err := c.doJSON(http.MethodPut, "/albums/"+albumID+"/assets", accessToken,
		map[string]any{"ids": []string{assetID}}, 10*time.Second)
	if err != nil {
		slog.Error("error adding asset to album", "album", albumID, "asset", assetID, "err", err)
		return false
	}
	if status != http.StatusOK {
		slog.Warn("album add rejected", "album", albumID, "asset", assetID,
			"status", status, "body", strings.TrimSpace(string(body)))
		return false
	}
	var results []map[string]any
	if err := json.Unmarshal(body, &results); err != nil {
		slog.Warn("album add returned unexpected body", "album", albumID, "asset", assetID,
			"body", strings.TrimSpace(string(body)))
		return false
	}
	for _, res := range results {
		if success, _ := res["success"].(bool); success {
			return true
		}
		if errStr, _ := res["error"].(string); errStr == "duplicate" {
			return true
		}
	}
	slog.Warn("album add accepted no asset", "album", albumID, "asset", assetID,
		"body", strings.TrimSpace(string(body)))
	return false
}

// BulkCheckResult is one entry of the bulk-upload-check response.
type BulkCheckResult struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	AssetID string `json:"assetId"`
}

// BulkUploadCheck asks Immich whether checksums are already known. Returns an
// empty map on any failure (the caller then proceeds with the upload).
func (c *Client) BulkUploadCheck(checks []map[string]string) map[string]BulkCheckResult {
	status, body, err := c.doJSON(http.MethodPost, "/assets/bulk-upload-check", "",
		map[string]any{"assets": checks}, 10*time.Second)
	if err != nil || status != http.StatusOK {
		return map[string]BulkCheckResult{}
	}
	var parsed struct {
		Results []BulkCheckResult `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return map[string]BulkCheckResult{}
	}
	out := make(map[string]BulkCheckResult, len(parsed.Results))
	for _, r := range parsed.Results {
		out[r.ID] = r
	}
	return out
}

// UploadParams describes one asset upload to POST /assets.
type UploadParams struct {
	AccessToken   string
	FileName      string    // sanitized name sent to Immich
	ContentType   string
	Data          io.Reader // file content, streamed (typically a disk spool file)
	Size          int64     // exact length of Data in bytes
	DeviceAssetID string
	DeviceID      string
	CreatedISO    string
	ModifiedISO   string
	Checksum      string            // SHA-1 hex, sent as x-immich-checksum
	OnProgress    func(percent int) // optional; called with 0-100
}

// UploadResult is the parsed outcome of an asset upload.
type UploadResult struct {
	StatusCode int
	AssetID    string
	Status     string // "created" or "duplicate"
	ErrMessage any    // Immich "message" field (or raw body) on non-2xx
}

// progressReader reports read progress as an integer percentage, emitting only
// on change (mirrors the Python MultipartEncoderMonitor callback).
type progressReader struct {
	r       io.Reader
	total   int64
	read    int64
	lastPct int
	cb      func(int)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.total > 0 && p.cb != nil {
		pct := int(p.read * 100 / p.total)
		if pct != p.lastPct {
			p.lastPct = pct
			p.cb(pct)
		}
	}
	return n, err
}

// UploadAsset posts the multipart asset payload to Immich. The file content is
// streamed from p.Data (a disk spool file) rather than buffered, so memory use
// is constant regardless of file size (only the tiny envelope is buffered).
func (c *Client) UploadAsset(p UploadParams) (*UploadResult, error) {
	// Build the multipart envelope: all simple fields, then the assetData part
	// header (prefix); the closing boundary (suffix) follows the file bytes.
	// Fields-before-file matches the order the Python requests library sent.
	var envelope bytes.Buffer
	mw := multipart.NewWriter(&envelope)
	fields := [][2]string{
		{"deviceAssetId", p.DeviceAssetID},
		{"deviceId", p.DeviceID},
		{"fileCreatedAt", p.CreatedISO},
		{"fileModifiedAt", p.ModifiedISO},
		{"isFavorite", "false"},
		{"filename", p.FileName},
		{"originalFileName", p.FileName},
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="assetData"; filename="%s"`,
		strings.ReplaceAll(strings.ReplaceAll(p.FileName, `\`, `\\`), `"`, `\"`)))
	contentType := p.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	hdr.Set("Content-Type", contentType)
	if _, err := mw.CreatePart(hdr); err != nil {
		return nil, err
	}
	prefix := append([]byte(nil), envelope.Bytes()...)
	if err := mw.Close(); err != nil {
		return nil, err
	}
	suffix := envelope.Bytes()[len(prefix):]

	total := int64(len(prefix)) + p.Size + int64(len(suffix))
	body := &progressReader{
		r:     io.MultiReader(bytes.NewReader(prefix), p.Data, bytes.NewReader(suffix)),
		total: total,
		cb:    p.OnProgress,
	}
	// The Python version used a flat 120 s, which caps uploads at ~1 GB on a
	// fast link. Scale with size instead (floor of ~1 MiB/s) so multi-GB
	// videos can finish while stalled transfers still get cancelled.
	timeout := 120*time.Second + time.Duration(p.Size>>20)*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/assets", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = total
	c.applyAuth(req, p.AccessToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("x-immich-checksum", p.Checksum)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	result := &UploadResult{StatusCode: resp.StatusCode}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var data map[string]any
		_ = json.Unmarshal(respBody, &data)
		result.AssetID, _ = data["id"].(string)
		result.Status = "created"
		if s, ok := data["status"].(string); ok {
			result.Status = s
		}
	} else {
		var data map[string]any
		if err := json.Unmarshal(respBody, &data); err == nil {
			if msg, ok := data["message"]; ok {
				result.ErrMessage = msg
			}
		}
		if result.ErrMessage == nil {
			result.ErrMessage = string(respBody)
		}
	}
	return result, nil
}
