package server

import (
	"deepaicode2api/internal/deepai"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// maxInFlightPerAccount is the per-account cap on concurrent upstream requests.
//
// Scheduling only decides who gets picked this round; it does not keep concurrency
// from piling onto a few accounts. Measured with 12 concurrent requests, all of them
// can land on 1-2 accounts, and any upstream rate limiting produces a long tail.
const maxInFlightPerAccount = 2

// releaseBody returns the account's in-flight slot when the response body is closed.
//
// Every upstream call site ends with defer resp.Body.Close(), so the slot is released
// together with the Body: a streaming response can last minutes, so the slot must not
// be returned as soon as the connection is established.
type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	b.once.Do(b.release)
	return b.ReadCloser.Close()
}

// pickFreeAccount scans the account pool for an account with a free in-flight slot.
//
// It cannot rely on max_retries greedy picks: under saturation those attempts mostly
// hit busy accounts, so the caller gets a 429 even though the pool still has free
// slots (docs/performance.md §14). The third return value reports whether the pool has
// any schedulable account, distinguishing "all busy" from "nothing to schedule"; the
// latter is left to noAccountError, which supplies an actionable reason
// (social_login / unverified...).
//
// Implementation notes: picking reads the store in a single pass (copying only the
// chosen account), and busy accounts are excluded by a pure predicate; acquireAccount
// only handles the narrow race between check and acquire, retrying after excluding
// the account on failure.
func (s *Server) pickFreeAccount(group string) (*deepai.Account, bool, bool) {
	// excluded is a slice rather than a map: retries only happen when check and
	// acquire collide, typically 0-1 times, so a slice saves one allocation on the
	// hot path that runs once per request.
	var excluded []string
	skip := func(a *deepai.Account) bool {
		for _, id := range excluded {
			if id == a.ID {
				return true
			}
		}
		return s.accountBusy(a.ID)
	}
	for {
		a, candidates := s.Store.PickAccount(group, skip)
		if a == nil {
			return nil, false, candidates
		}
		if s.acquireAccount(a.ID) {
			return a, true, true
		}
		// Race: someone else filled the account between the predicate seeing it idle
		// and our acquire. Exclude it and look at the next one.
		excluded = append(excluded, a.ID)
	}
}

// accountBusy reports whether the account's in-flight slots are full (read-only, no acquisition).
func (s *Server) accountBusy(id string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.inflight[id] >= maxInFlightPerAccount
}

// acquireAccount tries to take one of the account's in-flight slots; returns false when full.
func (s *Server) acquireAccount(id string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight == nil {
		s.inflight = map[string]int{}
	}
	if s.inflight[id] >= maxInFlightPerAccount {
		return false
	}
	s.inflight[id]++
	return true
}

// releaseAccount returns one in-flight slot.
func (s *Server) releaseAccount(id string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight[id] <= 0 {
		delete(s.inflight, id)
		return
	}
	s.inflight[id]--
	if s.inflight[id] == 0 {
		delete(s.inflight, id)
	}
}

// hasFreeAccount reports whether any account in the pool has a free in-flight slot.
//
// After a 409 it distinguishes "switch accounts immediately" from "the whole pool is
// busy, wait": the old code always slept for Retry-After before switching, wasting up
// to 30s in the worst case and eating into the non-streaming budget (90s). It performs
// a single pointer scan plus map lookups and does not copy the whole pool.
func (s *Server) hasFreeAccount() bool {
	return s.Store.AnySchedulable(func(a *deepai.Account) bool {
		return !s.accountBusy(a.ID)
	})
}

// errAllAccountsBusy indicates that every candidate account hit its concurrency cap.
//
// 429 rather than 502: this is retryable backpressure, and the client should retry later.
func errAllAccountsBusy() error {
	return &deepai.HTTPError{
		Op:     "账号调度",
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"message":"所有账号并发已满，请稍后重试","type":"rate_limit_error"}}`),
	}
}

// blockedReason appends the upstream error code to the account's last_err.
//
// Storing only the message loses the code: when the whole pool is blocked, the code is
// what distinguishes social_login_required from email_unverified, giving the caller an
// actionable error.
func blockedReason(he *deepai.HTTPError) string {
	if code := he.Code(); code != "" {
		return code + ": " + he.Error()
	}
	return he.Error()
}

// noAccountError produces an actionable error when no account is selectable.
//
// When the upstream asks every account in the pool to switch to Google/Apple login, it
// is not enough to reply "all accounts unavailable" - that is a policy change on the
// operator side, and the caller must see the reason.
func (s *Server) noAccountError() error {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return errors.New("没有可用账号，请先在控制台导入桌面端凭据或添加 API Key")
	}
	blocked := 0
	for _, a := range accts {
		if a.Status == deepai.StatusBlocked {
			blocked++
		}
	}
	if blocked > 0 && blocked == len(accts) {
		// When all are blocked, spell out the reason: a bare "all unavailable" makes
		// people think the gateway is broken.
		social, unverified := 0, 0
		for _, a := range accts {
			switch {
			case strings.Contains(a.LastErr, deepai.CodeSocialLogin):
				social++
			case strings.Contains(a.LastErr, deepai.CodeEmailUnverified):
				unverified++
			}
		}
		if unverified > 0 && social == 0 {
			return &deepai.HTTPError{
				Op:     "账号调度",
				Status: http.StatusServiceUnavailable,
				Body:   []byte(`{"error":{"message":"全部账号都被上游判定为邮箱未验证（email_unverified），请先验证邮箱后重试","type":"email_unverified","code":"email_unverified"}}`),
			}
		}
		if social > 0 {
			return &deepai.HTTPError{
				Op:     "账号调度",
				Status: http.StatusServiceUnavailable,
				Body:   []byte(`{"error":{"message":"全部账号已被上游要求改用 Google/Apple 登录（social_login_required）；邮箱密码注册的账号无法再推理","type":"social_login_required","code":"social_login_required"}}`),
			}
		}
		return &deepai.HTTPError{
			Op:     "账号调度",
			Status: http.StatusServiceUnavailable,
			Body:   []byte(`{"error":{"message":"全部账号都被上游拦截（策略 / 登录态），请到控制台查看各账号的 last_err","type":"account_blocked","code":"account_blocked"}}`),
		}
	}
	return errors.New("全部账号均不可用")
}
