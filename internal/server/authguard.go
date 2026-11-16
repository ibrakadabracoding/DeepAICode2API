package server

import (
	"deepaicode2api/internal/httpx"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Admin surface: password brute-force protection ---
//
// The admin password is the only line of defense for the console and server state, and
// /login is a public page. Previously withAdminAuth only did a constant-time comparison,
// so failures had no cost-anyone on the LAN could guess without limit.
//
// Strategy (per source IP):
//   - The first free failures return 401 as usual;
//   - After that, exponential backoff applies (base×2^n, capped at max); while in
//     backoff the response is a direct 429 + Retry-After;
//   - One success resets the count;
//   - When the table reaches its limit, expired entries are pruned so forged sources
//     cannot exhaust memory.

const (
	// adminAuthFreeAttempts is the number of consecutive failures allowed before backoff starts.
	adminAuthFreeAttempts = 5
	// adminAuthBackoffBase is the initial backoff duration (tests can inject a shorter value).
	adminAuthBackoffBase = time.Second
	// adminAuthBackoffMax is the backoff cap: too long would lock operators out too.
	adminAuthBackoffMax = 30 * time.Second
	// adminAuthGuardMaxEntries is the table size limit; expired entries are pruned first when exceeded.
	adminAuthGuardMaxEntries = 4096
)

// adminGuardEntry is the failure count and backoff deadline for one source IP.
type adminGuardEntry struct {
	fails int
	until time.Time
}

// adminGuard records admin password failures.
type adminGuard struct {
	mu      sync.Mutex
	entries map[string]*adminGuardEntry
}

func newAdminGuard() *adminGuard {
	return &adminGuard{entries: map[string]*adminGuardEntry{}}
}

// clientIP extracts the source IP (without port); on parse failure it returns the original string.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	return host
}

// blocked reports whether the source is currently in backoff and returns the remaining duration.
func (g *adminGuard) blocked(ip string, now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[ip]
	if e == nil || e.until.IsZero() {
		return 0, false
	}
	if now.Before(e.until) {
		return e.until.Sub(now), true
	}
	return 0, false
}

// failure records one failure and returns the backoff duration determined for it (0 means backoff has not started).
func (g *adminGuard) failure(ip string, now time.Time, free int, base, max time.Duration) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.entries) >= adminAuthGuardMaxEntries {
		g.pruneLocked(now)
	}
	e := g.entries[ip]
	if e == nil {
		e = &adminGuardEntry{}
		g.entries[ip] = e
	}
	e.fails++
	if e.fails <= free {
		return 0
	}
	// On the first failure past the free allowance, backoff = base; each subsequent failure doubles it.
	shift := e.fails - free - 1
	backoff := base
	for i := 0; i < shift && backoff < max; i++ {
		backoff *= 2
	}
	if backoff > max {
		backoff = max
	}
	e.until = now.Add(backoff)
	return backoff
}

// success clears the failure record for the source.
func (g *adminGuard) success(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, ip)
}

// pruneLocked removes entries past their backoff that are no longer meaningful (caller holds the lock).
func (g *adminGuard) pruneLocked(now time.Time) {
	for ip, e := range g.entries {
		if e.until.IsZero() || now.After(e.until) {
			delete(g.entries, ip)
		}
	}
}

// adminGuardParams returns the effective free/base/max (tests may inject shorter values).
func (s *Server) adminGuardParams() (free int, base, max time.Duration) {
	free, base, max = adminAuthFreeAttempts, s.adminGuardBase, adminAuthBackoffMax
	if base <= 0 {
		base = adminAuthBackoffBase
	}
	return free, base, max
}

// checkAdminThrottle is called before comparing the password: return 429 directly while in backoff.
func (s *Server) checkAdminThrottle(w http.ResponseWriter, r *http.Request) bool {
	delay, blocked := s.adminGuard.blocked(clientIP(r.RemoteAddr), time.Now())
	if !blocked {
		return true
	}
	secs := int(delay.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpx.WriteErr(w, http.StatusTooManyRequests,
		"管理口令尝试过于频繁，请 "+strconv.Itoa(secs)+" 秒后再试", "invalid_request_error")
	return false
}

// noteAdminAuthFailure records a failure and logs when backoff begins.
func (s *Server) noteAdminAuthFailure(r *http.Request) {
	ip := clientIP(r.RemoteAddr)
	free, base, max := s.adminGuardParams()
	backoff := s.adminGuard.failure(ip, time.Now(), free, base, max)
	if backoff > 0 {
		log.Printf("[admin-auth] 管理口令连续失败，来源 %s 退避 %s（%s %s）",
			ip, backoff, r.Method, r.URL.Path)
	}
}

// noteAdminAuthSuccess resets the count when the password is correct.
func (s *Server) noteAdminAuthSuccess(r *http.Request) {
	s.adminGuard.success(clientIP(r.RemoteAddr))
}
