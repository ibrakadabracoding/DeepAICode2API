package server

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/parallel"
	"deepaicode2api/internal/pool"
	"deepaicode2api/internal/retry"
)

// --- Account quota probing ---

const (
	// probeWorkers is the concurrency limit for probing accounts in bulk.
	//
	// Probing 20 accounts serially takes up to 10 minutes in the worst case (30s timeout each), while full concurrency easily
	// turns the probe itself into an upstream risk-control signal; 4 is the empirically measured value that balances speed and restraint.
	probeWorkers = 4

	// quotaStaleAfter is the trigger threshold for "on-demand re-probing": for accounts in use, quota information
	// older than this is refreshed asynchronously. Aligned with pool.QuotaStaleAfter on the scheduling side -
	// the scheduler stops trusting balance=0 after the same threshold, so both sides must make consistent judgments.
	quotaStaleAfter = pool.QuotaStaleAfter

	// QuotaGuardInterval / QuotaGuardBatch define the cadence of the background quota guard.
	//
	// Only accounts whose refresh_at has passed / have never been probed / have not been probed for over 6 hours are selected; in steady state
	// almost no requests are generated; at the daily quota refresh moment, the entire pool expires at once, processed in batches of 24 per 5 minutes
	// in a queue (about 2.3 hours to cover 653 accounts). No full concurrent scan - that is an obvious
	// risk-control signal and would also turn the probe into traffic.
	QuotaGuardInterval = 5 * time.Minute
	QuotaGuardBatch    = 24

	// quotaGuardStaleAfter is the "too old" threshold for the background guard.
	// It is much looser than on-demand re-probing: active accounts are refreshed incidentally by the inference path, and the background only handles fallback.
	quotaGuardStaleAfter = 6 * time.Hour

	// maxCheckinBatch is the maximum number of accounts probed in a single /admin/api/checkin call.
	//
	// Chaining all 653 accounts takes 11 minutes, exceeding the patience of browsers / reverse proxies; so it is changed to batching +
	// remaining, and the console continues to the next batch based on remaining (checked_at of accounts probed in each batch
	// is pushed to now, so the next batch naturally picks up accounts that have not been probed).
	maxCheckinBatch = 200
)

// probeRetryDelays is the backoff sequence for probing quota before entering the pool.
//
// Pool entry is the only probing point that blocks the caller, and /theos failures are almost all egress jitter
// (No exit node / TLS handshake / unexpected EOF), one retry can recover the vast majority.
var probeRetryDelays = []time.Duration{time.Second}

// quotaRefreshAt parses refresh_at returned by the upstream; returns zero value when parsing fails.
func quotaRefreshAt(a *deepai.Account) time.Time {
	if a == nil || a.Theos == nil {
		return time.Time{}
	}
	raw := strings.TrimSpace(a.Theos.RefreshAt)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	// Tolerates variants without a timezone, treating them as UTC (the upstream currently always uses RFC3339).
	if t, err := time.Parse("2006-01-02T15:04:05", raw); err == nil {
		return t
	}
	return time.Time{}
}

// quotaNeedsRefresh reports whether the account's quota information is no longer trustworthy.
//
// staleAfter<=0 means strict mode: only recognizes "never probed" and "refresh_at has passed",
// Used for the stale_only filter in manual checkin (to avoid probing accounts that were just probed again).
func quotaNeedsRefresh(a *deepai.Account, now time.Time, staleAfter time.Duration) bool {
	if a == nil || !a.Enabled {
		return false
	}
	// Cooldown after a probe failure: failures often come in batches (exit node failure / upstream flakiness),
	// Without caching failed results, the background daemon would re-probe the entire same batch of failed accounts every 5 minutes.
	if a.ProbeAfter > now.Unix() {
		return false
	}
	if a.Theos == nil || a.Theos.CheckedAt <= 0 {
		return true
	}
	if rt := quotaRefreshAt(a); !rt.IsZero() && !now.Before(rt) {
		return true
	}
	if staleAfter > 0 && now.Sub(time.Unix(a.Theos.CheckedAt, 0)) >= staleAfter {
		return true
	}
	return false
}

// quotaDueRank gives the probe priority: 0 = refresh_at has passed (quota was just refreshed, highest priority to probe),
// 1 = never probed, 2 = simply too old.
func quotaDueRank(a *deepai.Account, now time.Time) int {
	if rt := quotaRefreshAt(a); !rt.IsZero() && !now.Before(rt) {
		return 0
	}
	if a.Theos == nil || a.Theos.CheckedAt <= 0 {
		return 1
	}
	return 2
}

// sortByQuotaDue puts the accounts most worth probing first (within the same rank, sorted by checked_at ascending).
//
// Batched probing relies on this order: when probing only the first N, probe those with "quota just refreshed" and "never probed" first,
// so that quota is not spent on accounts that are already the most up to date.
func sortByQuotaDue(accts []*deepai.Account, now time.Time) {
	sort.SliceStable(accts, func(i, j int) bool {
		ri, rj := quotaDueRank(accts[i], now), quotaDueRank(accts[j], now)
		if ri != rj {
			return ri < rj
		}
		return checkedAt(accts[i]) < checkedAt(accts[j])
	})
}

