package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"deepaicode2api/internal/httpx"
	"net/http"
	"net/url"
	"strings"
)

// --- Data plane / admin plane authentication ---

// withAuth validates the Bearer key.
//
// When no key has been issued yet:
//   - requests from the local loopback are allowed (convenient for first use and for
//     bootstrapping the console);
//   - requests from non-loopback sources always get 503 setup_required.
//     Otherwise, while listening on 0.0.0.0 (container / LAN), the gateway would be an
//     open relay with no authentication between first startup and creation of the first key.
//
// Once a key has been issued, every source is subject to authentication.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Store.HasKeys() {
			if isLoopbackAddr(r.RemoteAddr) {
				next(w, r)
				return
			}
			httpx.WriteErr(w, http.StatusServiceUnavailable,
				"网关尚未签发 API 密钥，请先在本机控制台创建密钥（/admin）", "setup_required")
			return
		}
		key := extractKey(r)
		if key == "" {
			httpx.WriteErr(w, http.StatusUnauthorized, "缺少 API 密钥", "invalid_request_error")
			return
		}
		if !s.Store.ValidKey(key) {
			httpx.WriteErr(w, http.StatusUnauthorized, "API 密钥无效或已停用", "invalid_request_error")
			return
		}
		// Per-key rate limiting: token bucket (RPS) + in-flight concurrency. Default 0 = unlimited.
		release, ok, retryAfter := s.rateLimitGate(keyIDOf(key))
		if !ok {
			writeRateLimited(w, retryAfter)
			return
		}
		defer release()
		next(w, r)
	}
}

// withAdminAuth protects the console endpoints.
//
// Authentication policy:
//   - Admin password set: it must be supplied via X-Admin-Password or Authorization: Bearer;
//   - Admin password not set: only requests from the local loopback are allowed (avoids
//     exposing the interface on the LAN).
//
// It also performs a same-origin check on write operations (non-GET/HEAD/OPTIONS) to
// block cross-site request forgery.
func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pw := s.Store.Settings().AdminPassword; pw != "" {
			// Check backoff first: a source in its backoff window gets 429 immediately, with no
			// hint about whether the password was right or wrong.
			if !s.checkAdminThrottle(w, r) {
				return
			}
			if !secureEqual(adminPasswordFrom(r), pw) {
				s.noteAdminAuthFailure(r)
				httpx.WriteErr(w, http.StatusUnauthorized, "管理密码缺失或错误", "invalid_request_error")
				return
			}
			s.noteAdminAuthSuccess(r)
		} else if !isLoopbackAddr(r.RemoteAddr) {
			httpx.WriteErr(w, http.StatusForbidden, "未设置管理密码，管理接口仅允许本机访问", "invalid_request_error")
			return
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.adminSameOrigin(r) {
				httpx.WriteErr(w, http.StatusForbidden, "跨站请求被拒绝", "invalid_request_error")
				return
			}
		}
		next(w, r)
	}
}

func adminPasswordFrom(r *http.Request) string {
	if v := r.Header.Get("X-Admin-Password"); v != "" {
		return v
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	return ""
}

func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) adminSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if cfg := strings.TrimSpace(s.Store.Settings().CORSOrigin); cfg != "" {
		for entry := range strings.SplitSeq(cfg, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !hasWildcard(entry) && strings.EqualFold(entry, origin) {
				return true
			}
		}
	}
	return false
}

func (s *Server) resolveKeyRef(ref string) string {
	for _, k := range s.Store.Keys() {
		if k.Key == ref || mask(k.Key) == ref {
			return k.Key
		}
	}
	return ref
}

// keyExists reports whether a full Key still exists in the account pool.
//
// resolveKeyRef returns its input unchanged when it finds no match, so a non-empty
// resolution result does not mean the key exists. Confirm with this function before
// delete / enable / disable; otherwise deleting an already-revoked Key would report success.
func (s *Server) keyExists(key string) bool {
	if key == "" {
		return false
	}
	for _, k := range s.Store.Keys() {
		if k.Key == key {
			return true
		}
	}
	return false
}

func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// handleAdminAuth validates the admin password itself (used by the login page).
//
// It is registered through withAdminAuth, so "missing or wrong password" has already
// been rejected there with 401; reaching this point means the password is correct. It
// deliberately returns neither the password nor any settings, just ok.
func (s *Server) handleAdminAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
