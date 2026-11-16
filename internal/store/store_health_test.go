package store

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// This group locks down "how failures are accounted for" and "how error marks self-heal" (docs/changes.md §45).
//
// Old implementation: any failure wrote status=error, and only a successful request / success /theos would clear it;
// So a probabilistic upstream EOF / 429 would make the account show as unhealthy on the console for a long time, while the scheduler actually
// only skips blocked - display and reality are out of sync.

func transientErr() error {
	return &deepai.HTTPError{Op: "上游", Status: http.StatusTooManyRequests, Body: []byte(`{}`)}
}

func hardErr() error {
	return &deepai.HTTPError{Op: "上游", Status: http.StatusUnauthorized,
		Body: []byte(`{"error":{"code":"invalid_api_key"}}`)}
}

func TestMarkAccountFailureTransientKeepsStatus(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true, Status: "ok"})

	st.MarkAccountFailure("a1", transientErr())

	a := st.Accounts()[0]
	if a.Status != "ok" {
		t.Fatalf("瞬时失败不应改状态，实际 %q", a.Status)
	}
	if a.Failures != 1 || a.LastErr == "" || a.LastErrAt == 0 {
		t.Fatalf("失败历史仍应记录：failures=%d lastErr=%q lastErrAt=%d", a.Failures, a.LastErr, a.LastErrAt)
	}
}

func TestMarkAccountFailureHardMarksError(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true, Status: "ok"})

	st.MarkAccountFailure("a1", hardErr())

	a := st.Accounts()[0]
	if a.Status != deepai.StatusError {
		t.Fatalf("账号级失败应标 error，实际 %q", a.Status)
	}
	if a.LastErrAt == 0 {
		t.Fatal("标记 error 时必须写 last_err_at，否则无法衰减")
	}
}

func TestMarkAccountSuccessClearsError(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true,
		Status: deepai.StatusError, LastErr: "旧错误", LastErrAt: time.Now().Unix()})

	st.MarkAccountResult("a1", true, "")

	a := st.Accounts()[0]
	if a.Status != "ok" || a.LastErr != "" || a.LastErrAt != 0 || a.Failures != 0 {
		t.Fatalf("成功应彻底清掉错误痕迹：status=%q lastErr=%q lastErrAt=%d failures=%d",
			a.Status, a.LastErr, a.LastErrAt, a.Failures)
	}
}

func TestRecoverStaleErrors(t *testing.T) {
	st := newStore(t)
	stale := time.Now().Add(-deepai.ErrorStaleAfter - time.Minute).Unix()
	fresh := time.Now().Unix()
	st.UpsertAccount(&deepai.Account{ID: "stale", APIKey: "k-stale", Enabled: true, Status: deepai.StatusError, LastErrAt: stale})
	st.UpsertAccount(&deepai.Account{ID: "fresh", APIKey: "k-fresh", Enabled: true, Status: deepai.StatusError, LastErrAt: fresh})
	st.UpsertAccount(&deepai.Account{ID: "blocked", APIKey: "k-blocked", Enabled: true, Status: deepai.StatusBlocked, LastErrAt: stale})
	// Old state files have no last_err_at: they must also be able to self-heal.
	st.UpsertAccount(&deepai.Account{ID: "legacy", APIKey: "k-legacy", Enabled: true, Status: deepai.StatusError})

	if n := st.RecoverStaleErrors(time.Now()); n != 2 {
		t.Fatalf("应自愈 2 个（stale + legacy），实际 %d", n)
	}
	byID := map[string]*deepai.Account{}
	for _, a := range st.Accounts() {
		byID[a.ID] = a
	}
	if byID["stale"].Status != "ok" || byID["legacy"].Status != "ok" {
		t.Fatalf("陈旧错误应置回 ok：stale=%q legacy=%q", byID["stale"].Status, byID["legacy"].Status)
	}
	if byID["fresh"].Status != deepai.StatusError {
		t.Fatalf("新鲜错误不该被动：%q", byID["fresh"].Status)
	}
	if byID["blocked"].Status != deepai.StatusBlocked {
		t.Fatalf("blocked 不会自愈，不该被改：%q", byID["blocked"].Status)
	}
	// Idempotent: running again has nothing to change.
	if n := st.RecoverStaleErrors(time.Now()); n != 0 {
		t.Fatalf("第二次应为 0，实际 %d", n)
	}
}

func TestAccountCountsUsesStalenessAndBlocked(t *testing.T) {
	st := newStore(t)
	stale := time.Now().Add(-deepai.ErrorStaleAfter - time.Minute).Unix()
	fresh := time.Now().Unix()
	st.UpsertAccount(&deepai.Account{ID: "ok", APIKey: "k-ok", Enabled: true, Status: "ok"})
	st.UpsertAccount(&deepai.Account{ID: "unknown", APIKey: "k-unknown", Enabled: true, Status: "unknown"})
	st.UpsertAccount(&deepai.Account{ID: "staleErr", APIKey: "k-staleerr", Enabled: true, Status: deepai.StatusError, LastErrAt: stale})
	st.UpsertAccount(&deepai.Account{ID: "freshErr", APIKey: "k-fresherr", Enabled: true, Status: deepai.StatusError, LastErrAt: fresh})
	st.UpsertAccount(&deepai.Account{ID: "blocked", APIKey: "k-blocked", Enabled: true, Status: deepai.StatusBlocked})
	st.UpsertAccount(&deepai.Account{ID: "off", APIKey: "k-off", Enabled: false, Status: "ok"})

	total, ready := st.AccountCounts()
	if total != 6 {
		t.Fatalf("总数 = %d，期望 6", total)
	}
	// ok + unknown + staleErr = 3; freshErr / blocked / off do not count.
	if ready != 3 {
		t.Fatalf("就绪数 = %d，期望 3（ok/unknown/陈旧 error）", ready)
	}
}

func TestSetAccountAllowanceTransientKeepsStatus(t *testing.T) {
	st := newStore(t)
	st.UpsertAccount(&deepai.Account{ID: "a1", APIKey: "k1", Enabled: true, Status: "ok"})

	// Probe failure (EOF): should not mark a good account as error, but must back off.
	st.SetAccountAllowance("a1", nil, errors.New("Get \"https://api.deepai.org\": unexpected EOF"))

	a := st.Accounts()[0]
	if a.Status != "ok" {
		t.Fatalf("瞬时探测失败不应改状态，实际 %q", a.Status)
	}
	if a.ProbeFailures != 1 || a.ProbeAfter == 0 {
		t.Fatalf("仍应累加探测退避：probeFailures=%d probeAfter=%d", a.ProbeFailures, a.ProbeAfter)
	}

	// Account-level failures must be marked error.
	st.SetAccountAllowance("a1", nil, hardErr())
	if got := st.Accounts()[0].Status; got != deepai.StatusError {
		t.Fatalf("账号级探测失败应标 error，实际 %q", got)
	}
}
