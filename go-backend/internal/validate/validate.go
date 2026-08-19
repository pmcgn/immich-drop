// Package validate holds the input-validation helpers shared by all handlers.
// Regexes and limits mirror app/app.py exactly; changing them changes which
// client requests are accepted.
package validate

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// Invite tokens are generated server-side as uuid4-hex: 32 lowercase hex chars.
	inviteTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// session_id / item_id come from crypto.randomUUID() or a base36 fallback in
	// the frontend. Dot-only names are rejected separately (no lookahead in RE2)
	// because the ids are used as directory names.
	clientIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	dotsOnlyRe = regexp.MustCompile(`^\.+$`)
	// fingerprint is "<uuid>:<digits>" (or bare device id) from the frontend.
	fingerprintRe = regexp.MustCompile(`^[A-Za-z0-9._:|-]{1,128}$`)
	// Immich album ids are UUIDs.
	albumIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)
	// Client-computed file checksums are SHA-1 hex digests.
	sha1HexRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

const (
	MaxNameLen      = 255
	MaxPasswordLen  = 256
	MaxTokensPerReq = 1000
	MaxChunks       = 100_000
	// Upper bound for client-supplied epoch-millis timestamps (year 3000).
	MaxEpochMS int64 = 32_503_680_000_000
)

// IsValidInviteToken reports whether token looks like a server-generated invite token.
func IsValidInviteToken(token string) bool {
	return inviteTokenRe.MatchString(token)
}

// IsValidClientID reports whether value is a plausible client-generated session/item id.
func IsValidClientID(value string) bool {
	return clientIDRe.MatchString(value) && !dotsOnlyRe.MatchString(value)
}

// IsValidAlbumID reports whether value looks like an Immich album id.
func IsValidAlbumID(value string) bool {
	return albumIDRe.MatchString(value)
}

// IsValidSHA1Hex reports whether value is a lowercase SHA-1 hex digest.
func IsValidSHA1Hex(value string) bool {
	return sha1HexRe.MatchString(value)
}

// CleanFingerprint returns the fingerprint if well-formed, else "".
func CleanFingerprint(value string) string {
	if fingerprintRe.MatchString(value) {
		return value
	}
	return ""
}

// ParseEpochMS coerces a client-supplied epoch-millis value to int64,
// returning nil when missing or implausible.
func ParseEpochMS(v any) *int64 {
	ms, ok := toInt64(v)
	if !ok {
		return nil
	}
	if ms > 0 && ms <= MaxEpochMS {
		return &ms
	}
	return nil
}

// ParseNonNegInt coerces v to a non-negative int64 no larger than max, else nil.
func ParseNonNegInt(v any, max int64) *int64 {
	n, ok := toInt64(v)
	if !ok {
		return nil
	}
	if n >= 0 && n <= max {
		return &n
	}
	return nil
}

// ToInt64 coerces JSON/form values (float64, string, int variants, bool) to
// int64 the way Python's int() does for the cases the frontend can produce.
func ToInt64(v any) (int64, bool) { return toInt64(v) }

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// CleanText returns v as a trimmed, length-capped string, or nil if v is not a string.
func CleanText(v any, maxLen int) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > maxLen {
		s = string(r[:maxLen])
	}
	return &s
}

// SanitizeFilename minimally sanitizes a filename while preserving the original
// name: control chars (0x00-0x1F, 0x7F) are removed, path separators become
// '_', and empty results fall back to "file".
func SanitizeFilename(name string) string {
	var b strings.Builder
	for _, ch := range name {
		switch {
		case ch < 32 || ch == 127:
			// drop control characters
		case ch == '/' || ch == '\\':
			b.WriteRune('_')
		default:
			b.WriteRune(ch)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return "file"
	}
	return cleaned
}
