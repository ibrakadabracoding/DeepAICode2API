package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/pool"
)

// newStore opens an empty state file in a temporary directory.
func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

func addAccount(t *testing.T, st *Store, key, group string) *deepai.Account {
	t.Helper()
	a := &deepai.Account{
		ID:      deepai.ShortHash(key)[:8],
		Name:    key,
		APIKey:  key,
		Group:   group,
		Enabled: true,
	}
	if !st.UpsertAccount(a) {
		t.Fatalf("账号 %s 应为新增", key)
	}
	return a
}

func TestOpenCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := st.Settings()
	if s.RequestTimeout <= 0 || s.MaxRetries <= 0 || s.LogKeep <= 0 {
		t.Fatalf("默认设置未填充: %#v", s)
	}
	if st.Stats().StartedAt == 0 {
		t.Fatal("StartedAt 应被初始化")
	}
	if st.HasKeys() {
		t.Fatal("新状态不应有密钥")
	}
	if got := st.Accounts(); len(got) != 0 {
		t.Fatalf("新状态不应有账号: %d", len(got))
	}
}

func TestOpenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("损坏的状态文件应报错，而不是静默重置")
	}
}

func TestSaveAndReopenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	addAccount(t, st, "sk-aaa", "g1")
	k := st.AddKey("console", "")
	st.SetMachineID("machine-1")
	st.SetModels([]deepai.Model{{ID: "m1", Name: "M1"}}, "m1")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The state file must be readable and writable only by the owner (Unix only: on Windows the mode bits of os.WriteFile
	// have no effect, and ACLs cannot be narrowed via chmod, so encryption is needed as a fallback).
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil {
			if perm := fi.Mode().Perm(); perm&0o077 != 0 {
				t.Fatalf("状态文件权限过宽: %o", perm)
			}
		}
	}

	back, err := Open(path)
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	if len(back.Accounts()) != 1 || back.Accounts()[0].APIKey != "sk-aaa" {
		t.Fatalf("账号未持久化: %#v", back.Accounts())
	}
	if !back.ValidKey(k.Key) {
		t.Fatal("密钥未持久化")
	}
	if back.MachineID() != "machine-1" {
		t.Fatalf("机器指纹未持久化: %q", back.MachineID())
	}
	if back.DefaultModel() != "m1" {
		t.Fatalf("模型目录未持久化: %q", back.DefaultModel())
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	addAccount(t, st, "sk-1", "")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The temporary file must have been renamed away; no .tmp may be left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("残留临时文件: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("状态文件不是合法 JSON:\n%s", raw)
	}
}

func TestUpsertDedupesByKeyAndEmail(t *testing.T) {
	st := newStore(t)
	a := addAccount(t, st, "sk-same", "")

	// Importing the same Key again → update rather than insert, and the ID stays unchanged.
	again := &deepai.Account{APIKey: "sk-same", Name: "renamed"}
	if st.UpsertAccount(again) {
		t.Fatal("同 Key 应为更新")
	}
	if again.ID != a.ID {
		t.Fatalf("更新后 ID 应保持 %q，实际 %q", a.ID, again.ID)
	}
	if got := len(st.Accounts()); got != 1 {
		t.Fatalf("账号数 = %d，期望 1", got)
	}

	// Same email, Key rotated → still treated as the same account.
	rotated := &deepai.Account{APIKey: "sk-new", Email: "x@y.com"}
	st.UpsertAccount(rotated)
	sameEmailAcct := &deepai.Account{APIKey: "sk-newer", Email: "X@Y.com"}
	if st.UpsertAccount(sameEmailAcct) {
		t.Fatal("同邮箱（大小写不同）应为更新")
	}
	if got := len(st.Accounts()); got != 2 {
		t.Fatalf("账号数 = %d，期望 2", got)
	}
}

