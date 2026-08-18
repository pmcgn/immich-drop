package server

import (
	"log/slog"
	"net/http"

	qrcode "github.com/skip2/go-qrcode"
)

// handleQR generates a QR code PNG for the given text (query param "text").
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		errJSON(w, http.StatusBadRequest, "missing_text")
		return
	}
	if len([]rune(text)) > 2048 {
		errJSON(w, http.StatusBadRequest, "text_too_long")
		return
	}
	png, err := qrcode.Encode(text, qrcode.Medium, 256)
	if err != nil {
		slog.Warn("qr generation failed", "err", err)
		errJSON(w, http.StatusNotImplemented, "qr_not_available")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}
