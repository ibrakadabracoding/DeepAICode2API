package httpx

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// --- Response compression ---
//
// The console's /admin/api/accounts page returns 50 accounts by default (about
// 20-40KB of JSON), and /admin/api/overview with its model catalog is also over
// ten KB; for LAN / remote deployments these are all plain text, and gzip usually
// compresses them to 1/5-1/10. The same applies to the embedded JS / CSS.
//
// Only explicitly compressible types are compressed, and **SSE is skipped**:
// compressed streaming responses must flush frame by frame, which many clients do
// not support, and the benefit is close to zero.

// compressibleType reports whether a response should be compressed.
func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	// SSE must be skipped: frame-by-frame flush + gzip prevents clients from receiving increments.
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	switch {
	case strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "text/"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "application/xml"),
		strings.HasPrefix(ct, "image/svg+xml"):
		return true
	}
	return false
}

// gzipWriter decides whether to actually compress based on Content-Type.
//
// The decision is deferred until WriteHeader because handlers usually call
// Set("Content-Type") before WriteHeader, so the type is known by then; deciding
// earlier would force every handler to declare it itself.
type gzipWriter struct {
	http.ResponseWriter
	gz        *gzip.Writer
	decided   bool
	compress  bool
	wroteHead bool
}

func (w *gzipWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	ct := w.Header().Get("Content-Type")
	// If Content-Encoding is already set (e.g. passed through from upstream), do not wrap it again.
	if w.Header().Get("Content-Encoding") != "" || !compressibleType(ct) {
		return
	}
	w.compress = true
	w.Header().Del("Content-Length")
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("Content-Encoding", "gzip")
	w.gz = gzip.NewWriter(w.ResponseWriter)
}

func (w *gzipWriter) WriteHeader(code int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	// 1xx / 204 / 304 have no response body: adding Content-Encoding would make
	// proxies and clients treat an empty body as a compressed stream (most likely
	// on a 304 ETag hit).
	if code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified {
		w.decide()
	} else {
		w.decided = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush flushes gzip first in compression mode (otherwise the compression buffer stalls increments).
func (w *gzipWriter) Flush() {
	if w.compress {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// close ends the compression stream; it only matters when compression actually happened.
func (w *gzipWriter) close() {
	if w.compress && w.gz != nil {
		_ = w.gz.Close()
	}
}

// WithGzip compresses text responses for clients that declare gzip support.
func WithGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !AcceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// AcceptsGzip reports whether the client accepts gzip (supports q weights; q=0 means reject).
//
// A naive `strings.Contains(params, "q=0")` check would treat q=0.5 (accepted) as a
// rejection. This parses the number after "q=" and compares it.
func AcceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		return encodingWeight(params) > 0
	}
	return false
}

// encodingWeight extracts the weight from parameters like ";q=0.5"; defaults to 1.
func encodingWeight(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		if w, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return w
		}
		return 0
	}
	return 1
}
