package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// logMaxBytes triggers rotation once the file exceeds this size: the current
// file is renamed to .1 and the old .1 is discarded.
//
// Logs are only used for post-mortem troubleshooting, so keeping one
// generation of rotated files is enough; there is no unbounded growth.
const logMaxBytes = 8 << 20

// logStore is an append-only request log file (JSONL) plus an in-memory ring
// buffer.
//
// It was split out of the main state file because the log changes on every
// request; keeping it inside State would permanently defeat Save's
// "skip if content unchanged" check - the background save every 30s would
// degrade into rewriting the entire 1.1MB state file plus fsync (measured at
// roughly 130MB/hour of write amplification).
//
// Writes use "open+write+close per append" rather than a long-lived handle:
//   - a long-lived handle prevents temp directories/log files from being
//     deleted or renamed on Windows;
//   - the gateway's request rate is single-digit to tens of rps, so the cost of
//     one open/close is negligible;
//   - no buffer flush logic is needed, and the most recent logs are not lost on
//     a crash.
//
// When a master key is configured, lines are encrypted one by one: one seal
// envelope per line, using the same key as the main state file.
type logStore struct {
	path string
	key  []byte

	mu   sync.Mutex
	size int64 // Current file size (used for rotation checks, initialized on open)
	ring []LogEntry
	keep int
}

// openLogStore opens (or creates) the log file and loads the most recent keep
// entries into the in-memory ring buffer.
func openLogStore(path string, key []byte, keep int) (*logStore, error) {
	l := &logStore{path: path, key: key, keep: keep}
	if fi, err := os.Stat(path); err == nil {
		l.size = fi.Size()
	}
	if err := l.loadTail(); err != nil {
		return nil, err
	}
	return l, nil
}

// loadTail reads the most recent keep entries from the file to fill the ring buffer.
func (l *logStore) loadTail() error {
	if l.keep <= 0 {
		return nil
	}
	entries, err := l.scan(QueryFilter{Limit: l.keep})
	if err != nil {
		return err
	}
	l.ring = entries
	return nil
}

// append appends one log entry: writes the file and updates the ring buffer.
//
// keep is passed in by the caller based on the current setting (LogKeep can be
// adjusted live from the console).
func (l *logStore) append(e LogEntry, keep int) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(l.key) > 0 {
		sealed, err := sealLine(l.key, raw)
		if err != nil {
			return err
		}
		raw = sealed
	}
	raw = append(raw, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.writeLocked(raw); err != nil {
		return err
	}
	l.ring = append(l.ring, e)
	if keep > 0 && len(l.ring) > keep {
		l.ring = l.ring[len(l.ring)-keep:]
	}
	return nil
}

// writeLocked appends one line to the file (caller holds the lock).
func (l *logStore) writeLocked(raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	if l.size >= logMaxBytes {
		_ = os.Remove(l.path + ".1")
		if err := os.Rename(l.path, l.path+".1"); err == nil {
			l.size = 0
			l.ring = nil
		}
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	n, werr := f.Write(raw)
	cerr := f.Close()
	l.size += int64(n)
	if werr != nil {
		return werr
	}
	return cerr
}

// Flush is a no-op kept for compatibility with old call sites: writes are not buffered.
func (l *logStore) Flush() error { return nil }

// tail returns the most recent limit log entries (limit<=0 means all).
func (l *logStore) tail(limit int) []LogEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.ring)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]LogEntry, limit)
	copy(out, l.ring[n-limit:])
	return out
}

// query filters the in-memory ring buffer (fast path, covers the most recent
// keep entries).
func (l *logStore) query(f QueryFilter) []LogEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LogEntry, 0, 64)
	for i := len(l.ring) - 1; i >= 0; i-- {
		if !f.match(l.ring[i]) {
			continue
		}
		out = append(out, l.ring[i])
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	reverseLogEntries(out)
	return out
}

// clear clears the in-memory buffer and truncates the file.
func (l *logStore) clear() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring = nil
	l.size = 0
	if err := os.Truncate(l.path, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// scan scans the log file and applies the filter (newest to oldest, at most
// limit entries).
//
// Used to query history beyond the in-memory ring buffer: it only reads the
// tail of the file (up to 4MB) so a large file scan does not slow down the
// admin API.
func (l *logStore) scan(f QueryFilter) ([]LogEntry, error) {
	const maxScan = 4 << 20

	fh, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()

	fi, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	start := fi.Size() - maxScan
	if start < 0 {
		start = 0
	}
	if _, err := fh.Seek(start, 0); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	out := make([]LogEntry, 0, 64)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// The start offset may cut a line in half: skip anything that is not valid JSON.
		e, ok := l.decodeLine([]byte(line))
		if !ok || !f.match(e) {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	reverseLogEntries(out)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// decodeLine parses one line (unseals first when a master key is configured).
func (l *logStore) decodeLine(raw []byte) (LogEntry, bool) {
	var e LogEntry
	body := raw
	if len(l.key) > 0 && isEnvelope(raw) {
		plain, _, err := unseal(l.key, raw)
		if err != nil {
			return e, false
		}
		body = plain
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return e, false
	}
	return e, true
}

func reverseLogEntries(xs []LogEntry) {
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
}

// QueryFilter describes a log query.
//
// It only applies to the in-memory ring buffer (the most recent LogKeep entries)
// or the tail of the file, avoiding a long transaction on the admin API. All
// conditions are combined with AND.
type QueryFilter struct {
	// Limit is the maximum number of entries to return (0 means unlimited; the caller applies a fallback).
	Limit int
	// Since keeps only logs after this unix second (0 means no limit).
	Since int64
	// Endpoint / Account filter by exact match (empty string means no limit).
	Endpoint string
	Account  string
	// MinStatus keeps only logs with status >= MinStatus (0 means no limit).
	MinStatus int
	// Keyword does a case-insensitive substring match against model and err.
	Keyword string
}

func (f QueryFilter) match(e LogEntry) bool {
	if f.Since > 0 && e.Time < f.Since {
		return false
	}
	if f.Endpoint != "" && e.Endpoint != f.Endpoint {
		return false
	}
	if f.Account != "" && e.Account != f.Account {
		return false
	}
	if f.MinStatus > 0 && e.Status < f.MinStatus {
		return false
	}
	if f.Keyword != "" {
		kw := strings.ToLower(f.Keyword)
		if !strings.Contains(strings.ToLower(e.Model), kw) &&
			!strings.Contains(strings.ToLower(e.Err), kw) {
			return false
		}
	}
	return true
}
