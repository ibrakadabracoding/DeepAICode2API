package server

import (
	"context"
	"io"
	"net/http"
	"testing"

	"deepaicode2api/internal/deepai"
)

// modelsForWarmup is the minimal valid response for /models: FetchModels requires a non-empty catalog.
const modelsForWarmup = `{"models":[{"id":"m1","name":"M1","reasoning":false,` +
	`"input":["text"],"contextWindow":1000,"maxTokens":100,"theosPerRequest":"0"}],"default":"m1"}`

// newWarmupUpstream starts a fake upstream that only recognizes /models.
func newWarmupUpstream(t *testing.T) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deepai.ModelBasePath+deepai.PathModels {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, modelsForWarmup)
	})
}

func countPath(u *upstream, path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, r := range u.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

// TestWarmUpstreamPrimesDistinctEgresses locks in "warm up each egress only once":
// The 14 accounts in the account pool often share only a few egresses; warming up per account would hammer the proxy and slow startup.
func TestWarmUpstreamPrimesDistinctEgresses(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	// Add two "direct" accounts plus one disabled account: the egress set is still {""}.
	for _, id := range []string{"acc-2", "acc-3"} {
		s.Store.UpsertAccount(&deepai.Account{ID: id, Name: id, APIKey: "k-" + id, Enabled: true, Status: "ok"})
	}
	s.Store.UpsertAccount(&deepai.Account{ID: "acc-off", Name: "off", APIKey: "k-off", Enabled: false})

	if got := s.WarmUpstream(context.Background()); got != 1 {
		t.Fatalf("预热出口数 = %d，期望 1（直连只有一个）", got)
	}
	if got := countPath(u, deepai.ModelBasePath+deepai.PathModels); got != 1 {
		t.Fatalf("/models 请求数 = %d，期望 1", got)
	}
}

// TestWarmUpstreamEmptyPool: an empty account pool should send no requests and should not error.
func TestWarmUpstreamEmptyPool(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)
	// Accounts() returns a copy; you must go through the store's mutation entry point to actually disable it.
	s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		a.Enabled = false
		return true
	})

	if got := s.WarmUpstream(context.Background()); got != 0 {
		t.Fatalf("空号池预热数 = %d，期望 0", got)
	}
	if got := countPath(u, deepai.ModelBasePath+deepai.PathModels); got != 0 {
		t.Fatalf("空号池不应发请求，实际 %d 次", got)
	}
}

// TestWarmUpstreamFailureIsSilent: when the upstream is down, warmup just returns 0 - no panic, no blocking.
func TestWarmUpstreamFailureIsSilent(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	s, _ := newTestServer(t, u)

	if got := s.WarmUpstream(context.Background()); got != 0 {
		t.Fatalf("上游 500 时预热数 = %d，期望 0", got)
	}
}

// A 401 also counts as a successful warmup: since 2026-11-14 the upstream requires Api-Key for /models, and warmup only cares whether
// DNS/TCP/TLS and the proxy CONNECT are reachable - a 401 already proves the link is up.
func TestWarmUpstreamCountsUnauthorizedAsReachable(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"auth_required","type":"auth_required"}}`)
	})
	s, _ := newTestServer(t, u)

	if got := s.WarmUpstream(context.Background()); got != 1 {
		t.Fatalf("401 应算预热成功（链路已通），实际 %d", got)
	}
}

// Warmup must carry account credentials: without an Api-Key, /models only returns 401; with one, the catalog request actually goes
// through the full link (which also incidentally verifies that "pick a representative account per egress" really gets a real account).
func TestWarmUpstreamSendsAccountKey(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	if got := s.WarmUpstream(context.Background()); got != 1 {
		t.Fatalf("预热数 = %d，期望 1", got)
	}
	if key := u.last(t).Header.Get(deepai.HeaderKeyName); key != "upstream-key" {
		t.Fatalf("预热请求的 Api-Key = %q，期望 upstream-key", key)
	}
}
