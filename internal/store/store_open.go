package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/pool"
)

// Open loads (or initializes) the state file.
//
// The master key comes from DEEPAI2API_MASTER_KEY: when configured, written state
// files are encrypted as a whole; when not configured they stay plaintext (and a
// warning is printed in the startup log).
func Open(path string) (*Store, error) {
	key, err := MasterKeyFromEnv()
	if err != nil {
		return nil, err
	}
	return OpenWithKey(path, key)
}

// deriveSidePaths derives the side file paths from the main state file path.
func deriveSidePaths(path string) (statsPath, logsPath string) {
	base := path
	if trimmed, ok := strings.CutSuffix(path, ".json"); ok {
		base = trimmed
	}
	return base + "-stats.json", base + "-logs.jsonl"
}

// OpenWithKey loads the state file with the specified master key (key nil means
// no encryption).
//
// Besides the main file there are two side files (derived from the main path):
//   - <base>-stats.json   cumulative stats (a few hundred bytes, written every 30s)
//   - <base>-logs.jsonl   request logs (append-only, rotated by size)
//
// Older versions embedded logs / stats in the main file; on load they are
// automatically moved out and the main file is rewritten.
func OpenWithKey(path string, key []byte) (*Store, error) {
	statsPath, logsPath := deriveSidePaths(path)
	s := &Store{
		path: path, statsPath: statsPath, logsPath: logsPath,
		st: &State{Settings: DefaultSettings()}, masterKey: key, sched: pool.New(),
	}

	var (
		seedLogs    []LogEntry
		needMigrate bool
	)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		plain, encrypted, uerr := unseal(key, raw)
		if uerr != nil {
			return nil, uerr
		}
		s.encrypted = encrypted
		var legacy legacyState
		if err := json.Unmarshal(plain, &legacy); err != nil {
			return nil, fmt.Errorf("状态文件解析失败: %w", err)
		}
		s.st.Accounts = legacy.Accounts
		s.st.Keys = legacy.Keys
		s.st.Settings = legacy.Settings
		s.st.Models = legacy.Models
		s.st.DefaultModel = legacy.DefaultModel
		s.st.ModelsUpdated = legacy.ModelsUpdated
		s.st.MachineID = legacy.MachineID
		// Remember the original content so the first Save can skip it directly.
		s.lastSaved = append([]byte(nil), plain...)
		// Old format: logs / stats are still in the main file → migrate them out and
		// rewrite the main file later.
		if len(legacy.Logs) > 0 || legacy.Stats != (Stats{}) {
			s.st.Stats = legacy.Stats
			seedLogs = legacy.Logs
			needMigrate = true
		}
	case errors.Is(err, os.ErrNotExist):
		s.st.Stats.StartedAt = time.Now().Unix()
	default:
		return nil, err
	}

	s.applySettingsDefaults()

	// Side files are newer than the old embedded fields, so prefer them.
	_, statErr := os.Stat(s.statsPath)
	hadStatsFile := statErr == nil
	if err := s.loadStats(); err != nil {
		return nil, err
	}
	if s.st.Stats.StartedAt == 0 {
		s.st.Stats.StartedAt = time.Now().Unix()
	}
	if !hadStatsFile {
		// First run or migration from the old format: stats must be written to the
		// side file, otherwise they only live in memory and are lost on the next
		// restart.
		s.statsDirty = true
	}

	logs, err := openLogStore(logsPath, key, s.st.Settings.LogKeep)
	if err != nil {
		return nil, err
	}
	s.logs = logs
	if len(seedLogs) > 0 && len(logs.ring) == 0 {
		for _, e := range seedLogs {
			if err := logs.append(e, s.st.Settings.LogKeep); err != nil {
				return nil, err
			}
		}
	}

	if needMigrate {
		// Force a rewrite of the main file: strip logs / stats out of it.
		s.lastSaved = nil
		if err := s.Save(); err != nil {
			return nil, fmt.Errorf("迁移状态文件失败: %w", err)
		}
	}
	return s, nil
}

