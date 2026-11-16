package deepai

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The account health determination is right here.
//
// Background (docs/changes.md §45): `status` is the **result of the last request**, not the account's
// permanent attribute. Upstream EOF / 429 "Too many requests today" occur per request; as long as
// every failure is written as status=error and never cleared, the badge will stay at 1/16 for a long time,
// while the account pool is actually working normally the whole time. So:
//
// - Transient first distinguishes "this attempt failed" from "this account is broken";
// - AccountReady then factors "whether the error is recent enough" into health (ErrorStaleAfter).

// Transient reports whether a failure belongs to the "transient" category: it only means this request failed,
// not that the account is broken (it should not be marked as error, nor used as a basis for health).
//
// Classified as transient: network-layer errors (EOF / reset / timeout), 429 rate limiting, 5xx, 409 session_busy
// (capacity rejection), check_in_refused (inference works, only the daily check-in limit is reached).
// Not transient: 401 / invalid Key, social_login_required, email_unverified,
// and **daily quota exhausted** (`Too many requests today`) - the latter means "today it truly cannot
// be used", and should be shown as degraded truthfully, not erased as noise.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		// Quota exhaustion takes precedence: even if it returns as 429, it is not "just take a break".
		if isDailyQuota(string(he.Body)) {
			return false
		}
		switch {
		case he.Status == http.StatusTooManyRequests:
			return true
		case he.Status >= 500:
			return true
		case he.Status == http.StatusConflict && he.Code() == CodeSessionBusy:
			// Capacity rejection: only visible when the whole pool is full, unrelated to the account itself.
			return true
		case he.Code() == CodeCheckInRefused:
			// Check-in rate limiting: inference works as usual, only the daily check-in quota is exhausted.
			return true
		}
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true // Timeout / temporary network error
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Upstream occasionally puts "restarting / gateway down" into the message instead of the status code,
	// This kind cannot be determined by type alone; use text as fallback. Note: quota exhaustion does not count.
	text := strings.ToLower(err.Error())
	if isDailyQuota(text) {
		return false
	}
	for _, needle := range []string{
		"unexpected eof",
		"connection reset",
		"broken pipe",
		"temporarily unavailable",
		"bad gateway",
		"service unavailable",
		"gateway timeout",
		"i/o timeout",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// isDailyQuota recognizes "daily quota exhausted"-style messages.
//
// This kind of failure does not self-heal within the day (it only resets at refresh_at), so it cannot be treated as transient noise:
// The account pool must be able to tell that "this account is unusable today".
func isDailyQuota(text string) bool {
	return strings.Contains(strings.ToLower(text), "too many requests today")
}

// AccountReady reports whether the account counts as "ready" at time now.
//
// Rules:
// - Disabled accounts do not count;
// - blocked (blocked by upstream policy) does not count: it will not self-heal and requires manual handling;
// - status=error but the error has not recurred for longer than ErrorStaleAfter → counts as ready
// (that is historical noise, and the scheduler has never skipped error accounts anyway);
// - Everything else (ok / unknown / new error) counts as ready; unknown means "not probed yet",
// and a freshly imported account should not be counted as bad.
func AccountReady(a *Account, now time.Time) bool {
	if a == nil || !a.Enabled || a.Status == StatusBlocked {
		return false
	}
	// Daily quota exhausted: excluded from scheduling until QuotaUntil (403 theos_exhausted).
	if a.QuotaUntil > 0 && now.Unix() < a.QuotaUntil {
		return false
	}
	if a.Status != StatusError {
		return true
	}
	return now.Sub(time.Unix(a.LastErrAt, 0)) >= ErrorStaleAfter
}

// AccountDegraded reports whether the account has "failed recently" (error and the error is still fresh).
//
// health uses it to produce a second number: how many accounts besides the ready ones are currently failing. Showing only ready/total
// would conflate "just got hit by a 429" with "actually broken".
func AccountDegraded(a *Account, now time.Time) bool {
	if a == nil || !a.Enabled || a.Status != StatusError {
		return false
	}
	return now.Sub(time.Unix(a.LastErrAt, 0)) < ErrorStaleAfter
}