func checkedAt(a *deepai.Account) int64 {
	if a == nil || a.Theos == nil {
		return 0
	}
	return a.Theos.CheckedAt
}

// probeCooldownLeft returns the account's remaining probe cooldown (0 when there is no cooldown / it has expired).
//
// Only used for logging: to make "retry after %s" clear, so operators do not have to look up ProbeAfter themselves.
func probeCooldownLeft(a *deepai.Account) time.Duration {
	if a == nil || a.ProbeAfter <= 0 {
		return 0
	}
	left := time.Until(time.Unix(a.ProbeAfter, 0))
	if left <= 0 {
		return 0
	}
	// Logs do not need second-level precision; trimming the remainder makes them easier to read.
	return left.Round(time.Second)
}

// claimProbe claims the probe slot for an account; returns false when a probe is already running.
//
// Three paths may probe the same batch of accounts at the same time: manual checkin, delayed re-probe after entering the pool, and during inference
// on-demand re-probe. Without deduplication, they overlap and probe the same account three times in a row.
func (s *Server) claimProbe(id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probing == nil {
		s.probing = map[string]struct{}{}
	}
	if _, busy := s.probing[id]; busy {
		return false
	}
	s.probing[id] = struct{}{}
	return true
}

func (s *Server) releaseProbe(id string) {
	s.probeMu.Lock()
	delete(s.probing, id)
	s.probeMu.Unlock()
}

// probeOutcome is the probe result for a single account.
type probeOutcome struct {
	Account   *deepai.Account
	Allowance *deepai.Allowance
	Err       error
	// Skipped means a probe is already running for this account; this round did not actually request the upstream.
	Skipped bool
}

// probeBatch probes a batch of accounts with bounded concurrency; results correspond one-to-one with the input by index.
//
// When strict=true, failures are recorded into the account state (manual probe: the user wants to know which account is unusable);
// When strict=false, only successful results are trusted (automatic re-probe: a brief upstream hiccup should not mark an account that is currently serving
// inference as error, let alone accumulate Failures and skew scheduling).
func (s *Server) probeBatch(ctx context.Context, accts []*deepai.Account, timeout time.Duration, strict bool) []probeOutcome {
	out := make([]probeOutcome, len(accts))
	indexes := make([]int, 0, len(accts))
	for i, a := range accts {
		if a == nil {
			continue
		}
		out[i].Account = a
		if !s.claimProbe(a.ID) {
			out[i].Skipped = true
			continue
		}
		indexes = append(indexes, i)
	}
	if len(indexes) == 0 {
		return out
	}
	defer func() {
		for _, i := range indexes {
			s.releaseProbe(accts[i].ID)
		}
	}()

	parallel.For(ctx, probeWorkers, len(indexes), func(ctx context.Context, k int) {
		i := indexes[k]
		a := accts[i]
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// Global in-flight limit: manual batches, background daemon, and on-demand re-probes share 4 slots.
		select {
		case s.probeSem <- struct{}{}:
			defer func() { <-s.probeSem }()
		case <-cctx.Done():
			return
		}
		al, err := s.clientFor(a).Theos(cctx, a)
		out[i].Allowance, out[i].Err = al, err
		if err != nil && !strict {
			// Automatic batches must also leave backoff (nothing is persisted after returning here, otherwise the next
			// quotaNeedsRefresh will immediately pick this account out again for re-probe).
			s.Store.RecordProbeFailure(a.ID)
			return
		}
		// A policy block is not a "quota probe failure": persist it separately as blocked, and the scheduler will skip it.
		if deepai.IsSocialLoginRequired(err) {
			s.Store.BlockAccount(a.ID, err.Error())
			return
		}
		// Check-in being rate-limited does not mean the account is bad: inference works as usual, it just cannot check in again that day.
		// Lock the quota probe to keep the background daemon from hitting another 403 every 5 minutes.
		if deepai.IsCheckInRefused(err) {
			s.Store.LockAccountProbe(a.ID, time.Now().Add(deepai.CheckInRefusedLock), err.Error())
			return
		}
		s.Store.SetAccountAllowance(a.ID, al, err)
	})

	// When everything is skipped by dedup, there is nothing to persist.
	if len(indexes) > 0 {
		s.persist()
	}
	return out
}

// probeAccounts probes a batch of accounts and persists to disk (individual results are not of interest).
func (s *Server) probeAccounts(accts []*deepai.Account) {
	s.probeBatch(context.Background(), accts, 20*time.Second, true)
}

// probeAndSave asynchronously probes a single account and persists to disk.
func (s *Server) probeAndSave(a *deepai.Account) {
	s.probeAccounts([]*deepai.Account{a})
}

