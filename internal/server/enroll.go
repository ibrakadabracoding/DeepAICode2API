package server

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// markAccountVerified marks an account as email-verified (locating it by ID or email).
func (s *Server) markAccountVerified(id, email string) error {
	// Idempotent: setting it to true repeatedly is harmless.
	mark := func(a *deepai.Account) { a.EmailVerified = true }

	// Try the ID first; on a re-run the ID in the ledger may be stale, then fall back
	// to the email.
	if id = strings.TrimSpace(id); id != "" && s.Store.UpdateAccount(id, mark) {
		return s.Store.Save()
	}
	if want := strings.TrimSpace(email); want != "" {
		for _, a := range s.Store.Accounts() {
			if strings.EqualFold(a.Email, want) && s.Store.UpdateAccount(a.ID, mark) {
				return s.Store.Save()
			}
		}
	}
	return errors.New("账号不存在")
}

// enrollRegistration is the only write path into the pool (shared by the admin API
// and browser login).
func (s *Server) enrollRegistration(reg deepai.Registration, al *deepai.Allowance) (string, bool, error) {
	acct, err := deepai.AccountFromRegistration(reg)
	if err != nil {
		return "", false, err
	}
	now := time.Now().Unix()
	acct.Status = "ok"
	acct.LastUsed = now
	acct.LastVerified = now
	acct.Failures = 0
	acct.LastErr = ""
	if al != nil {
		acct.Theos = al
	}

	isNew := s.Store.UpsertAccount(acct)
	// Upsert preserves the old runtime state; write this verification result back
	// explicitly.
	s.Store.UpdateAccount(acct.ID, func(stored *deepai.Account) {
		stored.Status = "ok"
		stored.Failures = 0
		stored.LastErr = ""
		stored.LastUsed = now
		stored.LastVerified = now
		stored.EmailVerified = reg.EmailVerified
		stored.Plan = reg.Plan
		if al != nil {
			stored.Theos = al.Clone()
		}
	})
	if err := s.Store.Save(); err != nil {
		return "", isNew, err
	}

	// A freshly registered account often has not received its daily quota from the
	// upstream yet: /theos returns balance=0 at this point, but the same key queried a
	// few minutes later returns 200. Without a re-probe, this 0 is cached as fact and
	// the scheduler will skip the account for a long time.
	if al == nil || !positiveQuota(al) {
		s.scheduleQuotaReprobe(acct.ID)
	}
	return acct.ID, isNew, nil
}

// positiveQuota reports whether the quota is definitively greater than 0.
//
// Unparseable / missing fields are both treated as "unknown" (return false): better to probe one extra time,
// than to declare a usable account dead.
func positiveQuota(al *deepai.Allowance) bool {
	if al == nil || !al.Enabled {
		return false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(al.Balance), 64)
	return err == nil && v > 0
}

// allowanceFromEnroll builds the quota snapshot carried when the account enters the pool.
//
// The upstream often returns balance=0 right after an account is created (the daily quota is issued later). This 0 is not
// "confirmed exhausted" but "unknown": caching it as a confirmed value makes scheduling exclude it in the first round, for up to 15 minutes
// (R2 measured: load testing immediately after a new batch enters the pool, only 12~15 of 50 accounts could be selected,
// the data plane covered only 19/50). So when quota is 0, leave CheckedAt at 0 and let pool.HasQuota
// pass it through as "unknown"; if there really is no quota the upstream will reject, and failed accounts are automatically down-weighted.
func allowanceFromEnroll(balance, granted, refreshAt string) *deepai.Allowance {
	al := &deepai.Allowance{Enabled: true, Balance: balance, Granted: granted, RefreshAt: refreshAt}
	if !positiveQuota(al) {
		return al
	}
	al.CheckedAt = time.Now().Unix()
	return al
}

// quotaReprobeDelays is the re-probe backoff sequence for 0 quota after entering the pool.
//
// Measured (R2 load test): at the moment of entering the pool, only 12~15 of 50 new accounts had quota;
// the original 3/6/12 minute sequence did not reach 47/50 until the 9th minute. After changing to 45s/2min/6min,
// the "schedulable" gap shrank from 9 minutes to under 1 minute; stop as soon as a positive quota is obtained.
var quotaReprobeDelays = []time.Duration{45 * time.Second, 2 * time.Minute, 6 * time.Minute}

// scheduleQuotaReprobe asynchronously re-probes an account's quota (only when it is still 0).
//
// One goroutine per account, with a capped number of attempts, so it does not amplify itself.
func (s *Server) scheduleQuotaReprobe(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	delays := s.quotaReprobeDelays
	if len(delays) == 0 {
		return
	}
	s.quotaWG.Add(1)
	go func() {
		defer s.quotaWG.Done()
		for _, d := range delays {
			// Interruptible by shutdown: the backoff is at most 12 minutes and must not hold up process exit.
			if !s.sleepOrStop(d) {
				return
			}
			a := s.Store.Account(id)
			if a == nil {
				return // already deleted
			}
			if positiveQuota(a.Theos) {
				return // probed by another path in the meantime
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			al, err := s.clientFor(a).Theos(ctx, a)
			cancel()
			s.Store.SetAccountAllowance(id, al, err)
			if err := s.Store.Save(); err != nil {
				log.Printf("[quota] 保存复探结果失败: %v", err)
			}
			if err == nil && positiveQuota(al) {
				log.Printf("[quota] %s 复探拿到额度: balance=%s granted=%s", a.Name, al.Balance, al.Granted)
				return
			}
		}
	}()
}

// waitQuotaReprobe blocks until all scheduled background persist-to-disk work (quota re-probe / pool-entry liveness probe) has finished.
//
// Both kinds of goroutine first write the quota into memory, then Save() to disk; if a test only waits for "quota updated"
// Return immediately; background persistence to disk will race with t.TempDir() cleanup and occur intermittently on CI
// "TempDir RemoveAll cleanup: directory not empty" (more likely to occur under -race).
func (s *Server) waitQuotaReprobe() { s.quotaWG.Wait() }
