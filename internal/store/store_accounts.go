package store

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Account ---

// Accounts returns a list of account snapshots (each item is a deep copy).
func (s *Store) Accounts() []*deepai.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*deepai.Account, len(s.st.Accounts))
	for i, a := range s.st.Accounts {
		out[i] = a.Clone()
	}
	return out
}

// Account looks up an account by ID and returns a deep-copy snapshot.
//
// Does not return live pointers from the account pool: account state is concurrently modified by scheduling, background probing, and management APIs,
// handing out live pointers is equivalent to pushing locking responsibility onto every caller. When fields need to be modified in place, use
// UpdateAccount / UpdateAccounts (completed under the write lock).
func (s *Store) Account(id string) *deepai.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			return a.Clone()
		}
	}
	return nil
}

// AccountPage is the result of a paginated account query.
type AccountPage struct {
	Items []*deepai.Account
	Total int // Total count after filtering (unpaginated)
}

// AccountCounts returns the total account count and ready count without unpacking snapshots.
//
// The console's first screen needs to display "ready/total" without syncing the entire account list,
// whereas Accounts() deep-copies every account.
//
// For the definition of "ready", see deepai.AccountReady: error is considered unhealthy only while the error is still fresh,
// blocked is never considered ready (it will not self-heal). The old implementation's `status != "error"` would
// permanently count a probabilistic EOF as unhealthy, and would also count blocked as ready.
func (s *Store) AccountCounts() (total, ready int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	for _, a := range s.st.Accounts {
		if deepai.AccountReady(a, now) {
			ready++
		}
	}
	return len(s.st.Accounts), ready
}

// AccountQuery describes an account list query.
type AccountQuery struct {
	Offset int
	Limit  int
	Group  string
	Status string
	Query  string
	// Sort takes account / group / source / plan / theos / status / failures / last_used;
	// An empty string means keep the original account pool order.
	Sort string
	// Desc true means descending order.
	Desc bool
}

// AccountPage filters by conditions, sorts, and then returns paginated account snapshots.
//
// The console renders only a few dozen rows per screen, while a full account snapshot is often hundreds of KB;
// Pagination reduces the transfer volume from O(all accounts) to O(single page).
//
// Sorting must be done here rather than on the frontend: the list is server-side paginated, so sorting only the current page will produce
// incorrect results (the first item on page 2 may be smaller than the last item on page 1).
// Equal values uniformly fall back to ID, ensuring entries do not jump between pages when paginating.
//
// offset<0 is treated as 0; limit<=0 uses the default 50; limit>200 is capped at 200.
func (s *Store) AccountPage(q AccountQuery) AccountPage {
	offset := q.Offset
	limit := q.Limit
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	group := strings.TrimSpace(q.Group)
	status := strings.ToLower(strings.TrimSpace(q.Status))
	query := strings.ToLower(strings.TrimSpace(q.Query))

	s.mu.RLock()
	defer s.mu.RUnlock()

	matched := make([]*deepai.Account, 0, len(s.st.Accounts))
	for _, a := range s.st.Accounts {
		if group != "" && !strings.EqualFold(a.GroupName(), group) {
			continue
		}
		if status != "" && !strings.EqualFold(a.Status, status) {
			continue
		}
		if query != "" && !accountMatches(a, query) {
			continue
		}
		matched = append(matched, a)
	}

	sortAccounts(matched, q.Sort, q.Desc)

	total := len(matched)
	if offset >= total {
		return AccountPage{Items: nil, Total: total}
	}
	end := offset + limit
	if end > total {
		end = total
	}
	items := make([]*deepai.Account, 0, end-offset)
	for _, a := range matched[offset:end] {
		items = append(items, a.Clone())
	}
	return AccountPage{Items: items, Total: total}
}