// applySettingsDefaults fills in settings that are missing or deprecated in old
// state files.
func (s *Store) applySettingsDefaults() {
	def := DefaultSettings()
	if s.st.Settings.ModelAliases == nil {
		s.st.Settings.ModelAliases = def.ModelAliases
	}
	if s.st.Settings.RequestTimeout == 0 {
		s.st.Settings.RequestTimeout = def.RequestTimeout
	}
	if s.st.Settings.MaxRetries == 0 {
		s.st.Settings.MaxRetries = def.MaxRetries
	}
	if s.st.Settings.LogKeep == 0 {
		s.st.Settings.LogKeep = def.LogKeep
	}
	if s.st.Settings.CORSOrigin == "*" {
		// The wildcard in historical state files no longer takes effect; tighten it
		// to local origins.
		s.st.Settings.CORSOrigin = ""
	}
}

// loadStats reads the stats side file; when missing, keeps the old embedded values.
func (s *Store) loadStats() error {
	raw, err := os.ReadFile(s.statsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	plain, _, uerr := unseal(s.masterKey, raw)
	if uerr != nil {
		return fmt.Errorf("统计文件解析失败: %w", uerr)
	}
	var st Stats
	if err := json.Unmarshal(plain, &st); err != nil {
		return fmt.Errorf("统计文件解析失败: %w", err)
	}
	s.st.Stats = st
	return nil
}

// Path returns the state file path.
func (s *Store) Path() string { return s.path }

// Save atomically writes the state back (the whole file is encrypted when a master key is configured).
//
// Split into three parts, each with its own independent "unchanged" check:
// 1. Main file (accounts/keys/settings/models): skipped if the content is unchanged;
// 2. Stats side file: written only when there are new requests, only a few hundred bytes;
// 3. Logs: handed off to logStore's buffered persist to disk.
//
// Serialization happens outside the lock: with 653 accounts in the account pool, a single marshal is about 1MB,
// doing this while holding the read lock would block the write lock on the request path (MarkAccountResult).
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snap := s.snapshotLocked()
	key := s.masterKey
	last := s.lastSaved
	enc := s.encrypted
	stats := s.st.Stats
	statsDirty := s.statsDirty
	logs := s.logs
	s.mu.RUnlock()

	plain, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}

	// 1) Main file. Must be rewritten once when the master key was just configured and the file on disk is still plaintext.
	if !bytes.Equal(plain, last) || (len(key) > 0 && !enc) {
		if err := writeAtomicSealed(s.path, plain, key); err != nil {
			return err
		}
		s.savesTotal.Add(1)
		s.savedBytes.Add(int64(len(plain)))
		s.mu.Lock()
		s.lastSaved = append(s.lastSaved[:0], plain...)
		if len(key) > 0 {
			s.encrypted = true
		}
		s.mu.Unlock()
	}

	// 2) Stats.
	if statsDirty {
		rawStats, err := json.Marshal(stats)
		if err != nil {
			return err
		}
		if err := writeAtomicSealed(s.statsPath, rawStats, key); err != nil {
			return err
		}
		s.clearStatsDirtyIfUnchanged(stats)
	}

	// 3) Log buffer.
	return logs.Flush()
}

// saveTimestampGranularity is the precision (in seconds) used when persisting runtime timestamps (LastUsed / Theos.CheckedAt) to disk.
//
// Both change with every request or every probe; second-level precision means the "skip if content unchanged" check never
// holds under sustained traffic: the state file for 653 accounts is about 950KB, and rewriting it every 30s means
// about 115MB/h of write amplification. Round down to 5 minutes when persisting to disk; in-memory values remain second-level;
// the only cost is that after a restart the "last used / quota probe time" precision drops to 5 minutes.
const saveTimestampGranularity = 300

