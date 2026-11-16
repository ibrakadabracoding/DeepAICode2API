package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"deepaicode2api/internal/store"
)

// setAdminPassword enables admin password verification.
func setAdminPassword(t *testing.T, s *Server, pw string) {
	t.Helper()
	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = pw })
}

// hit sends one admin authentication request from the given source and returns the
// status code.
func hit(s *Server, remoteAddr, pw string) int {
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth", nil)
	req.RemoteAddr = remoteAddr
	if pw != "" {
		req.Header.Set("Authorization", "Bearer "+pw)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// TestAdminPasswordThrottle is the regression guard for P1-2.
//
// After consecutive failures exceed the threshold, the guard must enter backoff:
// during that period even the correct password gets 429 (no hint about whether it
// was right), and it recovers when the backoff expires; one success resets the count.
func TestAdminPasswordThrottle(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.adminGuardBase = 200 * time.Millisecond
	setAdminPassword(t, s, "s3cret")

	const ip = "127.0.0.1:34567" // default source of newTestServer
	for i := 0; i < adminAuthFreeAttempts; i++ {
		if code := hit(s, ip, "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误口令应回 401，实际 %d", i+1, code)
		}
	}
	// The 6th attempt is still "just one more wrong try": backoff starts counting
	// from this one.
	if code := hit(s, ip, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("触发退避的那次仍应回 401，实际 %d", code)
	}

	// Backoff period: both wrong and correct passwords get 429, so the endpoint
	// cannot be used as a password oracle.
	if code := hit(s, ip, "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("超过阈值后应回 429，实际 %d", code)
	}
	if code := hit(s, ip, "s3cret"); code != http.StatusTooManyRequests {
		t.Fatalf("退避期内正确口令也应回 429，实际 %d", code)
	}

	// Recovers once the backoff expires.
	time.Sleep(260 * time.Millisecond)
	if code := hit(s, ip, "s3cret"); code != http.StatusOK {
		t.Fatalf("退避结束后应放行，实际 %d", code)
	}

	// The counter resets after a success: another wrong try is just a normal 401 and
	// must not immediately return 429.
	if code := hit(s, ip, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("成功后计数应清零，实际 %d", code)
	}
}

// TestAdminPasswordThrottlePerIP verifies that backoff is isolated per source: other
// machines are unaffected.
func TestAdminPasswordThrottlePerIP(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.adminGuardBase = 30 * time.Millisecond
	setAdminPassword(t, s, "s3cret")

	const attacker = "10.0.0.9:1234"
	for i := 0; i < adminAuthFreeAttempts+2; i++ {
		hit(s, attacker, "wrong")
	}
	if code := hit(s, attacker, "s3cret"); code != http.StatusTooManyRequests {
		t.Fatalf("攻击来源应处于退避，实际 %d", code)
	}
	// Another source can still authenticate normally.
	if code := hit(s, "127.0.0.1:34567", "s3cret"); code != http.StatusOK {
		t.Fatalf("其它来源不应被牵连，实际 %d", code)
	}
}

// TestAdminPasswordNotConfiguredLoopbackOnly guards "when no password is set, only
// the local machine can administer".
func TestAdminPasswordNotConfiguredLoopbackOnly(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// newTestServer sets no admin password.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非回环来源应被拒（403），实际 %d", rec.Code)
	}
}