// probeAndSaveAsync probes an account in the background and persists to disk, registering this async work with quotaWG.
//
// Registration (Add) must happen before starting the goroutine: the probe writes memory first and then calls Save(); if a test only waits for
// the interface to return and exits midway, the background persist will race with t.TempDir() cleanup, showing up on CI as
// "TempDir RemoveAll cleanup: directory not empty".
func (s *Server) probeAndSaveAsync(a *deepai.Account) {
	if a == nil {
		return
	}
	s.quotaWG.Add(1)
	go func() {
		defer s.quotaWG.Done()
		s.probeAndSave(a)
	}()
}

// probeBatchAsync probes a batch of accounts in the background and persists to disk, registering with quotaWG.
//
// Administrative actions (such as assigning a machine fingerprint) change account state and trigger a re-probe; this probe
// is not within the request scope, so the process must wait for it to finish before persisting on exit.
func (s *Server) probeBatchAsync(accts []*deepai.Account) {
	if len(accts) == 0 {
		return
	}
	s.quotaWG.Add(1)
	go func() {
		defer s.quotaWG.Done()
		s.probeAccounts(accts)
	}()
}

// probeAsync asynchronously refreshes a single account's quota; returns whether a probe was actually initiated.
//
// Acquire the slot synchronously before starting the goroutine: otherwise every request would start a goroutine that exits immediately,
// which under high concurrency is a meaningless scheduling storm.
func (s *Server) probeAsync(a *deepai.Account) bool {
	if a == nil || !s.claimProbe(a.ID) {
		return false
	}
	// If the global slot cannot be acquired, give up: leave this batch of stale quotas for the next round (background daemon or next request),
	// without queuing on the hot path or piling up probes into a risk-control signature.
	select {
	case s.probeSem <- struct{}{}:
	default:
		s.releaseProbe(a.ID)
		return false
	}
	s.quotaWG.Add(1)
	go func() {
		defer s.quotaWG.Done()
		defer func() { <-s.probeSem }()
		defer s.releaseProbe(a.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		al, err := s.clientFor(a).Theos(ctx, a)
		if err != nil {
			// A failed automatic re-probe does not change account status: the inference path does not depend on this result,
			// and writing transient errors into Status/Failures would only cause scheduling misjudgment.
			// But social_login_required is a terminal policy state and must be persisted to disk so the scheduler stops.
			if deepai.IsSocialLoginRequired(err) {
				s.Store.BlockAccount(a.ID, err.Error())
				if saveErr := s.Store.Save(); saveErr != nil {
					log.Printf("[quota] 保存策略拦截状态失败: %v", saveErr)
				}
				log.Printf("[quota] %s 已被上游要求改用 Google/Apple 登录（social_login_required）", a.Name)
				return
			}
			// Backoff must be kept: otherwise the quota is always judged stale, and every request that selects this account will probe again,
			// turning the hot path into a /theos storm.
			s.Store.RecordProbeFailure(a.ID)
			if saveErr := s.Store.Save(); saveErr != nil {
				log.Printf("[quota] 保存探测退避失败: %v", saveErr)
			}
			log.Printf("[quota] %s 额度复探失败（不影响调度，%s 后再试）: %v",
				a.Name, probeCooldownLeft(s.Store.Account(a.ID)), err)
			return
		}
		s.Store.SetAccountAllowance(a.ID, al, nil)
		if err := s.Store.Save(); err != nil {
			log.Printf("[quota] 保存复探结果失败: %v", err)
		}
	}()
	return true
}

// ReprobeStale probes a batch of accounts whose quota information is too old, returning the number of probes actually initiated.
//
// limit<=0 means unlimited (for testing); in production, QuotaGuardBatch enforces the rate limit.
func (s *Server) ReprobeStale(limit int) int {
	now := time.Now()
	accts := s.Store.Accounts()
	due := make([]*deepai.Account, 0, len(accts))
	for _, a := range accts {
		if quotaNeedsRefresh(a, now, quotaGuardStaleAfter) {
			due = append(due, a)
		}
	}
	if len(due) == 0 {
		return 0
	}
	sortByQuotaDue(due, now)
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	out := s.probeBatch(context.Background(), due, 20*time.Second, false)
	probed := 0
	for _, o := range out {
		if o.Account != nil && !o.Skipped {
			probed++
		}
	}
	return probed
}

// probeAccountQuota probes one account's quota; retries once on transient errors (for pool admission only, blocking).
func (s *Server) probeAccountQuota(acct *deepai.Account) (*deepai.Allowance, error) {
	var al *deepai.Allowance
	err := retry.Do(context.Background(), probeRetryDelays, deepai.IsTransient, func(parent context.Context) error {
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		got, err := s.clientFor(acct).Theos(ctx, acct)
		if err != nil {
			return err
		}
		al = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return al, nil
}
