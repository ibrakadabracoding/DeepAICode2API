package store

import (
	"time"
)

// --- Settings / Stats / Logs ---

// Settings returns a settings snapshot.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.st.Settings
	cp.ModelAliases = map[string]string{}
	for k, v := range s.st.Settings.ModelAliases {
		cp.ModelAliases[k] = v
	}
	return cp
}

// UpdateSettings partially updates settings.
func (s *Store) UpdateSettings(fn func(*Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st.Settings)
}

// statsDayLayout is the date key format for daily usage (local time zone).
const statsDayLayout = "2006-01-02"

// StatusClientGone is the log status code for client disconnected (following nginx's 499 convention).
//
// The value is unrelated to the HTTP spec (499 is not a standard code): it only appears in **our own request logs**,
// meaning the upstream did not fail; the caller left first-client disconnect, tab close, SDK timeout cancellation.
// The reason to distinguish it separately: such requests are neither successes nor failures of ours/the upstream; counting them in the failure rate would
// draw a fake downward line in the success rate, and would make it impossible to tell whether the upstream is actually stable.
const StatusClientGone = 499

// rollStatsDay resets the daily counters to zero and updates the date key when crossing days.
//
// Placed inside Record's write lock: it needs to modify Day and the three counters together; doing them separately would produce
// an intermediate state where the date has rolled over but the counters are still yesterday's. The caller must hold the s.mu write lock.
func rollStatsDay(st *Stats, now time.Time) {
	today := now.Format(statsDayLayout)
	if st.Day == today {
		return
	}
	st.Day = today
	st.DayTokensIn = 0
	st.DayTokensOut = 0
	st.DayCachedTokens = 0
}

// Stats returns a stats snapshot.
//
// The read path also performs a day-rollover check (modifying only the copy): after a restart, when no request has been made that day,
// it must not display yesterday's daily usage as today's.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	st := s.st.Stats
	s.mu.RUnlock()
	if st.Day != time.Now().Format(statsDayLayout) {
		st.Day = time.Now().Format(statsDayLayout)
		st.DayTokensIn = 0
		st.DayTokensOut = 0
		st.DayCachedTokens = 0
	}
	return st
}

// Logs returns the most recent logs.
func (s *Store) Logs(limit int) []LogEntry {
	s.mu.RLock()
	logs := s.logs
	s.mu.RUnlock()
	if logs == nil {
		return nil
	}
	return logs.tail(limit)
}

// Record records a request result and updates stats.
func (s *Store) Record(e LogEntry, outChars int) {
	s.mu.Lock()
	s.st.Stats.TotalRequests++
	// 499 means the client left first: it counts as neither success nor failure and is only kept in logs for troubleshooting.
	// Otherwise, a single tab-close action would inflate the production failure rate (in measured data, 3/4 of 502s are this kind).
	if (e.Status >= 400 || e.Err != "") && e.Status != StatusClientGone {
		s.st.Stats.FailedRequests++
	}
	s.st.Stats.TotalCharsOut += int64(outChars)
	s.st.Stats.TotalTokensIn += int64(e.TokensIn)
	s.st.Stats.TotalTokensOut += int64(e.TokensOut)
	s.st.Stats.TotalCachedTokens += int64(e.CachedTokens)
	// Daily usage: reset to zero first when crossing days (no scheduled task needed; checking on the read path is sufficient).
	rollStatsDay(&s.st.Stats, time.Now())
	s.st.Stats.DayTokensIn += int64(e.TokensIn)
	s.st.Stats.DayTokensOut += int64(e.TokensOut)
	s.st.Stats.DayCachedTokens += int64(e.CachedTokens)
	s.st.Stats.TotalCost += e.Cost
	s.statsDirty = true
	keep := s.st.Settings.LogKeep
	logs := s.logs
	s.mu.Unlock()

	// Log writes (including file IO) are outside the lock: they should not block account pool scheduling.
	if logs != nil {
		_ = logs.append(e, keep)
	}
}

// QueryLogs queries logs by criteria.
//
// Uses file scanning (reads at most the last 4MB), so it can trace back history beyond the in-memory ring buffer;
// Falls back to the in-memory ring buffer when the file is unreadable. Defaults to 200 when limit<=0.
func (s *Store) QueryLogs(f QueryFilter) []LogEntry {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	s.mu.RLock()
	logs := s.logs
	s.mu.RUnlock()
	if logs == nil {
		return nil
	}
	out, err := logs.scan(f)
	if err != nil {
		return logs.query(f)
	}
	return out
}

// ClearLogs clears logs (in-memory ring buffer and file).
func (s *Store) ClearLogs() {
	s.mu.RLock()
	logs := s.logs
	s.mu.RUnlock()
	if logs != nil {
		_ = logs.clear()
	}
}

// ClearStats resets statistics.
func (s *Store) ClearStats() {
	s.mu.Lock()
	started := s.st.Stats.StartedAt
	if started == 0 {
		started = time.Now().Unix()
	}
	// Keeps StartedAt (process start), records ResetAt separately: logs are not cleared,
	// The console needs to be able to explain "statistics count is less than log count".
	day := time.Now().Format(statsDayLayout)
	s.st.Stats = Stats{StartedAt: started, ResetAt: time.Now().Unix(), Day: day}
	s.statsDirty = true
	s.mu.Unlock()
}
