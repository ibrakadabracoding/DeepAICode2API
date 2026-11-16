package deepai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// processStartedAt is the process start time, used for up=<seconds> in the heartbeat.
var processStartedAt = time.Now()

// checkInMinInterval is the minimum interval between two check-in attempts.
//
// The desktop app does not check in proactively before it receives check_in
// (it is triggered by account status refreshes); the gateway has no separate
// "status polling", so a lower bound on the first check-in avoids hitting a
// 403 on every hot-path request.
const checkInMinInterval = 30 * time.Second

// checkInAttemptTimeout is the **default** timeout budget for supplementary
// check-ins: check-in is only context, and failure does not affect inference.
//
// Measured in production (2026-11-13, v0.1.3, 82 minutes): check-in blocked
// inference 292 times, averaging 4.8s, with 53 hits at the 10s cap; it was the
// main source of first-byte latency. The budget can be lowered via
// Client.SetCheckInBudget (the console's "check-in blocking budget"); the
// default is kept unchanged so upgrading does not change behavior.
const checkInAttemptTimeout = 10 * time.Second

// CheckInRefusedLock is the silent lock duration after hitting check_in_refused.
//
// Once the upstream's daily "restart check-in" count is exhausted, the 403
// still carries a new nonce; without stopping, needAck would make
// EnsureCheckIn hit it again every 30s (checkInMinInterval), turning into a
// self-inflicted check-in storm. Locking for a while is far more restrained
// than "retry anyway after recognizing the error code". The upstream
// rejection message is "Try again tomorrow", so 6h is used here (consistent
// with the client's daily quota cadence); after it expires one more attempt is
// allowed, and if still refused the lock is renewed.
const CheckInRefusedLock = 6 * time.Hour

// newInstanceID generates a process-level instance ID (hex of a 16-byte random
// value, 32 characters).
func newInstanceID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(buf[:])
}

// UptimeSeconds returns the number of seconds the process has been running.
func UptimeSeconds() int { return int(time.Since(processStartedAt).Seconds()) }

type checkInState struct {
	checkIn   *CheckIn  // most recent check-in challenge issued by /theos
	noteAt    time.Time // when the challenge was received
	attemptAt time.Time // time of the most recent proactive check-in
	lastRelay time.Time // time of the most recent inference relay (heartbeat pi field)

	// ackedNonce is the nonce already relayed back to the upstream (for
	// diagnostics / tests).
	//
	// "Receiving a nonce" and "relaying the nonce back" are two different
	// things: empirically only the latter counts as a completed check-in, and
	// clients that don't complete it are switched by the upstream to the
	// stand-in model branch (docs/recon.md §5.1).
	ackedNonce string

	// ackedAt is when the last relay completed; the check-in cadence is based
	// on it.
	//
	// Production incident on 2026-11-13: the old implementation decided whether
	// a supplementary check-in was needed via "latest nonce != acked nonce", but
	// the upstream issues a **new** nonce on every /theos call, so the condition
	// became true again after each relay; the desktop's seconds/2 (default 300s)
	// cadence was bypassed entirely and degenerated to one call per
	// checkInMinInterval (30s), adding 292 synchronous blocks in production.
	ackedAt time.Time

	// refusedUntil is the silent lock for check_in_refused (zero value = unlocked).
	//
	// An account rate-limited per day by the upstream cannot get the real model
	// branch that day, but inference still works; continuing to hit /theos only
	// wastes requests and may deepen risk control.
	refusedUntil time.Time
}

type checkInStore struct {
	mu sync.Mutex
	m  map[string]*checkInState
}

func newCheckInStore() *checkInStore {
	return &checkInStore{m: map[string]*checkInState{}}
}

// checkInKey distinguishes check-in state by API Key: the same Client is reused
// by multiple accounts (bucketed only by proxy), so check-in challenges must be
// isolated per account.
func checkInKey(acct *Account) string {
	if acct == nil {
		return ""
	}
	return acct.APIKey
}

func (s *checkInStore) state(key string) *checkInState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[key]
	if st == nil {
		st = &checkInState{}
		s.m[key] = st
	}
	return st
}

func (c *Client) stateFor(acct *Account) *checkInState {
	if c.checkIns == nil {
		c.checkIns = newCheckInStore()
	}
	return c.checkIns.state(checkInKey(acct))
}

// captureCheckIn extracts check_in from the /theos response body (present on
// both success and rejection).
func (c *Client) captureCheckIn(acct *Account, body []byte) {
	var payload struct {
		CheckIn *CheckIn `json:"check_in"`
	}
	if json.Unmarshal(body, &payload) != nil || !payload.CheckIn.Valid() {
		return
	}
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	defer c.checkIns.mu.Unlock()
	cp := *payload.CheckIn
	st.checkIn = &cp
	st.noteAt = time.Now()
}

// CheckIn returns the cached check-in challenge (for tests / console diagnostics).
func (c *Client) CheckIn(acct *Account) *CheckIn {
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	defer c.checkIns.mu.Unlock()
	if st.checkIn == nil {
		return nil
	}
	cp := *st.checkIn
	return &cp
}

// markAcked records the acked nonce and ack time (called only when /theos
// actually sent it).
func (c *Client) markAcked(acct *Account, nonce string) {
	if nonce == "" {
		return
	}
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	st.ackedNonce = nonce
	st.ackedAt = time.Now()
	c.checkIns.mu.Unlock()
}

