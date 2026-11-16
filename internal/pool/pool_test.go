package pool

import (
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

func acct(id, group string) *deepai.Account {
	return &deepai.Account{ID: id, Name: id, APIKey: "sk-" + id, Group: group, Enabled: true}
}

// Three healthy accounts should be split evenly.
func TestPickRoundRobins(t *testing.T) {
	s := New()
	accounts := []*deepai.Account{acct("a", ""), acct("b", ""), acct("c", "")}

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		got := s.Pick(accounts, "")
		if got == nil {
			t.Fatal("应有可用账号")
		}
		seen[got.ID]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] != 10 {
			t.Fatalf("账号 %s 被选中 %d 次，期望 10；分布=%v", id, seen[id], seen)
		}
	}
}

func TestPickSkipsDisabledAndFailed(t *testing.T) {
	s := New()
	good, bad, off := acct("good", ""), acct("bad", ""), acct("off", "")
	bad.Failures = 3
	bad.Status = "error"
	off.Enabled = false
	accounts := []*deepai.Account{good, bad, off}

	for i := 0; i < 10; i++ {
		got := s.Pick(accounts, "")
		if got == nil || got.ID != "good" {
			t.Fatalf("应只选到健康账号，实际 %v", got)
		}
	}
}

// When the whole group has failed, do not return nil: better to send the request out than to block hard.
func TestPickFallsBackWhenAllFailed(t *testing.T) {
	s := New()
	a, b := acct("a", ""), acct("b", "")
	a.Failures = 5
	b.Failures = 2
	if got := s.Pick([]*deepai.Account{a, b}, ""); got == nil {
		t.Fatal("全部失败时应回退到失败最少的账号")
	}
}

func TestPickRespectsGroup(t *testing.T) {
	s := New()
	accounts := []*deepai.Account{acct("g1", "g1"), acct("g2", "g2")}

	for i := 0; i < 5; i++ {
		if got := s.Pick(accounts, "g1"); got == nil || got.GroupName() != "g1" {
			t.Fatalf("分组隔离失效: %v", got)
		}
	}
	if got := s.Pick(accounts, "nope"); got != nil {
		t.Fatalf("不存在的分组应返回 nil，实际 %v", got)
	}
	// Empty group = all groups.
	if got := s.Pick(accounts, ""); got == nil {
		t.Fatal("空分组应覆盖全部账号")
	}
}

// Prefer accounts with quota: avoid being rejected by the upstream once before switching every time.
func TestPickPrefersQuota(t *testing.T) {
	s := New()
	now := time.Now().Unix()
	empty, full := acct("empty", ""), acct("full", "")
	empty.Theos = &deepai.Allowance{Enabled: true, Balance: "0.00000", CheckedAt: now}
	full.Theos = &deepai.Allowance{Enabled: true, Balance: "2.50000", CheckedAt: now}

	for i := 0; i < 10; i++ {
		if got := s.Pick([]*deepai.Account{empty, full}, ""); got == nil || got.ID != "full" {
			t.Fatalf("应优先选择有额度的账号，实际 %v", got)
		}
	}
}

// Zero quota counts as exhausted only when it was probed recently.
func TestHasQuota(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name string
		a    *deepai.Account
		want bool
	}{
		{"nil", nil, true},
		{"无 Theos", &deepai.Account{}, true},
		{"未启用配额", &deepai.Account{Theos: &deepai.Allowance{Enabled: false, Balance: "0"}}, true},
		{"余额为空", &deepai.Account{Theos: &deepai.Allowance{Enabled: true}}, true},
		{"无法解析", &deepai.Account{Theos: &deepai.Allowance{Enabled: true, Balance: "n/a"}}, true},
		{"有余额", &deepai.Account{Theos: &deepai.Allowance{Enabled: true, Balance: "1.5"}}, true},
		{"刚探到 0", &deepai.Account{Theos: &deepai.Allowance{Enabled: true, Balance: "0", CheckedAt: now}}, false},
		{"陈旧的 0", &deepai.Account{Theos: &deepai.Allowance{
			Enabled: true, Balance: "0",
			CheckedAt: now - int64(QuotaStaleAfter/time.Second) - 60,
		}}, true},
		{"0 但无时间戳", &deepai.Account{Theos: &deepai.Allowance{Enabled: true, Balance: "0"}}, true},
	}
	for _, c := range cases {
		if got := HasQuota(c.a); got != c.want {
			t.Fatalf("%s: HasQuota = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestPickEmpty(t *testing.T) {
	if got := New().Pick(nil, ""); got != nil {
		t.Fatalf("空号池应返回 nil，实际 %v", got)
	}
}

// Accounts blocked by the upstream policy (social_login_required) must not be scheduled again.
func TestPickSkipsBlocked(t *testing.T) {
	s := New()
	blocked, good := acct("blocked", ""), acct("good", "")
	blocked.Status = deepai.StatusBlocked
	accounts := []*deepai.Account{blocked, good}

	for i := 0; i < 10; i++ {
		if got := s.Pick(accounts, ""); got == nil || got.ID != "good" {
			t.Fatalf("应跳过被拦截账号，实际 %v", got)
		}
	}
	if got := s.Pick([]*deepai.Account{blocked}, ""); got != nil {
		t.Fatalf("只剩被拦截账号时应返回 nil，实际 %v", got)
	}
}

// TestPickSkipsQuotaExhausted fixes the scheduling skip for "daily quota used up":
// not selectable before QuotaUntil (otherwise every request wastefully hits 403),
// and automatically restored when the time arrives.
func TestPickSkipsQuotaExhausted(t *testing.T) {
	now := time.Now().Unix()
	accts := []*deepai.Account{
		{ID: "a", Enabled: true, Status: "ok", QuotaUntil: now + 3600},
		{ID: "b", Enabled: true, Status: "ok"},
	}
	s := New()
	for i := 0; i < 3; i++ {
		got := s.Pick(accts, "")
		if got == nil || got.ID != "b" {
			t.Fatalf("应只选中未耗尽额度的账号，实际 %+v", got)
		}
	}

	// Selectable again after expiry.
	accts[0].QuotaUntil = now - 1
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		if got := s.Pick(accts, ""); got != nil {
			seen[got.ID] = true
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("额度恢复后两个账号都应参与轮询，实际 %v", seen)
	}
}
