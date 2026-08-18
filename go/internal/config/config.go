// Package config loads application settings from environment variables (.env).
// Mirrors app/config.py: settings are read once at startup and never mutated.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Settings holds all runtime configuration. Names and defaults match the
// Python implementation so existing .env files keep working.
type Settings struct {
	ImmichBaseURL           string
	ImmichAPIKey            string
	MaxConcurrent           int
	AlbumName               string
	PublicUploadPageEnabled bool
	PublicBaseURL           string
	StateDB                 string
	SessionSecret           string
	LogLevel                string
	ChunkedUploadsEnabled   bool
	ChunkSizeMB             int

	// ChunkDir is where chunked-upload parts are spooled. The Python version
	// hardcoded /data/chunks; CHUNK_DIR makes it configurable with the same default.
	ChunkDir string
	// FrontendDir is the directory containing the static frontend files.
	FrontendDir string

	Host string
	Port string
}

// NormalizedBaseURL returns the Immich base URL without a trailing slash.
func (s *Settings) NormalizedBaseURL() string {
	return strings.TrimRight(s.ImmichBaseURL, "/")
}

func asBool(v string, def bool) bool {
	if v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func asInt(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// findFrontendDir probes the usual locations of the frontend/ directory so the
// binary works both from the repo root and from inside the go/ subfolder.
func findFrontendDir() string {
	candidates := []string{"frontend", filepath.Join("..", "frontend")}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "frontend"),
			filepath.Join(dir, "..", "frontend"),
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return "frontend"
}

// Load reads .env (if present) and returns the settings with defaults applied.
func Load() *Settings {
	_ = godotenv.Load()

	secret := os.Getenv("SESSION_SECRET")
	if secret == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		secret = hex.EncodeToString(b)
	}

	return &Settings{
		ImmichBaseURL:           envOr("IMMICH_BASE_URL", "http://127.0.0.1:2283/api"),
		ImmichAPIKey:            os.Getenv("IMMICH_API_KEY"),
		MaxConcurrent:           asInt(os.Getenv("MAX_CONCURRENT"), 3),
		AlbumName:               os.Getenv("IMMICH_ALBUM_NAME"),
		PublicUploadPageEnabled: asBool(os.Getenv("PUBLIC_UPLOAD_PAGE_ENABLED"), false),
		PublicBaseURL:           os.Getenv("PUBLIC_BASE_URL"),
		StateDB:                 envOr("STATE_DB", "/data/state.db"),
		SessionSecret:           secret,
		LogLevel:                strings.ToUpper(envOr("LOG_LEVEL", "INFO")),
		ChunkedUploadsEnabled:   asBool(os.Getenv("CHUNKED_UPLOADS_ENABLED"), false),
		ChunkSizeMB:             asInt(os.Getenv("CHUNK_SIZE_MB"), 95),
		ChunkDir:                envOr("CHUNK_DIR", "/data/chunks"),
		FrontendDir:             envOr("FRONTEND_DIR", findFrontendDir()),
		Host:                    envOr("HOST", "0.0.0.0"),
		Port:                    envOr("PORT", "8080"),
	}
}
