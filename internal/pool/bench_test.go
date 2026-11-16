package pool

import (
	"strconv"
	"testing"

	"deepaicode2api/internal/deepai"
)

// benchAccounts builds n healthy accounts (including a balance field, close to a real snapshot).
func benchAccounts(n int) []*deepai.Account {
	out := make([]*deepai.Account, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &deepai.Account{
			ID:       "acc-" + strconv.Itoa(i),
			Name:     "acc-" + strconv.Itoa(i),
			APIKey:   "key-" + strconv.Itoa(i),
			Enabled:  true,
			Status:   "ok",
			Failures: i % 3,
			Theos:    &deepai.Allowance{Balance: "10", Granted: "20", Enabled: true},
		})
	}
	return out
}

// BenchmarkPick500 is the baseline for the account-selection hot path.
//
// Goal: a single pick from a 500-account pool stays at nanosecond scale with zero extra
// allocations. If someone ever reintroduces a "deep-copy the whole pool", the ns/op and allocs/op
// here will immediately show it.
func BenchmarkPick500(b *testing.B) {
	s := New()
	accounts := benchAccounts(500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.Pick(accounts, "") == nil {
			b.Fatal("应选到账号")
		}
	}
}

// BenchmarkPickFiltered500 covers the path with concurrency-slot filtering (the server's actual usage).
func BenchmarkPickFiltered500(b *testing.B) {
	s := New()
	accounts := benchAccounts(500)
	skip := func(a *deepai.Account) bool { return a.ID == "acc-7" }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.PickFiltered(accounts, "", skip) == nil {
			b.Fatal("应选到账号")
		}
	}
}

// BenchmarkHasEligible500 covers the all-busy determination path.
func BenchmarkHasEligible500(b *testing.B) {
	s := New()
	accounts := benchAccounts(500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !s.HasEligible(accounts, "") {
			b.Fatal("应有可调度账号")
		}
	}
}