func TestUpsertPreservesRuntimeState(t *testing.T) {
	st := newStore(t)
	a := addAccount(t, st, "sk-1", "")
	st.MarkAccountResult(a.ID, false, "boom")
	st.SetAccountAllowance(a.ID, &deepai.Allowance{Balance: "3"}, nil)

	// Re-importing (e.g. the desktop client switched Keys) must not clear the failure count or the quota cache.
	st.UpsertAccount(&deepai.Account{APIKey: "sk-1", Name: "n"})
	got := st.Account(a.ID)
	if got == nil {
		t.Fatal("账号丢失")
	}
	if got.LastErr != "" {
		t.Fatalf("SetAccountAllowance 成功应清空 LastErr，实际 %q", got.LastErr)
	}
	if got.Theos == nil || got.Theos.Balance != "3" {
		t.Fatalf("配额缓存丢失: %#v", got.Theos)
	}
}

func TestNextAccountRotationIsFair(t *testing.T) {
	st := newStore(t)
	for _, k := range []string{"sk-a", "sk-b", "sk-c"} {
		addAccount(t, st, k, "")
	}

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		a := st.NextAccountGroup("")
		if a == nil {
			t.Fatal("应有可用账号")
		}
		seen[a.APIKey]++
	}
	// Rotation: 30 requests should be split evenly across 3 accounts.
	for _, k := range []string{"sk-a", "sk-b", "sk-c"} {
		if seen[k] != 10 {
			t.Errorf("账号 %s 被选中 %d 次，期望 10；分布=%v", k, seen[k], seen)
		}
	}
}

func TestNextAccountSkipsFailedAndDisabled(t *testing.T) {
	st := newStore(t)
	good := addAccount(t, st, "sk-good", "")
	bad := addAccount(t, st, "sk-bad", "")
	off := addAccount(t, st, "sk-off", "")

	st.MarkAccountResult(bad.ID, false, "api down")
	// Consistent with the enable/disable of /admin/api/accounts: write paths all go through UpdateAccount.
	st.UpdateAccount(off.ID, func(x *deepai.Account) { x.Enabled = false })

	for i := 0; i < 10; i++ {
		if got := st.NextAccountGroup(""); got == nil || got.ID != good.ID {
			t.Fatalf("应只选到健康账号，实际 %#v", got)
		}
	}
}

func TestNextAccountFallsBackWhenAllFailed(t *testing.T) {
	// When the whole group fails, do not return nil: better to send the request out than to hard-block.
	st := newStore(t)
	a := addAccount(t, st, "sk-1", "")
	addAccount(t, st, "sk-2", "")
	st.MarkAccountResult(a.ID, false, "x")
	st.MarkAccountResult(a.ID, false, "x")

	if got := st.NextAccountGroup(""); got == nil {
		t.Fatal("全部失败时应回退到失败最少的账号，而不是 nil")
	}
}

func TestNextAccountGroupIsolation(t *testing.T) {
	st := newStore(t)
	addAccount(t, st, "sk-g1", "g1")
	addAccount(t, st, "sk-g2", "g2")

	for i := 0; i < 5; i++ {
		if got := st.NextAccountGroup("g1"); got == nil || got.GroupName() != "g1" {
			t.Fatalf("g1 分组取到了 %#v", got)
		}
	}
	if got := st.NextAccountGroup("missing"); got != nil {
		t.Fatalf("不存在的分组应返回 nil，实际 %#v", got)
	}
	// An empty group is equivalent to "all groups".
	if got := st.NextAccountGroup(""); got == nil {
		t.Fatal("空分组应匹配全部账号")
	}
}

func TestNextAccountEmptyPool(t *testing.T) {
	st := newStore(t)
	if got := st.NextAccountGroup(""); got != nil {
		t.Fatalf("空号池应返回 nil，实际 %#v", got)
	}
}

func TestMarkAccountResultResetsFailures(t *testing.T) {
	st := newStore(t)
	a := addAccount(t, st, "sk-1", "")
	st.MarkAccountResult(a.ID, false, "boom")
	st.MarkAccountResult(a.ID, false, "boom2")
	if got := st.Account(a.ID); got.Failures != 2 || got.Status != "error" || got.LastErr != "boom2" {
		t.Fatalf("失败计数错误: %#v", got)
	}

	st.MarkAccountResult(a.ID, true, "")
	if got := st.Account(a.ID); got.Failures != 0 || got.Status != "ok" || got.LastErr != "" {
		t.Fatalf("成功后应清零: %#v", got)
	}
}