// sortAccounts sorts by the specified field; unknown fields keep the original order.
func sortAccounts(xs []*deepai.Account, key string, desc bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || len(xs) < 2 {
		return
	}
	cmp, ok := accountComparator(key)
	if !ok {
		return
	}
	sort.SliceStable(xs, func(i, j int) bool {
		c := cmp(xs[i], xs[j])
		if c == 0 {
			return xs[i].ID < xs[j].ID // Stable fallback
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
}

// accountComparator returns an ascending comparison function for a field; for unknown fields it returns ok=false.
func accountComparator(key string) (func(a, b *deepai.Account) int, bool) {
	cmpStr := func(a, b string) int { return strings.Compare(a, b) }
	cmpInt := func(a, b int64) int {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	}
	switch key {
	case "account", "name", "email":
		return func(a, b *deepai.Account) int { return cmpStr(accountLabel(a), accountLabel(b)) }, true
	case "group":
		return func(a, b *deepai.Account) int { return cmpStr(a.GroupName(), b.GroupName()) }, true
	case "source":
		return func(a, b *deepai.Account) int { return cmpStr(a.Source, b.Source) }, true
	case "plan":
		return func(a, b *deepai.Account) int { return cmpStr(accountPlanLabel(a), accountPlanLabel(b)) }, true
	case "theos":
		return func(a, b *deepai.Account) int { return cmpInt(quotaValue(a), quotaValue(b)) }, true
	case "status":
		return func(a, b *deepai.Account) int { return cmpStr(a.Status, b.Status) }, true
	case "failures":
		return func(a, b *deepai.Account) int {
			return cmpInt(int64(a.Failures), int64(b.Failures))
		}, true
	case "last_used":
		return func(a, b *deepai.Account) int { return cmpInt(a.LastUsed, b.LastUsed) }, true
	}
	return nil, false
}

// accountLabel is the display name of an account (email first, then name, and finally ID).
func accountLabel(a *deepai.Account) string {
	if a == nil {
		return ""
	}
	switch {
	case a.Email != "":
		return a.Email
	case a.Name != "":
		return a.Name
	default:
		return a.ID
	}
}

// accountPlanLabel is the display name of the plan (consistent with the frontend).
func accountPlanLabel(a *deepai.Account) string {
	if a == nil {
		return ""
	}
	if a.IsPro {
		return "pro"
	}
	if a.Plan != "" {
		return a.Plan
	}
	return "free"
}

// quotaValue parses the Theos balance into a numeric value; unknown/unparseable values are treated as -1 (sorted first in ascending order).
func quotaValue(a *deepai.Account) int64 {
	if a == nil || a.Theos == nil {
		return -1
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(a.Theos.Balance), 64)
	if err != nil {
		return -1
	}
	return int64(v * 1000) // Keep 3 decimal places to avoid floating-point jitter in comparisons
}

// accountMatches performs substring matching on ID / name / email / username / group / proxy.
func accountMatches(a *deepai.Account, kw string) bool {
	for _, field := range []string{a.ID, a.Name, a.Email, a.Username, a.GroupName(), a.Proxy, a.Fingerprint} {
		if strings.Contains(strings.ToLower(field), kw) {
			return true
		}
	}
	return false
}

// UpdateAccount modifies an account in place under the write lock; returns whether the account exists.
func (s *Store) UpdateAccount(id string, fn func(*deepai.Account)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			fn(a)
			return true
		}
	}
	return false
}

// UpdateAccounts iterates over all accounts in bulk under the write lock.
//
// fn returning true means the account was modified this time; the return value is the number of modifications.
func (s *Store) UpdateAccounts(fn func(*deepai.Account) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.st.Accounts {
		if fn(a) {
			n++
		}
	}
	return n
}

// UpsertAccount deduplicates by API Key fingerprint or email before writing the account, and returns whether it was newly added.
//
// When the same account logs in again, the API Key may remain unchanged, but when re-registering/re-running, the email is the same,
// the Key may have been rotated; secondary matching by email prevents the same account from appearing twice in the account pool.
func (s *Store) UpsertAccount(a *deepai.Account) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ex := range s.st.Accounts {
		if ex.KeyID() != a.KeyID() && !sameEmail(ex.Email, a.Email) {
			continue
		}
		a.ID = ex.ID
		// Runtime state prefers the old value, while identity fields prefer the new value (falling back on empty values) - see MergeFrom.
		a.MergeFrom(ex)
		s.st.Accounts[i] = a.Clone()
		return false
	}
	s.st.Accounts = append(s.st.Accounts, a.Clone())
	return true
}

