package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// fillPool replaces the account pool with n available accounts (first clearing the one that comes with newTestServer).
func fillPool(t *testing.T, s *Server, n int) {
	t.Helper()
	for _, a := range s.Store.Accounts() {
		s.Store.RemoveAccount(a.ID)
	}
	for i := 0; i < n; i++ {
		id := "acc-" + strconv.Itoa(i)
		s.Store.UpsertAccount(&deepai.Account{
			ID: id, Name: id, APIKey: "key-" + strconv.Itoa(i),
			Enabled: true, Status: "ok",
		})
	}
}

// TestPickFreeAccountAllocationsBounded is the regression guard for P0-1.
//
// The old implementation was "loop n times, each time deep-copying the whole pool via Accounts() + taking the NextAccountGroup write lock",
// allocations grew linearly with pool size (with 500 accounts, a single selection did over a thousand allocations). Now it should
// only be "the one selected account + a small fixed overhead".
func TestPickFreeAccountAllocationsBounded(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fillPool(t, s, 500)

	const budget = 40 // far less than 500 (the old implementation did at least 500×2 allocations)
	got := testing.AllocsPerRun(50, func() {
		a, ok, _ := s.pickFreeAccount("")
		if !ok || a == nil {
			t.Fatalf("应当选到账号")
		}
		s.releaseAccount(a.ID)
	})
	if got > budget {
		t.Fatalf("单次选号分配 %.1f 次，基线 %d：又在全池拷贝了？", got, budget)
	}
}

// TestPickFreeAccountSkipsBusy verifies that busy concurrency slots are excluded during selection,
// and that "all busy" can be distinguished from "no schedulable accounts".
func TestPickFreeAccountSkipsBusy(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fillPool(t, s, 2)

	// Each account allows at most maxInFlightPerAccount concurrent requests; first fill them all.
	var held []string
	for i := 0; i < 2*maxInFlightPerAccount; i++ {
		a, ok, candidates := s.pickFreeAccount("")
		if !ok {
			t.Fatalf("第 %d 次应仍有空槽（candidates=%v）", i, candidates)
		}
		held = append(held, a.ID)
	}
	if len(held) != 2*maxInFlightPerAccount {
		t.Fatalf("应占满 2 个账号 × %d 个槽位，实际 %d", maxInFlightPerAccount, len(held))
	}
	// All slots full: no account can be obtained, but candidates must be true (the pool has accounts, they are just busy).
	if a, ok, candidates := s.pickFreeAccount(""); ok || a != nil || !candidates {
		t.Fatalf("全忙时不应选中账号：a=%v ok=%v candidates=%v", a, ok, candidates)
	}
	for _, id := range held {
		s.releaseAccount(id)
	}
	if a, ok, _ := s.pickFreeAccount(""); !ok || a == nil {
		t.Fatal("槽位释放后应能重新选到账号")
	}
}

// TestPickFreeAccountEmptyPool distinguishes "no accounts" from "all busy".
func TestPickFreeAccountEmptyPool(t *testing.T) {
	s, _ := newTestServer(t, nil)
	for _, a := range s.Store.Accounts() {
		s.Store.RemoveAccount(a.ID)
	}
	if a, ok, candidates := s.pickFreeAccount(""); ok || a != nil || candidates {
		t.Fatalf("空池应返回 (nil,false,false)，实际 (%v,%v,%v)", a, ok, candidates)
	}
}

// TestCloseInterruptsReprobeBackoff is the regression guard for P0-3.
//
// The quota re-probe backoff is up to 12 minutes; Close must be able to interrupt the sleep and return immediately,
// otherwise process shutdown will hang until timeout.
func TestCloseInterruptsReprobeBackoff(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.quotaReprobeDelays = []time.Duration{time.Hour}

	s.scheduleQuotaReprobe("acc-1")

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Close(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 未能在 2s 内返回：退避睡眠没有被关停信号打断")
	}
}

// TestCloseWaitsForProbes verifies that Close waits for registered probes to persist to disk.
func TestCloseWaitsForProbes(t *testing.T) {
	s, _ := newTestServer(t, nil)

	started := make(chan struct{})
	s.quotaWG.Add(1)
	go func() {
		defer s.quotaWG.Done()
		close(started)
		time.Sleep(100 * time.Millisecond)
	}()
	<-started

	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Close(ctx)
	if elapsed := time.Since(begin); elapsed < 90*time.Millisecond {
		t.Fatalf("Close 提前返回（%s）：没有等待后台任务", elapsed)
	}
}
