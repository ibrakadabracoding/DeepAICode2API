package server

import (
	"deepaicode2api/internal/deepai"
	"encoding/json"
	"net/http"
)

// sseWriter unifies SSE response headers and flush.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	// Commit 200 here: if the caller writes headers afterwards, net/http will flag it as
	// superfluous (the old implementation did exactly this, only creating noise in the logs).
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) data(raw []byte) {
	_ = deepai.WriteSSE(s.w, raw)
	s.flush()
}

func (s *sseWriter) event(name string, raw []byte) {
	_ = deepai.WriteSSEEvent(s.w, name, raw)
	s.flush()
}

func (s *sseWriter) done() {
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	s.flush()
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// jsonEvent serializes the event payload and returns nil on failure.
func jsonEvent(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