func TestRemoveAccount(t *testing.T) {
	st := newStore(t)
	a := addAccount(t, st, "sk-1", "")
	if !st.RemoveAccount(a.ID) {
		t.Fatal("删除应成功")
	}
	if st.RemoveAccount(a.ID) {
		t.Fatal("重复删除应返回 false")
	}
	if st.Account(a.ID) != nil {
		t.Fatal("账号仍存在")
	}
}

func TestKeysLifecycle(t *testing.T) {
	st := newStore(t)
	if st.HasKeys() {
		t.Fatal("空状态不应有密钥")
	}

	k := st.AddKey("cli", "grp")
	if k.Key == "" || k.Group != "grp" || !k.Enabled {
		t.Fatalf("密钥创建错误: %#v", k)
	}
	if !st.HasKeys() {
		t.Fatal("HasKeys 应为 true")
	}
	if !st.ValidKey(k.Key) {
		t.Fatal("有效密钥应通过校验")
	}
	if got := st.KeyGroup(k.Key); got != "grp" {
		t.Fatalf("KeyGroup = %q", got)
	}
	if st.KeyGroup("sk-unknown") != "" {
		t.Fatal("未知密钥的 KeyGroup 应为空")
	}

	// Rebinding the group: keep the key value unchanged; an empty group falls back to default.
	if !st.SetKeyGroup(k.Key, "default") {
		t.Fatal("SetKeyGroup 应找到已存在的密钥")
	}
	if got := st.KeyGroup(k.Key); got != "default" {
		t.Fatalf("改绑后 KeyGroup = %q", got)
	}
	if !st.SetKeyGroup(k.Key, "  ") || st.KeyGroup(k.Key) != "default" {
		t.Fatalf("空分组应归一为 default，实际 %q", st.KeyGroup(k.Key))
	}
	if st.SetKeyGroup("sk-unknown", "x") {
		t.Fatal("未知密钥不应改绑成功")
	}
	if k.Key == "" || len(st.Keys()) != 1 {
		t.Fatal("改绑不应影响密钥数量或值")
	}

	// Counting: ValidKey increments the usage count.
	k2 := st.Keys()[0]
	if k2.Requests != 1 || k2.LastUsed == 0 {
		t.Fatalf("用量未累计: %#v", k2)
	}

	st.ToggleKey(k.Key)
	if st.ValidKey(k.Key) {
		t.Fatal("停用后不应通过校验")
	}
	if st.ToggleKey("sk-unknown") {
		t.Fatal("未知密钥 Toggle 应返回 false")
	}

	if !st.DeleteKey(k.Key) {
		t.Fatal("删除密钥应成功")
	}
	if st.DeleteKey(k.Key) {
		t.Fatal("重复删除应返回 false")
	}
	if st.HasKeys() {
		t.Fatal("删除后不应再有密钥")
	}
}

func TestAddKeyDefaultsToDefaultGroup(t *testing.T) {
	st := newStore(t)
	k := st.AddKey("x", "   ")
	if k.Group != "default" {
		t.Fatalf("空分组应归一为 default，实际 %q", k.Group)
	}
}

func TestRecordTrimsLogs(t *testing.T) {
	st := newStore(t)
	st.UpdateSettings(func(s *Settings) { s.LogKeep = 3 })

	for i := 0; i < 10; i++ {
		st.Record(LogEntry{Model: "m", Endpoint: "chat", Status: 200}, 10)
	}
	logs := st.Logs(0)
	if len(logs) != 3 {
		t.Fatalf("日志应被裁剪到 3 条，实际 %d", len(logs))
	}
	// The newest one should be kept.
	if st.Stats().TotalRequests != 10 {
		t.Fatalf("统计不应被裁剪，实际 %d", st.Stats().TotalRequests)
	}
}

func TestRecordCountsFailures(t *testing.T) {
	st := newStore(t)
	st.Record(LogEntry{Status: 200}, 5)
	st.Record(LogEntry{Status: 502}, 0)
	s := st.Stats()
	if s.TotalRequests != 2 || s.FailedRequests != 1 {
		t.Fatalf("统计错误: %#v", s)
	}
}

