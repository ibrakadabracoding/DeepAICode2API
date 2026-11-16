package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deepaicode2api/internal/store"
)

// TestShapeCacheTTLAndLimit covers expiration and entry limit of shape memory.
func TestShapeCacheTTLAndLimit(t *testing.T) {
	s := &Server{}
	if s.shapeNeeded("k1") {
		t.Fatal("未标记时不应命中")
	}
	s.markShapeNeeded("k1")
	if !s.shapeNeeded("k1") {
		t.Fatal("标记后应命中")
	}
	// Expiration: set the expiry time to the past.
	s.shapeMu.Lock()
	s.shapeNeed[shapeCacheKey("k1")] = pastTime()
	s.shapeMu.Unlock()
	if s.shapeNeeded("k1") {
		t.Fatal("过期条目不应命中")
	}

	// Still usable after exceeding the limit (expired entries are cleaned up / rebuilt internally).
	for i := 0; i < presetShapeMax+10; i++ {
		s.markShapeNeeded("key-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + itoa64(int64(i)))
	}
	s.markShapeNeeded("last")
	if !s.shapeNeeded("last") {
		t.Fatal("超过上限后最新条目仍应命中")
	}
	if shapeCacheKey("") != "" {
		t.Fatal("空 key 不应产生哈希")
	}
}

func pastTime() (t time.Time) { return time.Now().Add(-time.Minute) }

// TestPresetDesktopShapeSkipsRejectedRoundTrip locks in P1:
// The first request goes through "400 → patch shape and retry"; subsequent requests patch system before the first send,
// so no extra upstream request that is doomed to fail is made.
func TestPresetDesktopShapeSkipsRejectedRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var attemptsWithoutSystem []bool
	var calls atomic.Int64
	// The handler needs to read "the just-recorded request body": newUpstream reads r.Body first and then hands it to the handler,
	// so here it is taken from u.last instead of reading r.Body again (which would yield an empty body).
	var u *upstream
	u = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/theos") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"enabled":true,"lane_version":5}`)
			return
		}
		payload := u.last(t).Payload
		hasSystem := false
		if msgs, ok := payload["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
					hasSystem = true
				}
			}
		}
		calls.Add(1)
		mu.Lock()
		attemptsWithoutSystem = append(attemptsWithoutSystem, !hasSystem)
		mu.Unlock()
		if !hasSystem {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"This request cannot be accepted.",`+
				`"type":"invalid_request_error","code":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	s, key := newTestServer(t, u)
	s.Store.UpdateSettings(func(st *store.Settings) { st.PresetDesktopShape = true })

	body := `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`

	// First time: learns "shape is needed", 2 upstream calls in total (1 rejected + 1 retry).
	if rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, body); rec.Code != http.StatusOK {
		t.Fatalf("第一次应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("第一次应有 2 次上游调用，实际 %d", calls.Load())
	}

	// Second time: system is preset, only 1 upstream call, and it includes system.
	before := calls.Load()
	if rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, body); rec.Code != http.StatusOK {
		t.Fatalf("第二次应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != before+1 {
		t.Fatalf("第二次应只打 1 次上游，实际增加 %d", calls.Load()-before)
	}
	mu.Lock()
	last := attemptsWithoutSystem[len(attemptsWithoutSystem)-1]
	mu.Unlock()
	if last {
		t.Fatal("第二次请求应在首次发送就带 system")
	}
}

// TestPresetDesktopShapeDisabledByDefault: when the switch is off, nothing is preset and the learning path is still used.
func TestPresetDesktopShapeDisabledByDefault(t *testing.T) {
	var calls atomic.Int64
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/theos") {
			_, _ = io.WriteString(w, `{"enabled":true,"lane_version":5}`)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	s, key := newTestServer(t, u)

	body := `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		if rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, body); rec.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d", rec.Code)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("开关关闭时每个请求只打 1 次上游，实际 %d", got)
	}
}