// markRelay records an inference relay time; the heartbeat's pi field uses it
// to tell whether work happened recently.
func (c *Client) markRelay(acct *Account) {
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	st.lastRelay = time.Now()
	c.checkIns.mu.Unlock()
}

// lockCheckIn puts a silent lock on an account's check-in (used when
// check_in_refused is hit).
func (c *Client) lockCheckIn(acct *Account, d time.Duration) {
	if acct == nil || d <= 0 {
		return
	}
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	st.refusedUntil = time.Now().Add(d)
	c.checkIns.mu.Unlock()
}

// CheckInRefusedUntil returns the expiry time of the account's check-in silent
// lock (zero value = unlocked).
//
// For console diagnostics / tests: a locked account no longer hits /theos that
// day, but inference works as usual.
func (c *Client) CheckInRefusedUntil(acct *Account) time.Time {
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	defer c.checkIns.mu.Unlock()
	return st.refusedUntil
}

// applyCheckInHeaders writes the check-in nonce and heartbeat (used only by /theos).
func (c *Client) applyCheckInHeaders(req *http.Request, acct *Account) {
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	ci := st.checkIn
	relayed := st.lastRelay
	c.checkIns.mu.Unlock()

	ttl := 600
	if ci != nil && ci.Seconds > 0 {
		ttl = ci.Seconds
	}
	if ci != nil && ci.Nonce != "" {
		req.Header.Set(HeaderCheckIn, ci.Nonce)
	}
	pi := 0
	if !relayed.IsZero() && time.Since(relayed) < time.Duration(ttl)*time.Second {
		pi = 1
	}
	// Same shape as the desktop's deepAIBeacon: v=<version>;up=<seconds>;pi=<0|1>.
	//
	// The desktop app only appends `;focus=<0|1>` when the focus state is known
	// (deepAIBeacon simply omits unknown facts). The gateway is a server process
	// with no window focus, so this field is not faked.
	req.Header.Set(HeaderBeacon, fmt.Sprintf("v=%s;up=%d;pi=%d", AppVersion, UptimeSeconds(), pi))
}

// CheckInOutcome is the result of one EnsureCheckIn, for callers to record
// metrics / logs.
type CheckInOutcome struct {
	// Attempted reports whether /theos was actually called this time.
	Attempted bool
	// Waited is the synchronous wait time (0 when Attempted is false).
	Waited time.Duration
	// TimedOut reports whether this wait was cut short by the budget (the
	// upstream did not respond within it).
	TimedOut bool
	// Err is the raw /theos error (403 sign-out, timeout, etc.; callers must not
	// reject inference based on it).
	Err error
}

// EnsureCheckIn performs one supplementary check-in before inference
// (corresponds to the desktop's ensureCheckedIn).
//
// The cadence matches the desktop: based on the time the last relay completed,
// the upstream is not disturbed within seconds/2 (default 300s); if no relay
// has ever completed (cold start / new account), check in immediately once,
// with attemptAt enforcing the minimum interval. No failure blocks inference:
// check-in is a side condition, not admission control.
func (c *Client) EnsureCheckIn(ctx context.Context, acct *Account) CheckInOutcome {
	var out CheckInOutcome
	if acct == nil {
		return out
	}
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	ci, attemptAt, ackedAt := st.checkIn, st.attemptAt, st.ackedAt
	refusedUntil := st.refusedUntil
	c.checkIns.mu.Unlock()

	now := time.Now()
	// An account locked by check_in_refused does not check in again that day:
	// inference can still use the stand-in branch, and continuing to hit /theos
	// only wastes time and risk-control budget (see CheckInRefusedLock).
	if !refusedUntil.IsZero() && now.Before(refusedUntil) {
		return out
	}
	// Already relayed: throttle per the desktop's half-life cadence (seconds/2).
	if ci != nil && ci.Valid() && !ackedAt.IsZero() {
		ttl := time.Duration(ci.Seconds) * time.Second
		if now.Sub(ackedAt) < ttl/2 {
			return out
		}
	}
	// Minimum-interval fallback: even with a long half-life (e.g. seconds=600)
	// it won't fire repeatedly.
	if !attemptAt.IsZero() && now.Sub(attemptAt) < checkInMinInterval {
		return out
	}
	c.checkIns.mu.Lock()
	st.attemptAt = now
	c.checkIns.mu.Unlock()

	out.Attempted = true
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, c.checkInBudget())
	defer cancel()
	// The result (including 403) never affects inference; the challenge has
	// already been captured in captureCheckIn.
	_, out.Err = c.Theos(cctx, acct)
	out.Waited = time.Since(start)
	out.TimedOut = errors.Is(out.Err, context.DeadlineExceeded)
	return out
}

// checkInBudget returns the wait budget for synchronous check-in.
func (c *Client) checkInBudget() time.Duration {
	if c.CheckInBudget > 0 {
		return c.CheckInBudget
	}
	return checkInAttemptTimeout
}

// SetCheckInBudget sets the wait budget for synchronous check-in before
// inference (<=0 means use the default 10s).
//
// Lowering it does not make check-in fail: once the budget is exceeded,
// inference is released with the old nonce and the next request continues the
// catch-up.
func (c *Client) SetCheckInBudget(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.CheckInBudget = d
}
