package store

import (
	"sync"
	"testing"
)

// TestKeysSnapshotIsRaceFree locks in the contract that "Keys() must return a copy".
//
// ValidKey increments LastUsed / Requests in place on every data-plane request (holding the write lock), while the console
// after obtaining Keys(), reads the same fields outside the lock (server.safeKeyList). Returning a live pointer from the account pool
// would cause the following concurrency to be caught directly by -race (the historical implementation was like this); returning a copy is always clean.
func TestKeysSnapshotIsRaceFree(t *testing.T) {
	st := newStore(t)
	key := st.AddKey("console", "")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if !st.ValidKey(key.Key) {
				t.Error("ValidKey 应该一直成功")
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			// The fields read are exactly the same as safeKeyList: any sharing will trigger the race detector.
			for _, k := range st.Keys() {
				_ = k.Enabled
				_ = k.Group
				_ = k.LastUsed
				_ = k.Requests
			}
		}
	}()
	wg.Wait()

	if got := st.Keys()[0]; got.Requests != 500 {
		t.Fatalf("Requests = %d，期望 500", got.Requests)
	}
}

// TestAddKeyReturnsCopy ensures that AddKey's return value is not a live pointer in the account pool.
func TestAddKeyReturnsCopy(t *testing.T) {
	st := newStore(t)
	k := st.AddKey("x", "")

	// Modifying the return value must not affect storage.
	k.Enabled = false
	k.Name = "hacked"
	k.Requests = 999

	got := st.Keys()[0]
	if !got.Enabled {
		t.Fatal("AddKey 返回值是活指针：改 Enabled 污染了号池")
	}
	if got.Name != "x" || got.Requests != 0 {
		t.Fatalf("AddKey 返回值是活指针：%+v", got)
	}
}
