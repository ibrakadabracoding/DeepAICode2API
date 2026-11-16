package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/parallel"
)

const (
	// WarmUpstreamWorkers is the concurrency limit for connection warm-up: when there are few egresses there is no need to open too many,
	// and when there are many egresses it should not saturate the proxies at startup.
	WarmUpstreamWorkers = 4
	// WarmUpstreamTimeout is the warm-up budget for a single egress (including proxy handshake).
	WarmUpstreamTimeout = 10 * time.Second
)

// WarmUpstream hits `/models` once per egress that actually appears in the account pool, completing
// DNS / TCP / TLS (and proxy CONNECT) ahead of time.
//
// Why it is worth doing: the first real inference request would otherwise pay the entire handshake cost serially. In r2 load tests, cold start
// p90 is 4129ms, while a warm connection is about 2000ms; `/models` does not burn Theos, making it the only endpoint that is "free but
// goes through the full path".
//
// Since 2026-11-14, /models requires an Api-Key, so for each egress we take **a real account on that egress**
// to warm up (rather than an empty account with only a Proxy); a 401 still counts as a successful warm-up - it already
// proves that DNS/TCP/TLS/proxy CONNECT all succeeded, only that the account on this egress has no Key.
//
// Semantics: only warm up the egresses used by enabled accounts (including the "direct" tier), and hit each egress only once;
// failures are silent (warm-up is a pure optimization and must not affect startup); returns the number of successful egresses.
func (s *Server) WarmUpstream(ctx context.Context) int {
	byEgress := map[string]*deepai.Account{}
	for _, a := range s.Store.Accounts() {
		if a == nil || !a.Enabled {
			continue
		}
		// The empty string represents direct connection (via s.Up), and here it participates in warm-up as an independent egress.
		if _, seen := byEgress[a.Proxy]; !seen {
			byEgress[a.Proxy] = a
		}
	}

	targets := make([]string, 0, len(byEgress))
	for proxy := range byEgress {
		targets = append(targets, proxy)
	}
	// Fixed order: only then do logs and tests line up, and it also avoids warm-up order jitter on each startup.
	sort.Strings(targets)

	var ok atomic.Int64
	parallel.For(ctx, WarmUpstreamWorkers, len(targets), func(ctx context.Context, i int) {
		attempt, cancel := context.WithTimeout(ctx, WarmUpstreamTimeout)
		defer cancel()
		acct := byEgress[targets[i]]
		client := s.clientFor(acct)
		if _, _, err := client.FetchModels(attempt, acct); err != nil && !warmReachable(err) {
			return
		}
		ok.Add(1)
	})
	return int(ok.Load())
}

// warmReachable determines whether a warm-up failure actually means "the path is already up".
//
// 401 = the upstream requires a Key but the account on this egress did not provide one (or the Key is invalid): DNS/TCP/TLS and proxy
// CONNECT have all succeeded, so the purpose of warm-up is equally achieved. Only 5xx / timeouts mean it is truly unreachable.
func warmReachable(err error) bool {
	var he *deepai.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusUnauthorized
}
