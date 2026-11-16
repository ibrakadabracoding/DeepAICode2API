package server

import (
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/store"
)

// 426 → borrow account credentials to probe /models for the new lane → switch lanes, retry successfully,
// and persist the result.
func TestLaneUnsupportedRefreshesAndRetries(t *testing.T) {
	var mu sync.Mutex
	var chatLanes []string
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		lane := r.Header.Get("X-DeepAI-Code-Theos")
		mu.Lock()
		chatLanes = append(chatLanes, lane)
		mu.Unlock()
		if lane != "7" {
			w.WriteHeader(http.StatusUpgradeRequired)
			_, _ = io.WriteString(w, `{"error":{"code":"lane_version_unsupported","message":"old build"}}`)
			return
		}
		if r.URL.Path == "/deepai_code/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"id":"glm-5.3-flash","name":"GLM","reasoning":true,`+
				`"input":["text"],"contextWindow":1000,"maxTokens":100}],"default":"glm-5.3-flash"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okChatResponse)
	})
	s, key := newTestServer(t, u)

	if got := s.CurrentLane(); got == "7" {
		t.Fatalf("初始车道不应是 7: %q", got)
	}
	rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, chatProbeBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("换道后应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if s.CurrentLane() != "7" {
		t.Fatalf("探测到的车道应生效，实际 %q", s.CurrentLane())
	}
	if got := s.Store.Settings().Lane; got != "7" {
		t.Fatalf("车道应落盘，实际 %q", got)
	}
	if got := s.Store.Settings().LaneBuild; got != deepai.LaneVersion {
		t.Fatalf("LaneBuild 应记录本次构建，实际 %q", got)
	}

	// The second request uses the new lane directly and does not trigger another probe.
	mu.Lock()
	before := len(chatLanes)
	mu.Unlock()
	rec = do(t, s, http.MethodPost, "/v1/chat/completions", key, chatProbeBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次应 200，实际 %d", rec.Code)
	}
	mu.Lock()
	added := chatLanes[before:]
	mu.Unlock()
	if len(added) != 1 || added[0] != "7" {
		t.Fatalf("第二次应只用新车道打一次，实际 %v", added)
	}
}

// When the state file still holds an old lane probed by a previous version, the upgrade should use
// the new compile-time lane directly instead of waiting for a 426 to switch.
func TestNewPrefersNewerCompiledLane(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.UpdateSettings(func(s *store.Settings) { s.Lane = "5" })
	if err := st.Save(); err != nil {
		t.Fatalf("保存状态: %v", err)
	}

	s := New(st)
	if got := s.CurrentLane(); got != deepai.LaneVersion {
		t.Fatalf("车道 = %q，期望编译期默认值 %q", got, deepai.LaneVersion)
	}
	if got := s.Up.Lane(); got != deepai.LaneVersion {
		t.Fatalf("客户端车道 = %q，期望 %q", got, deepai.LaneVersion)
	}
}

// When the probed value was verified by the current build, trust it even if it is older than the
// compile-time default: the upstream may only have the old lane available, and continuing to send
// the compiled value would keep returning 426.
func TestNewTrustsLowerLaneVerifiedByThisBuild(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.UpdateSettings(func(s *store.Settings) {
		s.Lane = "5"
		s.LaneBuild = deepai.LaneVersion
	})
	if err := st.Save(); err != nil {
		t.Fatalf("保存状态: %v", err)
	}

	s := New(st)
	if got := s.CurrentLane(); got != "5" {
		t.Fatalf("车道 = %q，期望本次构建验证过的 5", got)
	}
	if got := s.Up.Lane(); got != "5" {
		t.Fatalf("客户端车道 = %q，期望 5", got)
	}
}

// A probed value from an older build (LaneBuild mismatch) must not override the compiled value after upgrade.
func TestNewPrefersCompiledOverProbeFromOlderBuild(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.UpdateSettings(func(s *store.Settings) {
		s.Lane = "5"
		s.LaneBuild = "5" // compile-time lane when this state was written
	})
	if err := st.Save(); err != nil {
		t.Fatalf("保存状态: %v", err)
	}

	if got := New(st).CurrentLane(); got != deepai.LaneVersion {
		t.Fatalf("车道 = %q，期望升级后的编译值 %q", got, deepai.LaneVersion)
	}
}

// When the probed lane is newer than the compile-time default, the probe result still wins (the case
// where the upstream raises the lane first).
func TestNewKeepsNewerProbedLane(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.UpdateSettings(func(s *store.Settings) { s.Lane = "7" })
	if err := st.Save(); err != nil {
		t.Fatalf("保存状态: %v", err)
	}

	if got := New(st).CurrentLane(); got != "7" {
		t.Fatalf("车道 = %q，期望保留探测到的 7", got)
	}
}

// A hand-entered non-numeric lane must not wedge the gateway: fall back to the compile-time default.
func TestNewerLaneFallsBackOnGarbage(t *testing.T) {
	if got := newerLane("  "); got != deepai.LaneVersion {
		t.Fatalf("空值 = %q，期望 %q", got, deepai.LaneVersion)
	}
	if got := newerLane("latest"); got != deepai.LaneVersion {
		t.Fatalf("非数字 = %q，期望 %q", got, deepai.LaneVersion)
	}
}
