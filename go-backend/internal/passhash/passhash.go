// Package passhash implements the invite password hash format used by the
// Python version: "pbkdf2_sha256$<iterations>$<salt_hex>$<hash_hex>".
// Existing hashes in state.db must keep verifying, so the format is frozen.
package passhash

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const iterations = 200_000

// Hash derives a new password hash (random 16-byte salt, 200 000 iterations).
func Hash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", iterations, hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
}

// Verify checks password against a stored hash string. Malformed hashes and
// empty passwords verify as false.
func Verify(stored, password string) bool {
	if password == "" {
		return false
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return hmac.Equal(dk, want)
}
