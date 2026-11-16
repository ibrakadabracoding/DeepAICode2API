package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// --- CORS and origin validation ---

// withCORS handles cross-origin requests.
//
// By default only local origins are allowed (localhost / 127.0.0.1 / [::1], any port);
// for LAN access, explicitly list comma-separated origins in settings. No wildcards are
// provided.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.corsOrigin(r.Header.Get("Origin"))
		writeCORSHeaders(w, allowed)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeCORSHeaders writes CORS response headers only when the origin passes validation.
func writeCORSHeaders(w http.ResponseWriter, allowed string) {
	if allowed == "" || hasWildcard(allowed) {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", allowed)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-beta, X-Admin-Password, X-Session-Affinity")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
}

// corsOrigin returns the Allow-Origin value that should be sent back to the browser; an
// empty string means not allowed.
func (s *Server) corsOrigin(raw string) string {
	if raw == "" || raw == "null" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}

	// 1) Explicit allow list (comma-separated; values come from console settings)
	if cfg := strings.TrimSpace(s.Store.Settings().CORSOrigin); cfg != "" {
		for _, entry := range strings.Split(cfg, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !hasWildcard(entry) && strings.EqualFold(entry, raw) {
				return entry
			}
		}
		return ""
	}

	// 2) Localhost only by default: hostname must be in the loopback set
	if !isLoopbackHost(u.Hostname()) {
		return ""
	}
	if port := u.Port(); port != "" {
		return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
	}
	return u.Scheme + "://" + u.Hostname()
}

func hasWildcard(s string) bool { return strings.IndexByte(s, '*') >= 0 }

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
