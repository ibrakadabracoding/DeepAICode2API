package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// When Save persists to disk, LastUsed should be quantized to 5 minutes: it changes on every
// request, and second-level precision would defeat "skip if unchanged" under sustained traffic
// (each rewrite of the 653-account pool is about 950KB).
func TestSaveQuantizesLastUsed(t *testing.T) {
	st := newStore(t)
	acct := addAccount(t, st, "sk-quant", "default")
	now := time.Now().Unix()
	st.UpdateAccount(acct.ID, func(a *deepai.Account) {
		a.LastUsed = now
		a.Theos = &deepai.Allowance{Enabled: true, Balance: "0.00000", CheckedAt: now}
	})

	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatalf("读取状态文件: %v", err)
	}
	var disk struct {
		Accounts []struct {
			LastUsed int64 `json:"last_used"`
			Theos    struct {
				CheckedAt int64 `json:"checked_at"`
			} `json:"theos"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("解析状态文件: %v", err)
	}
	if len(disk.Accounts) != 1 {
		t.Fatalf("账号数 = %d", len(disk.Accounts))
	}
	got := disk.Accounts[0].LastUsed
	if got%saveTimestampGranularity != 0 {
		t.Fatalf("落盘 LastUsed = %d，未量化到 %ds", got, saveTimestampGranularity)
	}
	if got > now {
		t.Fatalf("落盘 LastUsed = %d 晚于当前时间 %d", got, now)
	}
	// The quota probe timestamp is quantized the same way: it changes on every probe,
	// otherwise write amplification comes right back.
	if ca := disk.Accounts[0].Theos.CheckedAt; ca%saveTimestampGranularity != 0 {
		t.Fatalf("落盘 checked_at = %d，未量化到 %ds", ca, saveTimestampGranularity)
	}
	// In memory, second-level precision is preserved.
	if mem := st.Account(acct.ID).LastUsed; mem != now {
		t.Fatalf("内存 LastUsed = %d，期望 %d", mem, now)
	}
}

// New requests within the same 5-minute window should not trigger a second rewrite of the main file.
func TestSaveSkipsRewriteWithinGranularityWindow(t *testing.T) {
	st := newStore(t)
	acct := addAccount(t, st, "sk-window", "default")
	base := time.Now().Unix() - time.Now().Unix()%saveTimestampGranularity
	st.UpdateAccount(acct.ID, func(a *deepai.Account) { a.LastUsed = base + 1 })
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	saves, _ := st.SaveCounters()

	st.UpdateAccount(acct.ID, func(a *deepai.Account) { a.LastUsed = base + 120 })
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got, _ := st.SaveCounters(); got != saves {
		t.Fatalf("同一窗口内又写了一次主文件：%d → %d", saves, got)
	}
}

// If a request advances stats after the snapshot is written, the dirty flag must not be cleared,
// otherwise the last stat delta only ever lives in memory.
func TestStatsDirtyKeptWhenRecordArrivesAfterSnapshot(t *testing.T) {
	st := newStore(t)

	written := st.Stats() // Mock the snapshot retrieved by Save
	st.Record(LogEntry{Status: 200}, 0)
	st.clearStatsDirtyIfUnchanged(written)

	st.mu.RLock()
	dirty := st.statsDirty
	st.mu.RUnlock()
	if !dirty {
		t.Fatal("快照之后到达的统计被清脏，最后一次自增不会落盘")
	}

	// The dirty flag may be cleared only when the snapshot matches the current values.
	st.clearStatsDirtyIfUnchanged(st.Stats())
	st.mu.RLock()
	dirty = st.statsDirty
	st.mu.RUnlock()
	if dirty {
		t.Fatal("统计未变化时脏标志应被清掉")
	}
}

func TestClearStatsKeepsStartedAtAndRecordsReset(t *testing.T) {
	st := newStore(t)
	st.Record(LogEntry{Status: 200}, 0)
	started := st.Stats().StartedAt
	time.Sleep(1100 * time.Millisecond) // StartedAt / ResetAt are both in seconds

	st.ClearStats()
	got := st.Stats()
	if got.StartedAt != started {
		t.Fatalf("StartedAt 被重置：%d → %d", started, got.StartedAt)
	}
	if got.ResetAt <= started {
		t.Fatalf("ResetAt = %d，应晚于 StartedAt = %d", got.ResetAt, started)
	}
	if got.TotalRequests != 0 {
		t.Fatalf("重置后 TotalRequests = %d", got.TotalRequests)
	}
}

// The snapshot must be isolated from live data: Save serializes outside the lock,
// so shared pointers would collide with in-place mutations.
func TestSnapshotIsolatesKeysAndModels(t *testing.T) {
	st := newStore(t)
	k1 := st.AddKey("a", "")
	k2 := st.AddKey("b", "")
	st.SetModels([]deepai.Model{{
		ID:             "m1",
		Input:          []string{"text"},
		TheosPerSecond: map[string]string{"out": "1"},
	}}, "m1")

	st.mu.RLock()
	snap := st.snapshotLocked()
	st.mu.RUnlock()

	// Changes made after the snapshot must not leak back into it.
	st.ToggleKey(k1.Key)
	st.DeleteKey(k2.Key)
	st.SetModels([]deepai.Model{{ID: "m2"}}, "m2")

	if len(snap.Keys) != 2 {
		t.Fatalf("快照密钥数 = %d，期望 2", len(snap.Keys))
	}
	if !snap.Keys[0].Enabled {
		t.Fatal("快照密钥被后来的 ToggleKey 改了")
	}
	if len(snap.Models) != 1 || snap.Models[0].ID != "m1" {
		t.Fatalf("快照模型被后来的 SetModels 改了: %#v", snap.Models)
	}
	if len(snap.Models[0].Input) != 1 || snap.Models[0].Input[0] != "text" {
		t.Fatalf("模型 Input 未深拷贝: %#v", snap.Models[0].Input)
	}
	if snap.Models[0].TheosPerSecond["out"] != "1" {
		t.Fatalf("模型 map 未深拷贝: %#v", snap.Models[0].TheosPerSecond)
	}
}

// Concurrency: key add/delete/toggle runs while Save is in progress (use -race to verify no shared writes).
func TestSaveWithConcurrentKeyMutations(t *testing.T) {
	st := newStore(t)
	st.AddKey("base", "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			k := st.AddKey("tmp", "")
			st.ToggleKey(k.Key)
			st.DeleteKey(k.Key)
		}
	}()
	for i := 0; i < 20; i++ {
		if err := st.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	<-done
}

func TestSaveCountersTrackMainFileWrites(t *testing.T) {
	st := newStore(t)
	addAccount(t, st, "sk-counter", "default")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	saves, written := st.SaveCounters()
	if saves != 1 || written <= 0 {
		t.Fatalf("首次保存计数 = (%d, %d)", saves, written)
	}
	// Content unchanged: should not write again.
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if again, _ := st.SaveCounters(); again != saves {
		t.Fatalf("无变化时仍写盘：%d → %d", saves, again)
	}
}

// The core promise of -seal: historical plaintext log lines must be sealed too, and remain readable after reopening.
func TestResealSealsLegacyPlaintextLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	key := bytes.Repeat([]byte{0x2a}, 32)

	// 1) First write plaintext state and plaintext logs in keyless mode (simulating historical data).
	plain, err := OpenWithKey(path, nil)
	if err != nil {
		t.Fatalf("OpenWithKey(nil): %v", err)
	}
	if plain.MasterKeySet() {
		t.Fatal("前置断言失败：无密钥模式不应报告已配置主密钥")
	}
	addAccount(t, plain, "sk-legacy", "default")
	plain.Record(LogEntry{Time: 1, Status: 200}, 0)
	if err := plain.Save(); err != nil {
		t.Fatalf("明文 Save: %v", err)
	}

	statsPath, logsPath := deriveSidePaths(path)
	extras := []string{
		logsPath,
		logsPath + ".1",
	}
	for _, p := range extras {
		if err := os.WriteFile(p, []byte(`{"time":2,"status":200}`+"\n"), 0o600); err != nil {
			t.Fatalf("写入明文日志 %s: %v", p, err)
		}
	}

	// 2) Reopen with the master key and seal.
	st, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("OpenWithKey(key): %v", err)
	}
	rep, err := st.Reseal()
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if rep.Accounts != 1 {
		t.Fatalf("Reseal 账号数 = %d", rep.Accounts)
	}
	if rep.SealedLines < 2 {
		t.Fatalf("封存的明文日志行 = %d，期望 ≥2", rep.SealedLines)
	}

	// 3) No file is plaintext anymore.
	assertSealedFile(t, path)
	assertSealedFile(t, statsPath)
	for _, p := range extras {
		assertSealedLines(t, p)
	}

	// 4) Historical logs are still readable after reopening (proving sealing did not corrupt the content).
	reopened, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	if logs := reopened.Logs(10); len(logs) < 1 {
		t.Fatalf("封存后日志丢失：%d 条", len(logs))
	}

	// 5) Idempotent: running it again should not "convert" any more lines.
	again, err := st.Reseal()
	if err != nil {
		t.Fatalf("二次 Reseal: %v", err)
	}
	if again.SealedLines != 0 {
		t.Fatalf("二次 Reseal 转换了 %d 行，应幂等", again.SealedLines)
	}
}

// Once encryption is enabled, logs must still be readable after a restart: this is the regression test
// for sealLine (single-line envelope).
//
// When writing logs with an indented envelope, one record is split across multiple lines,
// and the line-based decodeLine cannot parse it - the symptom is "logs vanish after a restart once encryption is on".
func TestEncryptedLogsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	key := bytes.Repeat([]byte{0x11}, 32)

	st, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("OpenWithKey: %v", err)
	}
	st.Record(LogEntry{Time: 100, Status: 200, Model: "glm-5.3-flash"}, 0)
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, logsPath := deriveSidePaths(path)
	assertSealedLines(t, logsPath)

	reopened, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	logs := reopened.Logs(10)
	if len(logs) != 1 {
		t.Fatalf("重启后日志条数 = %d，期望 1", len(logs))
	}
	if logs[0].Status != 200 || logs[0].Model != "glm-5.3-flash" {
		t.Fatalf("日志内容损坏: %#v", logs[0])
	}
}

func TestResealRequiresMasterKey(t *testing.T) {
	st := newStore(t) // no master key configured
	if _, err := st.Reseal(); err == nil {
		t.Fatal("未配置主密钥时 Reseal 应报错")
	}
}

func assertSealedFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	if !isEnvelope(raw) {
		t.Fatalf("%s 仍是明文", path)
	}
}

func assertSealedLines(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	for i, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !isEnvelope(line) {
			t.Fatalf("%s 第 %d 行仍是明文: %q", path, i+1, line)
		}
	}
}
