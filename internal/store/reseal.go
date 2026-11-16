package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ResealReport describes the result of sealing existing plaintext data.
type ResealReport struct {
	// Files are the files actually rewritten (missing files are skipped).
	Files []string
	// SealedLines is the number of historical plaintext log lines converted to ciphertext.
	SealedLines int
	// KeptLines is the number of unparseable lines kept as-is (neither decrypted nor discarded).
	KeptLines int
	// Accounts is the number of accounts in the main file, used to check that an empty pool was not written.
	Accounts int
}

// Reseal seals existing plaintext in the main file, the stats side file, and
// the logs with the current master key.
//
// Why it is needed: enabling encryption only affects data written afterwards.
// The main file and stats are fully rewritten as v2 on the next Save, but the
// request log is append-only - historical plaintext lines do not disappear just
// because a master key is configured (decodeLine tolerates mixed formats, so
// reads do not error either). Merely setting an environment variable would
// leave long-lived plaintext traces, so this explicitly rewrites them.
//
// Idempotent: already encrypted lines are kept as-is (but verified with the
// same key, so a key mismatch fails immediately). Requires stopping the running
// gateway first: this method renames the main file and the log file.
func (s *Store) Reseal() (ResealReport, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	if len(s.masterKey) == 0 {
		return ResealReport{}, fmt.Errorf("未设置 %s，无法加密", EnvMasterKey)
	}
	key := s.masterKey

	s.mu.RLock()
	snap := s.snapshotLocked()
	accounts := len(s.st.Accounts)
	stats := s.st.Stats
	s.mu.RUnlock()

	rep := ResealReport{Accounts: accounts}

	plain, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return rep, err
	}
	if err := writeAtomicSealed(s.path, plain, key); err != nil {
		return rep, fmt.Errorf("重写状态文件失败: %w", err)
	}
	s.savesTotal.Add(1)
	s.savedBytes.Add(int64(len(plain)))
	rep.Files = append(rep.Files, s.path)

	rawStats, err := json.Marshal(stats)
	if err != nil {
		return rep, err
	}
	if err := writeAtomicSealed(s.statsPath, rawStats, key); err != nil {
		return rep, fmt.Errorf("重写统计文件失败: %w", err)
	}
	rep.Files = append(rep.Files, s.statsPath)

	converted, kept, err := s.logs.reseal(key)
	if err != nil {
		return rep, err
	}
	rep.SealedLines += converted
	rep.KeptLines += kept
	if converted+kept > 0 {
		rep.Files = append(rep.Files, s.logsPath)
	}

	// Rotated files (<logs>.1) may also contain plaintext; handle them too and
	// skip if missing.
	rotated := s.logsPath + ".1"
	if c, k, existed, err := resealLogFile(rotated, key); err != nil {
		return rep, err
	} else if existed {
		rep.SealedLines += c
		rep.KeptLines += k
		rep.Files = append(rep.Files, rotated)
	}

	if err := verifySealedStateFile(s.path, key); err != nil {
		return rep, fmt.Errorf("封存校验失败: %w", err)
	}
	if err := verifySealedStateFile(s.statsPath, key); err != nil {
		return rep, fmt.Errorf("封存校验失败: %w", err)
	}
	if err := verifySealedLogFile(s.logsPath); err != nil {
		return rep, fmt.Errorf("封存校验失败: %w", err)
	}
	if err := verifySealedLogFile(rotated); err != nil {
		return rep, fmt.Errorf("封存校验失败: %w", err)
	}

	s.mu.Lock()
	s.lastSaved = append(s.lastSaved[:0], plain...)
	s.encrypted = true
	s.statsDirty = false
	s.mu.Unlock()

	return rep, nil
}

// reseal rewrites the log file and syncs the file size back into memory (used
// for rotation checks).
func (l *logStore) reseal(key []byte) (converted, kept int, err error) {
	if l == nil {
		return 0, 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	converted, kept, _, err = resealLogFile(l.path, key)
	if err != nil {
		return converted, kept, err
	}
	if fi, statErr := os.Stat(l.path); statErr == nil {
		l.size = fi.Size()
	} else {
		l.size = 0
	}
	return converted, kept, nil
}

// resealLogFile seals plaintext lines in a JSONL file one by one; existed
// indicates whether the file exists.
//
// Lines that cannot be parsed as JSON are kept as-is: better to keep one bad
// line than to drop an entire block of troubleshooting logs due to format
// noise. Already encrypted lines are first decrypted and verified with the same
// key, avoiding the "looks sealed but cannot be opened" situation after a key
// mismatch.
func resealLogFile(path string, key []byte) (converted, kept int, existed bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, true, err
	}

	var out bytes.Buffer
	out.Grow(len(raw))
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if isEnvelope(trimmed) {
			if _, _, uerr := unseal(key, trimmed); uerr != nil {
				return converted, kept, true, fmt.Errorf("%s: %w", path, uerr)
			}
			out.Write(trimmed)
			out.WriteByte('\n')
			continue
		}
		if !json.Valid(trimmed) {
			kept++
			out.Write(trimmed)
			out.WriteByte('\n')
			continue
		}
		sealed, serr := sealLine(key, trimmed)
		if serr != nil {
			return converted, kept, true, serr
		}
		out.Write(sealed)
		out.WriteByte('\n')
		converted++
	}

	// When the key is nil, writeAtomicSealed only performs the atomic write and
	// does not seal again.
	if err := writeAtomicSealed(path, out.Bytes(), nil); err != nil {
		return converted, kept, true, err
	}
	return converted, kept, true, nil
}

// verifySealedStateFile verifies whole-file JSON such as the main/stats files:
// it must be an envelope decryptable with the current key.
func verifySealedStateFile(path string, key []byte) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isEnvelope(raw) {
		return fmt.Errorf("%s 仍是明文", path)
	}
	if _, _, err := unseal(key, raw); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// verifySealedLogFile verifies a JSONL file: every line must be an envelope.
func verifySealedLogFile(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !isEnvelope(line) {
			return fmt.Errorf("%s 第 %d 行仍是明文", path, i+1)
		}
	}
	return nil
}