func TestModelCatalog(t *testing.T) {
	st := newStore(t)
	// When the catalog has not been fetched, DefaultModel is empty; the fallback is handled by server.resolveModel
	// (empty catalog → deepai.DefaultModelEntry).
	if got := st.DefaultModel(); got != "" {
		t.Fatalf("未设置目录时 DefaultModel 应为空，实际 %q", got)
	}
	st.SetModels([]deepai.Model{{ID: "a"}, {ID: "b"}}, "b")
	if st.DefaultModel() != "b" {
		t.Fatalf("DefaultModel = %q", st.DefaultModel())
	}
	if st.ModelsUpdated() == 0 {
		t.Fatal("ModelsUpdated 应被填充")
	}
	if len(st.Models()) != 2 {
		t.Fatalf("模型数 = %d", len(st.Models()))
	}

	// When def is empty, fall back to the first catalog entry; an existing default is not overwritten by an empty value.
	st2 := newStore(t)
	st2.SetModels([]deepai.Model{{ID: "first"}}, "")
	if st2.DefaultModel() != "first" {
		t.Fatalf("空 def 应回落到首个模型，实际 %q", st2.DefaultModel())
	}
	st2.SetModels([]deepai.Model{{ID: "second"}}, "")
	if st2.DefaultModel() != "first" {
		t.Fatalf("已有默认值不应被空覆盖，实际 %q", st2.DefaultModel())
	}
}

func TestSettingsSnapshotIsCopy(t *testing.T) {
	st := newStore(t)
	st.UpdateSettings(func(s *Settings) {
		s.ModelAliases = map[string]string{"a": "b"}
		s.AdminPassword = "pw"
	})

	snap := st.Settings()
	snap.ModelAliases["a"] = "mutated"

	// map is a reference type and must be copied; scalar fields are guaranteed by Go's value semantics.
	if fresh := st.Settings(); fresh.ModelAliases["a"] != "b" {
		t.Fatal("Settings 返回的别名表应与内部状态隔离")
	}
	if fresh := st.Settings(); fresh.AdminPassword != "pw" {
		t.Fatal("Settings 快照不应影响内部状态")
	}
}

func TestConcurrentAccessIsRaceFree(t *testing.T) {
	// Run with -race: account pool scheduling, key validation, log writes, and concurrent save.
	st := newStore(t)
	for _, k := range []string{"sk-a", "sk-b"} {
		addAccount(t, st, k, "")
	}
	key := st.AddKey("k", "")

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if a := st.NextAccountGroup(""); a != nil {
					st.MarkAccountResult(a.ID, j%2 == 0, "")
				}
				_ = st.ValidKey(key.Key)
				st.Record(LogEntry{Status: 200}, 1)
				_ = st.Accounts()
				_ = st.Settings()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if err := st.Save(); err != nil {
		t.Fatalf("并发后 Save: %v", err)
	}
}

func TestNextAccountPrefersAccountsWithQuota(t *testing.T) {
	// The upstream issues a daily quota per machine, so the pool often contains accounts with balance=0;
	// Accounts with quota should be selected first, so we don't get rejected by the upstream once every time.
	st := newStore(t)
	empty := addAccount(t, st, "sk-empty", "")
	full := addAccount(t, st, "sk-full", "")
	now := time.Now().Unix()
	st.SetAccountAllowance(empty.ID, &deepai.Allowance{Enabled: true, Balance: "0.00000", CheckedAt: now}, nil)
	st.SetAccountAllowance(full.ID, &deepai.Allowance{Enabled: true, Balance: "2.50000", CheckedAt: now}, nil)

	for i := 0; i < 10; i++ {
		got := st.NextAccountGroup("")
		if got == nil || got.ID != full.ID {
			t.Fatalf("应优先选择有额度的账号，实际 %#v", got)
		}
	}
}