func sameEmail(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

// RemoveAccount deletes an account.
func (s *Store) RemoveAccount(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.st.Accounts {
		if a.ID == id {
			s.st.Accounts = append(s.st.Accounts[:i], s.st.Accounts[i+1:]...)
			return true
		}
	}
	return false
}

// PickAccount selects an available account within the specified group using rotation.
//
// Scheduling rules (group / quota / failure weighting / rotation) still live in internal/pool; this only handles
// one read lock + one scan.
//
// Hot path once per request: this performs only one read lock + one scan, and **deep-copies only the selected account**.
// The old version had the caller loop over "Accounts() (deep copy of the entire pool) + NextAccountGroup (write lock + another copy)"
// implementation; with 653 accounts it was O(n²) copies and one write lock per round.
//
// skip is an optional temporary exclusion predicate (e.g. "concurrency slots full") and must be a pure function.
// The second return flag indicates "whether the pool has any schedulable account (ignoring skip)",
// so the caller can distinguish "all busy" from "no accounts at all".
func (s *Store) PickAccount(group string, skip func(*deepai.Account) bool) (*deepai.Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acct := s.sched.PickFiltered(s.st.Accounts, group, skip)
	if acct == nil {
		return nil, s.sched.HasEligible(s.st.Accounts, group)
	}
	return acct.Clone(), true
}

// AnySchedulable reports whether there is a schedulable account satisfying ok (enabled, not blocked),
// without unpacking the snapshot or deep-copying.
func (s *Store) AnySchedulable(ok func(*deepai.Account) bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.st.Accounts {
		if a == nil || !a.Enabled || a.Status == deepai.StatusBlocked {
			continue
		}
		if ok == nil || ok(a) {
			return true
		}
	}
	return false
}

// NextAccountGroup is compatible with old call sites: equivalent to the success branch of PickAccount(group, nil).
func (s *Store) NextAccountGroup(group string) *deepai.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acct := s.sched.Pick(s.st.Accounts, group)
	if acct == nil {
		return nil
	}
	return acct.Clone()
}

// MarkAccountResult records the result of one account call.
//
// Whether to mark the account as error on failure is determined by the transient check (see MarkAccountFailure):
// This entry point is reserved for callers that have already determined the failure should be recorded (e.g. cases without an error object).
func (s *Store) MarkAccountResult(id string, ok bool, note string) {
	s.markAccountResult(id, ok, note, false)
}

// MarkAccountFailure records a failure with an error object.
//
// Transient failures (EOF / 429 / 5xx / capacity rejection / check-in rate limiting) only increment Failures and
// last_err, and **do not touch status**: whether the next request succeeds has nothing to do with this one; a single blip should not
// make the account show as "unhealthy" in the console for a long time (docs/changes.md §45).
func (s *Store) MarkAccountFailure(id string, err error) {
	if err == nil {
		return
	}
	// Context canceled = we abandoned this request ourselves (client disconnected / upper layer wrapping up),
	// not an account problem. Recording it would only mix noise unrelated to the upstream into failures,
	// and failures is the first thing the console looks at to judge "is this account broken".
	if errors.Is(err, context.Canceled) {
		return
	}
	s.markAccountResult(id, false, err.Error(), deepai.Transient(err))
}

func (s *Store) markAccountResult(id string, ok bool, note string, transient bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		now := time.Now().Unix()
		a.LastUsed = now
		if ok {
			a.Failures = 0
			a.Status = "ok"
			a.LastErr = ""
			a.LastErrAt = 0
			// Inference success means the quota has recovered (or this 403 is historical noise).
			a.QuotaUntil = 0
			// Note: ProbeAfter is not cleared here - inference success does not mean /theos will succeed.
			// The accounts rate-limited on check-in are exactly the "can infer, cannot check in" ones; clearing the cooldown would make the background guardian
			// hit the 403 again every minute. After a successful probe, SetAccountAllowance clears it.
			if note != "" {
				a.Note = note
			}
			return
		}
		a.Failures++
		a.LastErr = note
		a.LastErrAt = now
		if !transient {
			a.Status = deepai.StatusError
		}
		return
	}
}

// RecoverStaleErrors changes error accounts that "have not failed again for a long time" back to ok, returning the number of changes.
//
// Why a one-shot scan is needed: status is the basis for display and sorting; relying only on AccountReady's lazy evaluation
// would make the red dots in the account list disagree with the top badge. The background ticks every 30s (O(n), same order as the number of accounts).
//
// blocked is left alone: it does not self-heal and must wait for a successful /theos or manual handling.
func (s *Store) RecoverStaleErrors(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	recovered := 0
	for _, a := range s.st.Accounts {
		if a.Status != deepai.StatusError {
			continue
		}
		if now.Sub(time.Unix(a.LastErrAt, 0)) < deepai.ErrorStaleAfter {
			continue
		}
		// Keep last_err / failures as history; only change "whether it is healthy now" back.
		a.Status = "ok"
		recovered++
	}
	return recovered
}

