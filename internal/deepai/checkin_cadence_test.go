package deepai

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nonceServer issues a **new** nonce on every /theos call (matching real
// upstream behavior).
//
// This detail is the root cause of the old implementation's bug: deciding
// whether to supplement a check-in via "latest nonce != acked nonce" becomes
// true again after every relay, bypassing the seconds/2 cadence into
// checkInMinInterval.
func nonceServer(t *testing.T, hits *int) *Client {
	t.Helper()
	var mu sync.Mutex
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*hits++
		mu.Unlock()
		if r.Header.Get(HeaderCheckIn) == "" {
			_, _ = io.WriteString(w, `{"enabled":true,"check_in":{"nonce":"`+repeatHex("a")+`","seconds":600}}`)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":true,"check_in":{"nonce":"`+repeatHex("b")+`","seconds":600}}`)
	})
	return c
}

func repeatHex(ch string) string {
	out := ""
	for i := 0; i < 32; i++ {
		out += ch
	}
	return out
}

// TestEnsureCheckInFollowsHalfLife pins down the desktop cadence:
// within seconds/2 after a completed relay (600s → 300s) the upstream is not
// disturbed; only past the half-life is another check-in performed. The old
// implementation hit it every 30s.
func TestEnsureCheckInFollowsHalfLife(t *testing.T) {
	hits := 0
	c := nonceServer(t, &hits)
	acct := &Account{APIKey: "k-cadence"}
	ctx := context.Background()

	// Cold start: challenge received but not yet relayed.
	if _, err := c.Theos(ctx, acct); err != nil {
		t.Fatalf("Theos: %v", err)
	}
	if hits != 1 {
		t.Fatalf("首次 /theos 应调用 1 次，实际 %d", hits)
	}

	// First supplementary check-in: relay immediately (must not wait for the
	// half-life, otherwise a cold start would go unsigned for 5 minutes).
	if out := c.EnsureCheckIn(ctx, acct); !out.Attempted {
		t.Fatal("冷启动后的第一次推理应补签")
	}
	if hits != 2 {
		t.Fatalf("补签应调用 /theos，实际 %d 次", hits)
	}

	// Within the half-life: move attemptAt back by 1 minute (to avoid the 30s
	// fallback); no supplementary check-in should happen.
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	st.attemptAt = time.Now().Add(-time.Minute)
	c.checkIns.mu.Unlock()
	if out := c.EnsureCheckIn(ctx, acct); out.Attempted {
		t.Fatal("半衰期内不应补签")
	}
	if hits != 2 {
		t.Fatalf("半衰期内不应请求上游，实际 %d 次", hits)
	}

	// Past the half-life (600s/2=300s): one supplementary check-in is expected.
	c.checkIns.mu.Lock()
	st.ackedAt = time.Now().Add(-301 * time.Second)
	st.attemptAt = time.Now().Add(-time.Minute)
	c.checkIns.mu.Unlock()
	if out := c.EnsureCheckIn(ctx, acct); !out.Attempted {
		t.Fatal("超过半衰期应补签")
	}
	if hits != 3 {
		t.Fatalf("超过半衰期应请求上游，实际 %d 次", hits)
	}
}

// TestEnsureCheckInBudget pins down "the check-in budget is adjustable":
// once the upstream times out, inference is released immediately, the result
// is marked TimedOut, and later requests still continue the catch-up.
func TestEnsureCheckInBudget(t *testing.T) {
	var hits atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"enabled":true}`)
	})
	c.SetCheckInBudget(40 * time.Millisecond)

	start := time.Now()
	out := c.EnsureCheckIn(context.Background(), &Account{APIKey: "k-budget"})
	elapsed := time.Since(start)

	if !out.Attempted {
		t.Fatal("应尝试签到")
	}
	if !out.TimedOut {
		t.Fatalf("应报告 TimedOut，实际 err=%v", out.Err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("预算 40ms 应尽快返回，实际等了 %v", elapsed)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1", got)
	}
}

// TestSetCheckInBudgetNegative covers out-of-range values: negatives are
// clamped to zero (= use the default 10s).
func TestSetCheckInBudgetNegative(t *testing.T) {
	c := NewClient()
	c.SetCheckInBudget(-time.Second)
	if c.CheckInBudget != 0 {
		t.Fatalf("负数预算应归零，实际 %v", c.CheckInBudget)
	}
	if got := c.checkInBudget(); got != checkInAttemptTimeout {
		t.Fatalf("默认预算 = %v，期望 %v", got, checkInAttemptTimeout)
	}
}