func TestNextAccountFallsBackToZeroQuota(t *testing.T) {
	// When the whole group has 0 quota, do not return nil, otherwise the request fails outright.
	st := newStore(t)
	a := addAccount(t, st, "sk-a", "")
	b := addAccount(t, st, "sk-b", "")
	now := time.Now().Unix()
	st.SetAccountAllowance(a.ID, &deepai.Allowance{Enabled: true, Balance: "0", CheckedAt: now}, nil)
	st.SetAccountAllowance(b.ID, &deepai.Allowance{Enabled: true, Balance: "0.0", CheckedAt: now}, nil)

	if got := st.NextAccountGroup(""); got == nil {
		t.Fatal("全组 0 额度时应回退，而不是返回 nil")
	}
}

func TestNextAccountUnknownQuotaIsUsable(t *testing.T) {
	// Not probed / unparseable / free quota disabled must not be treated as "no quota".
	cases := []*deepai.Allowance{
		nil,
		{Enabled: true, Balance: "abc"},
		{Enabled: true, Balance: ""},
		{Enabled: false, Balance: "0"},
	}
	for _, al := range cases {
		st := newStore(t)
		a := addAccount(t, st, "sk-1", "")
		st.SetAccountAllowance(a.ID, al, nil)
		if got := st.NextAccountGroup(""); got == nil || got.ID != a.ID {
			t.Fatalf("额度状态 %#v 不应导致账号被跳过", al)
		}
	}
}

func TestUpsertPreservesMachineID(t *testing.T) {
	st := newStore(t)
	a := addAccount(t, st, "sk-1", "")
	// The account pool no longer hands out live pointers externally; field changes must go through UpdateAccount.
	st.UpdateAccount(a.ID, func(x *deepai.Account) { x.MachineID = "machine-keep" })

	// Re-importing (e.g. the desktop client refreshed the Key) should not clear the machine fingerprint,
	// otherwise the account's quota attribution on the upstream will break.
	st.UpsertAccount(&deepai.Account{APIKey: "sk-1", Name: "n"})
	if got := st.Account(a.ID); got.MachineID != "machine-keep" {
		t.Fatalf("机器指纹被清空: %q", got.MachineID)
	}

	// A non-empty new value should be allowed to overwrite.
	st.UpsertAccount(&deepai.Account{APIKey: "sk-1", MachineID: "machine-new"})
	if got := st.Account(a.ID); got.MachineID != "machine-new" {
		t.Fatalf("新指纹未生效: %q", got.MachineID)
	}
}