// snapshotLocked builds the snapshot to persist. The caller must hold the read lock.
//
// The snapshot must deep-copy all fields that may be modified in place: Save's serialization happens outside the lock,
// reusing live pointers would make ToggleKey / DeleteKey collide with marshal (visible under -race).
func (s *Store) snapshotLocked() State {
	snap := *s.st
	snap.Accounts = make([]*deepai.Account, len(s.st.Accounts))
	for i, a := range s.st.Accounts {
		c := a.Clone()
		c.LastUsed = quantizeTimestamp(c.LastUsed)
		if c.Theos != nil {
			// Clone has already copied the Allowance struct; what is modified here is the snapshot's own copy.
			c.Theos.CheckedAt = quantizeTimestamp(c.Theos.CheckedAt)
		}
		snap.Accounts[i] = c
	}
	snap.Keys = make([]*APIKey, len(s.st.Keys))
	for i, k := range s.st.Keys {
		c := *k
		snap.Keys[i] = &c
	}
	snap.Models = cloneModels(s.st.Models)
	// ModelAliases is a map: make a deep copy so serialization outside the lock cannot race with concurrent writes.
	if s.st.Settings.ModelAliases != nil {
		snap.Settings.ModelAliases = maps.Clone(s.st.Settings.ModelAliases)
	}
	return snap
}

// cloneModels copies the model catalog (including the Input slice and TheosPerSecond map).
func cloneModels(models []deepai.Model) []deepai.Model {
	if models == nil {
		return nil
	}
	out := make([]deepai.Model, len(models))
	for i, m := range models {
		if m.Input != nil {
			m.Input = append([]string(nil), m.Input...)
		}
		if m.TheosPerSecond != nil {
			m.TheosPerSecond = maps.Clone(m.TheosPerSecond)
		}
		out[i] = m
	}
	return out
}

// quantizeTimestamp rounds the runtime timestamp down to disk-persistence precision; 0 / negative values are returned as-is.
func quantizeTimestamp(t int64) int64 {
	if t <= 0 {
		return t
	}
	return t - t%saveTimestampGranularity
}

// clearStatsDirtyIfUnchanged clears the dirty flag only when the written snapshot is still the current stats.
//
// A Record may be inserted between Save taking the snapshot and persisting to disk: directly setting it to false would swallow that increment,
// leaving the last stats entry only in memory (lost if the process exits afterward). All Stats fields are comparable,
// so use it to define the boundary precisely.
func (s *Store) clearStatsDirtyIfUnchanged(written Stats) {
	s.mu.Lock()
	if s.st.Stats == written {
		s.statsDirty = false
	}
	s.mu.Unlock()
}

// SaveCounters returns the actual number of writes to the main file and the plaintext byte count (for observing write amplification).
func (s *Store) SaveCounters() (saves, bytes int64) {
	return s.savesTotal.Load(), s.savedBytes.Load()
}

// writeAtomicSealed writes atomically using a unique temp file + fsync + rename, encrypting the whole file when necessary.
func writeAtomicSealed(path string, plain, key []byte) error {
	raw := plain
	if len(key) > 0 {
		sealed, err := seal(key, plain)
		if err != nil {
			return err
		}
		raw = sealed
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The temp file name must be unique: an early implementation always used <path>.tmp, so two writers
	// (concurrent Save, or an accidentally started second gateway instance) would truncate each other.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // After a successful rename, it is a no-op.

	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	// fsync before rename: otherwise a power loss may leave a file whose name has been swapped but whose contents are empty.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Encrypted reports whether the state file on disk is in encrypted format.
//
// Note: if a master key is configured but nothing has been saved yet, the file on disk is still plaintext,
// in which case this method returns false (use MasterKeySet to determine whether encryption has been configured).
func (s *Store) Encrypted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.encrypted
}

// MasterKeySet reports whether a master key has been configured (once configured, every Save encrypts).
func (s *Store) MasterKeySet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.masterKey) > 0
}
