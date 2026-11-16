package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// LockAccountProbe only sets the probe cooldown and a note: it does not change
// status or increment the failure count.
//
// With errors like check_in_refused the account can still run inference (it just
// falls into the substitute-model branch); marking it as error would not affect
// scheduling (the scheduler only skips blocked) but would pollute the health
// display. What really needs preventing is the background guardian hitting 403
// again every 5 minutes - i.e. a re-probe storm (docs/performance.md §14).
func TestLockAccountProbeKeepsStatus(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true, Status: "ok"})
	st.SetAccountAllowance("a1", &deepai.Allowance{Enabled: true}, nil)
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	until := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	st.LockAccountProbe("a1", until, "check_in_refused: too many restarts today")

	a := st.Accounts()[0]
	if a.Status != "ok" {
		t.Fatalf("状态不应变化，实际 %q", a.Status)
	}
	if a.Failures != 0 {
		t.Fatalf("失败计数不应增加，实际 %d", a.Failures)
	}
	if a.ProbeAfter != until.Unix() {
		t.Fatalf("探测冷却 = %d，期望 %d", a.ProbeAfter, until.Unix())
	}
	if !strings.Contains(a.LastErr, "check_in_refused") {
		t.Fatalf("备注未写入: %q", a.LastErr)
	}
	if a.Theos == nil {
		t.Fatal("额度快照不应被清掉")
	}
}

// When a longer cooldown already exists, a shorter lock must not shorten it.
func TestLockAccountProbeKeepsLongerCooldown(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true})
	far := time.Now().Add(12 * time.Hour).Truncate(time.Second)
	st.LockAccountProbe("a1", far, "")
	st.LockAccountProbe("a1", time.Now().Add(time.Minute), "")
	if got := st.Accounts()[0].ProbeAfter; got != far.Unix() {
		t.Fatalf("冷却被缩短: %d，期望 %d", got, far.Unix())
	}
}

// An unknown account must not panic or produce side effects.
func TestLockAccountProbeUnknown(t *testing.T) {
	st := newStore(t)
	st.LockAccountProbe("nope", time.Now().Add(time.Hour), "x")
	if got := st.Accounts(); len(got) != 0 {
		t.Fatalf("不应新建账号: %+v", got)
	}
}

// Probe failures back off per attempt: 2m → 10m → 1h, cleared on success.
func TestProbeCooldownBackoff(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true})

	steps := []time.Duration{2 * time.Minute, 10 * time.Minute, time.Hour, time.Hour}
	for i, want := range steps {
		st.SetAccountAllowance("a1", nil, errors.New("查询 Theos: HTTP 502"))
		a := st.Accounts()[0]
		if a.ProbeFailures != i+1 {
			t.Fatalf("第 %d 次失败后 ProbeFailures = %d", i+1, a.ProbeFailures)
		}
		got := time.Until(time.Unix(a.ProbeAfter, 0))
		if got < want-time.Minute || got > want+time.Minute {
			t.Fatalf("第 %d 次失败后冷却 = %v，期望约 %v", i+1, got, want)
		}
	}

	st.SetAccountAllowance("a1", &deepai.Allowance{Enabled: true}, nil)
	a := st.Accounts()[0]
	if a.ProbeAfter != 0 || a.ProbeFailures != 0 {
		t.Fatalf("成功应清零冷却，实际 after=%d failures=%d", a.ProbeAfter, a.ProbeFailures)
	}
}

// RecordProbeFailure only advances the probe backoff: when an on-demand re-probe
// on the online hot path fails, the account must not be marked bad (Status/Failures
// stay as-is), but ProbeAfter must move forward, otherwise quotaNeedsRefresh is
// always true and every request that selects the account probes again
// (docs/changes.md §61: the same account 323 times/hour).
func TestRecordProbeFailureBacksOffOnlyProbe(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{
		ID: "a1", APIKey: "k1", Enabled: true, Status: "ok",
		Theos: &deepai.Allowance{Enabled: true, CheckedAt: time.Now().Add(-time.Hour).Unix()},
	})

	st.RecordProbeFailure("a1")

	a := st.Accounts()[0]
	if a.ProbeFailures != 1 {
		t.Fatalf("ProbeFailures = %d，期望 1", a.ProbeFailures)
	}
	left := time.Until(time.Unix(a.ProbeAfter, 0))
	if left < 90*time.Second || left > 3*time.Minute {
		t.Fatalf("首次失败冷却 = %v，期望约 2 分钟", left)
	}
	if a.Status != "ok" || a.Failures != 0 {
		t.Fatalf("不应改调度状态: status=%q failures=%d", a.Status, a.Failures)
	}
	if a.Theos == nil {
		t.Fatal("额度快照不应被清掉")
	}

	// The second failure should extend to 10 minutes, and the third caps at 1 hour.
	for i, want := range []time.Duration{10 * time.Minute, time.Hour} {
		st.RecordProbeFailure("a1")
		a = st.Accounts()[0]
		left = time.Until(time.Unix(a.ProbeAfter, 0))
		if left < want-time.Minute || left > want+time.Minute {
			t.Fatalf("第 %d 次失败冷却 = %v，期望约 %v", i+2, left, want)
		}
	}

	// A successful probe (any path that writes the allowance) must clear the backoff.
	st.SetAccountAllowance("a1", &deepai.Allowance{Enabled: true}, nil)
	a = st.Accounts()[0]
	if a.ProbeAfter != 0 || a.ProbeFailures != 0 {
		t.Fatalf("成功应清零退避，实际 after=%d failures=%d", a.ProbeAfter, a.ProbeFailures)
	}
}

// An unknown account must not panic or affect other accounts.
func TestRecordProbeFailureUnknownAccount(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true})
	st.RecordProbeFailure("nope")
	if a := st.Accounts()[0]; a.ProbeFailures != 0 || a.ProbeAfter != 0 {
		t.Fatalf("不应影响其它账号: %+v", a)
	}
}