func TestSaveSkipsUnchangedWrites(t *testing.T) {
	// Background Save every 30s; when the content is unchanged, the whole state file should not be rewritten repeatedly.
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	addAccount(t, st, "sk-1", "")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Content unchanged → skip (mtime unchanged).
	time.Sleep(10 * time.Millisecond)
	if err := st.Save(); err != nil {
		t.Fatalf("二次 Save: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Fatal("内容未变化时不应重写文件")
	}

	// Content changed → must persist to disk.
	st.AddKey("new", "")
	if err := st.Save(); err != nil {
		t.Fatalf("变更后 Save: %v", err)
	}
	third, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if third.ModTime().Equal(second.ModTime()) {
		t.Fatal("内容变化后必须重写文件")
	}
	back, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	if !back.HasKeys() {
		t.Fatal("变更未持久化")
	}
}

func TestZeroQuotaGoesStale(t *testing.T) {
	// A 0 quota at the moment of registration must not permanently condemn the account; a stale 0 must be treated as "unknown" again.
	now := time.Now().Unix()

	fresh := &deepai.Account{ID: "a", Enabled: true,
		Theos: &deepai.Allowance{Enabled: true, Balance: "0.00000", CheckedAt: now}}
	if pool.HasQuota(fresh) {
		t.Fatal("刚探到的 0 额度不应算可用")
	}

	stale := &deepai.Account{ID: "b", Enabled: true,
		Theos: &deepai.Allowance{Enabled: true, Balance: "0.00000",
			CheckedAt: now - int64(pool.QuotaStaleAfter/time.Second) - 60}}
	if !pool.HasQuota(stale) {
		t.Fatal("超过时限的 0 额度应视为未知（允许重探）")
	}

	// A 0 with no probe timestamp: treat as unknown.
	noStamp := &deepai.Account{ID: "c", Enabled: true,
		Theos: &deepai.Allowance{Enabled: true, Balance: "0"}}
	if !pool.HasQuota(noStamp) {
		t.Fatal("无 CheckedAt 的 0 额度应按未知处理")
	}
}

func TestStaleZeroQuotaIsSchedulable(t *testing.T) {
	// End-to-end: a stale 0-quota account must be schedulable.
	st := newStore(t)
	a := addAccount(t, st, "sk-stale", "")
	st.SetAccountAllowance(a.ID, &deepai.Allowance{
		Enabled: true, Balance: "0.00000",
		CheckedAt: time.Now().Add(-time.Hour).Unix(),
	}, nil)
	if got := st.NextAccountGroup(""); got == nil || got.ID != a.ID {
		t.Fatalf("陈旧的 0 额度账号应可被调度，实际 %#v", got)
	}
}

// TestRecordClientGoneIsNotFailure guards the counting semantics of 499.
//
// Client disconnect (closing the tab / SDK cancellation) used to be recorded as 502 and counted in failed_requests:
// In production, 3 of 4 measured 502s were this case, which inflated the failure rate and made it impossible to tell whether the upstream was actually stable.
// Now 499 only goes to the logs and is not counted as a failure (nor as a success - it is neither).
func TestRecordClientGoneIsNotFailure(t *testing.T) {
	st := newStore(t)
	st.Record(LogEntry{Status: 200}, 1)
	st.Record(LogEntry{Status: StatusClientGone, Err: "context canceled"}, 0)
	s := st.Stats()
	if s.TotalRequests != 2 {
		t.Fatalf("TotalRequests = %d，期望 2", s.TotalRequests)
	}
	if s.FailedRequests != 0 {
		t.Fatalf("FailedRequests = %d，期望 0（499 不计失败）", s.FailedRequests)
	}
	// The log entry must still be visible, otherwise "the client left" becomes a silent black hole.
	if logs := st.QueryLogs(QueryFilter{Limit: 10}); len(logs) != 2 {
		t.Fatalf("日志条数 = %d，期望 2", len(logs))
	}
}

// TestStatsDayCachedTokens guards the numerator of the "daily cache hit rate".
//
// The console's hit rate used to be total_cached_tokens / total_tokens_in (cumulative / cumulative),
// When displayed alongside the "today's token input" metric, it looks like a today metric, but it is always lagging. To have a same-day basis,
// cached must also be accumulated per day and reset to zero when crossing days.
func TestStatsDayCachedTokens(t *testing.T) {
	st := newStore(t)
	st.Record(LogEntry{Status: 200, TokensIn: 100, CachedTokens: 80}, 0)
	st.Record(LogEntry{Status: 200, TokensIn: 100, CachedTokens: 0}, 0)
	s := st.Stats()
	if s.DayTokensIn != 200 || s.DayCachedTokens != 80 {
		t.Fatalf("当日 token 统计错误: in=%d cached=%d", s.DayTokensIn, s.DayCachedTokens)
	}
	if s.TotalCachedTokens != 80 || s.TotalTokensIn != 200 {
		t.Fatalf("累计 token 统计错误: in=%d cached=%d", s.TotalTokensIn, s.TotalCachedTokens)
	}

	// Simulate crossing days: change the date key to yesterday; the next Record should reset the day's count (including cached) to zero.
	st.mu.Lock()
	st.st.Stats.Day = "2000-01-01"
	st.st.Stats.DayTokensIn = 999
	st.st.Stats.DayCachedTokens = 999
	st.mu.Unlock()
	st.Record(LogEntry{Status: 200, TokensIn: 10, CachedTokens: 7}, 0)
	s = st.Stats()
	if s.DayTokensIn != 10 || s.DayCachedTokens != 7 {
		t.Fatalf("跨天未清零: in=%d cached=%d", s.DayTokensIn, s.DayCachedTokens)
	}
	if s.TotalCachedTokens != 87 {
		t.Fatalf("累计 cached 不应被跨天清零: %d", s.TotalCachedTokens)
	}
}
