// Package pool implements the account pool's selection strategy (group filtering + failure-weighted
// rotation + quota awareness).
//
// These are business policies rather than persistence details; they used to live in the store
// package, which meant:
//   - unit tests had to construct a complete Store just to verify scheduling;
//   - tweaking rules like "when a 0 quota is considered stale" required touching the persistence
//     layer.
//
// After extraction, Scheduler holds no account list; callers pass a snapshot in while holding the lock.
package pool

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"deepaicode2api/internal/deepai"
)

// QuotaStaleAfter is the age after which a "0 quota" is considered stale and needs reprobing.
//
// Newly registered accounts are often still at 0 the moment they join the pool; the daily quota is
// only granted a few minutes later (see README §6.10, measured). Treat a stale 0 as "unknown", so
// an account is not permanently excluded from scheduling.
const QuotaStaleAfter = 15 * time.Minute

// Scheduler holds the rotation cursor and implements account pool selection.
//
// The cursor uses atomic rather than a plain int: selection is a per-request hot path, and with an
// atomic the caller (store) can select concurrently under a read lock without taking the write
// lock just to advance the cursor.
type Scheduler struct {
	rotate atomic.Int64
}

// New constructs a scheduler.
func New() *Scheduler { return &Scheduler{} }

// Pick selects an account from accounts by group and health; returns nil when none is available.
//
// An empty group means all groups. Selection has two rounds:
//  1. Consider only accounts with "unknown quota or quota > 0"-upstream grants a daily free quota
//     per machine, and the pool often has balance=0 accounts; hitting one first only gets rejected
//     once by upstream before rotating;
//  2. Then select from the full set, ensuring we never hard-block while an account exists.
//
// Within each round, prefer the account with the fewest failures; ties advance by the rotation cursor.
func (s *Scheduler) Pick(accounts []*deepai.Account, group string) *deepai.Account {
	return s.PickFiltered(accounts, group, nil)
}

// PickFiltered adds an extra filter predicate on top of Pick.
//
// skip is used for temporary exclusions unrelated to account health, such as "concurrent slots full"; it must be a **pure function**:
// If the first scan finds nothing, a second scan is performed; the same account may be queried twice.
func (s *Scheduler) PickFiltered(accounts []*deepai.Account, group string, skip func(*deepai.Account) bool) *deepai.Account {
	group = strings.TrimSpace(group)
	eligible := func(a *deepai.Account) bool {
		if !a.Enabled {
			return false
		}
		// Upstream policy interception (social_login_required) is an account-level terminal state:
		// Continuing rotation only causes every request to hit a 403 once.
		if a.Status == deepai.StatusBlocked {
			return false
		}
		// Daily quota exhausted (theos_exhausted): not scheduled until the reset time, to avoid every request
		// hitting a pointless 403. Automatically recovers after QuotaUntil; no manual intervention needed.
		if a.QuotaUntil > 0 && time.Now().Unix() < a.QuotaUntil {
			return false
		}
		if group != "" && !strings.EqualFold(a.GroupName(), group) {
			return false
		}
		return skip == nil || !skip(a)
	}
	if a := s.pick(accounts, eligible, true); a != nil {
		return a
	}
	return s.pick(accounts, eligible, false)
}

// HasEligible reports whether the pool contains any schedulable account (grouped / enabled / not intercepted),
// ignoring PickFiltered's skip. Callers use it to distinguish "all busy" from "no accounts in the pool".
func (s *Scheduler) HasEligible(accounts []*deepai.Account, group string) bool {
	group = strings.TrimSpace(group)
	now := time.Now().Unix()
	for _, a := range accounts {
		if a == nil || !a.Enabled || a.Status == deepai.StatusBlocked {
			continue
		}
		if a.QuotaUntil > 0 && now < a.QuotaUntil {
			continue
		}
		if group == "" || strings.EqualFold(a.GroupName(), group) {
			return true
		}
	}
	return false
}

// pick selects an account from eligible by "fewest failures + rotation".
//
// When needQuota is true, skip accounts known to have 0 quota.
func (s *Scheduler) pick(accounts []*deepai.Account, eligible func(*deepai.Account) bool, needQuota bool) *deepai.Account {
	n := len(accounts)
	if n == 0 {
		return nil
	}
	rot := int(s.rotate.Load())
	if rot < 0 || rot >= n {
		rot = 0
	}
	var best *deepai.Account
	bestIdx := -1
	for i := 0; i < n; i++ {
		idx := (rot + i) % n
		a := accounts[idx]
		if !eligible(a) {
			continue
		}
		if needQuota && !HasQuota(a) {
			continue
		}
		if best == nil || a.Failures < best.Failures {
			best = a
			bestIdx = idx
		}
		if a.Failures == 0 {
			s.rotate.Store(int64((idx + 1) % n))
			return a
		}
	}
	if bestIdx >= 0 {
		s.rotate.Store(int64((bestIdx + 1) % n))
	}
	return best
}

// HasQuota reports whether an account may still have available quota.
//
// Be conservative: only accounts that are "probed, enabled, and whose balance parses to 0" are considered exhausted,
// all others (not probed / unparseable / already paid) are treated as potentially available.
func HasQuota(a *deepai.Account) bool {
	if a == nil || a.Theos == nil || !a.Theos.Enabled {
		return true
	}
	bal := strings.TrimSpace(a.Theos.Balance)
	if bal == "" {
		return true
	}
	v, err := strconv.ParseFloat(bal, 64)
	if err != nil {
		return true
	}
	if v > 0 {
		return true
	}
	// 0 quota only counts when "just probed".
	if a.Theos.CheckedAt <= 0 {
		return true
	}
	return time.Since(time.Unix(a.Theos.CheckedAt, 0)) > QuotaStaleAfter
}