// BlockAccount marks the account as "upstream policy blocked" (current source: social_login_required).
//
// Difference from an ordinary failure: this is not a network/quota problem, and retrying with another account will not save it. The scheduler skips
// blocked until a successful /theos sets it back to ok (see SetAccountAllowance).
func (s *Store) BlockAccount(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastUsed = time.Now().Unix()
		a.Status = deepai.StatusBlocked
		a.LastErr = reason
		if a.Failures < 1 {
			a.Failures = 1
		}
		return
	}
}

// MarkQuotaExhausted marks the account as "daily quota used up"; the scheduler skips it until the given time.
//
// Difference from BlockAccount: this is a self-healing state (recovers automatically when due), requires no manual handling,
// and the console should not display it as "policy blocked".
func (s *Store) MarkQuotaExhausted(id string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.QuotaUntil = until.Unix()
		a.LastErr = "当日额度用完（" + until.Format(time.RFC3339) + " 恢复）"
		a.LastErrAt = time.Now().Unix()
		return
	}
}

// ClearQuotaExhausted clears the quota-exhausted mark (called when a probe detects the balance has recovered).
func (s *Store) ClearQuotaExhausted(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID == id {
			a.QuotaUntil = 0
			return
		}
	}
}

// SetAccountAllowance caches the result of one quota probe.
func (s *Store) SetAccountAllowance(id string, al *deepai.Allowance, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastUsed = time.Now().Unix()
		if err != nil {
			// Transient errors (EOF / 429 / check-in rate limiting) should not mark the account as error:
			// Probe failures are often widespread egress flakiness, not the account itself being broken.
			if !deepai.Transient(err) {
				a.Status = deepai.StatusError
			}
			a.LastErr = err.Error()
			a.LastErrAt = time.Now().Unix()
			a.Failures++
			bumpProbeBackoff(a)
			return
		}
		a.Theos = al
		a.Status = "ok"
		a.LastErr = ""
		a.LastErrAt = 0
		a.Failures = 0
		a.ProbeFailures = 0
		a.ProbeAfter = 0
		return
	}
}

// RecordProbeFailure records one "automatic re-probe" failure: it only advances probe backoff and does not change scheduling state.
//
// Difference from the error branch of SetAccountAllowance: there, **someone explicitly wants to know**
// The probe result path (manual checkin / pool-entry liveness probe) must persist failures to Status / Failures;
// This comes from on-demand re-probing on the inference path; a single egress flake should not make the scheduler judge the account as broken,
// nor should it overwrite the more specific reason on the console (notes such as social_login).
//
// But the backoff **must** remain: without recording ProbeAfter, quotaNeedsRefresh always returns true,
// every request that selects this account will probe again - measured in production, the same account failed continuously
// 323 times/hour (see docs/changes.md §61), and the probe itself becomes an upstream risk-control signature.
func (s *Store) RecordProbeFailure(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		bumpProbeBackoff(a)
		return
	}
}

// bumpProbeBackoff advances the account's probe backoff (caller holds the lock).
func bumpProbeBackoff(a *deepai.Account) {
	a.ProbeFailures++
	a.ProbeAfter = time.Now().Add(probeBackoff(a.ProbeFailures)).Unix()
}

// probeBackoff returns the cooldown duration after consecutive probe failures: 2min → 10min → 1h (capped).
//
// When failure results are not cached, the background daemon re-probes the entire same batch of failed accounts every 5 minutes;
// and failures are often widespread (egress failure / upstream flakiness); at a scale of 653 accounts, this is a continuous
// /theos storm. This is the same constraint as "check-in is a limited resource" in docs/recon.md.
func probeBackoff(consecutive int) time.Duration {
	switch {
	case consecutive <= 1:
		return 2 * time.Minute
	case consecutive == 2:
		return 10 * time.Minute
	default:
		return time.Hour
	}
}

// LockAccountProbe adds a silent lock to the account's quota probe (current source: check_in_refused).
//
// Difference from ordinary failures: the upstream is not broken and the account can still perform inference; it is just that the day's "restart check-in" count is exhausted;
// Locking the probe prevents the background daemon and pre-inference check-in from repeatedly hitting 403.
func (s *Store) LockAccountProbe(id string, until time.Time, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.st.Accounts {
		if a.ID != id {
			continue
		}
		a.LastUsed = time.Now().Unix()
		if until.Unix() > a.ProbeAfter {
			a.ProbeAfter = until.Unix()
		}
		if note != "" {
			a.LastErr = note
		}
		return
	}
}
