package server

// Garbage collection for the chunk spool directory. A client that calls
// chunk/init but never chunk/complete leaves its parts on disk forever (a
// known gap in the Python version, see docs/rewrite-notes.md #6); since the
// chunk endpoints are unauthenticated this is also a disk-exhaustion vector.
// The sweeper removes upload directories that have seen no writes for
// chunkTTL. In-progress uploads keep touching their part files, so only
// genuinely abandoned uploads are collected.

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	chunkTTL      = 24 * time.Hour
	sweepInterval = time.Hour
)

// StartChunkSweeper launches the background sweep loop (one goroutine for the
// process lifetime). Safe to call even when chunked uploads are disabled;
// it then only cleans up leftovers from earlier runs.
func (s *Server) StartChunkSweeper() {
	go func() {
		s.sweepChunks(time.Now().Add(-chunkTTL))
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for now := range ticker.C {
			s.sweepChunks(now.Add(-chunkTTL))
		}
	}()
}

// sweepChunks removes every {session}/{item} spool directory whose newest
// write is older than cutoff, then prunes empty (and equally stale) session
// directories.
func (s *Server) sweepChunks(cutoff time.Time) {
	root := s.cfg.ChunkDir
	sessions, err := os.ReadDir(root)
	if err != nil {
		return // spool dir missing/unreadable; nothing to sweep
	}
	for _, sess := range sessions {
		if !sess.IsDir() {
			continue
		}
		sessPath := filepath.Join(root, sess.Name())
		items, err := os.ReadDir(sessPath)
		if err != nil {
			continue
		}
		remaining := len(items)
		for _, item := range items {
			if !item.IsDir() {
				continue
			}
			itemPath := filepath.Join(sessPath, item.Name())
			if newestMTime(itemPath).Before(cutoff) {
				if err := os.RemoveAll(itemPath); err != nil {
					slog.Warn("chunk sweep failed", "dir", itemPath, "err", err)
				} else {
					slog.Info("removed abandoned chunk upload", "dir", itemPath)
					remaining--
				}
			}
		}
		// Only prune a session dir that is empty AND stale, so a concurrent
		// chunk/init creating its first item dir is never raced.
		if remaining == 0 {
			if st, err := os.Stat(sessPath); err == nil && st.ModTime().Before(cutoff) {
				_ = os.Remove(sessPath)
			}
		}
	}
}

// newestMTime returns the most recent modification time of dir or any direct
// entry in it (parts and meta.json live flat in the upload dir).
func newestMTime(dir string) time.Time {
	var newest time.Time
	if st, err := os.Stat(dir); err == nil {
		newest = st.ModTime()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}
