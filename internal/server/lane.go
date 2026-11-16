package server

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/store"
)

// laneProbeCooldown is the minimum interval between two lane probes.
//
// When the lane is wrong, 426s arrive in batches (all requests hit it at once); without a cooldown,
// one lane upgrade turns every failed request into a /models probe.
const laneProbeCooldown = 60 * time.Second

// CurrentLane returns the currently effective lane.
//
// When the probe result has been verified by the **current build** (LaneBuild == compile-time lane),
// trust it unconditionally, even if it is older than the compiled value-the upstream may only have
// the old lane available, and continuing to send the compiled value would keep returning 426.
// Otherwise fall back to newerLane: the state file may come from an older version, so after an
// upgrade the newer compiled value should be used directly.
func (s *Server) CurrentLane() string {
	st := s.Store.Settings()
	lane := strings.TrimSpace(st.Lane)
	if lane != "" && st.LaneBuild == deepai.LaneVersion {
		return lane
	}
	return newerLane(lane)
}

// newerLane returns the newer of the persisted lane value and the compile-time default.
//
// Both sides are compared as decimal integers; if either is not a number it falls back to the
// compile-time default, so a hand-entered odd value can't wedge the lane (a wrong lane makes the
// upstream return 426, and probing rewrites it).
func newerLane(stored string) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return deepai.LaneVersion
	}
	s, errStored := strconv.Atoi(stored)
	c, errCompiled := strconv.Atoi(deepai.LaneVersion)
	if errStored != nil || errCompiled != nil {
		return deepai.LaneVersion
	}
	if s > c {
		return stored
	}
	return deepai.LaneVersion
}

// RefreshLane probes an available lane once and persists it, returning the lane finally used.
//
// The probe hits /models (consumes no Theos and triggers no check-in), trying the deepai.Client
// candidate sequence (compiled lane first) until a 2xx-once the upstream raises the lane the gateway
// follows automatically, with no code change. **As of 2026-11-14 /models requires an Api-Key**, so
// the probe must borrow an account's credentials; with no account the prober only gets a 401 and
// skips probing (the compiled LaneVersion + 426 fallback still applies).
// During cooldown it returns the current value directly; on probe failure it keeps the old value
// and hands the error to the caller.
func (s *Server) RefreshLane(ctx context.Context) (string, error) {
	if strings.TrimSpace(s.Up.Machine) == "" {
		return s.CurrentLane(), errors.New("缺少机器指纹，无法探测车道")
	}
	acct := s.probeAccount()
	if acct == nil {
		return s.CurrentLane(), errors.New("没有可用账号：上游 /models 需要 Api-Key，无法探测车道")
	}

	s.laneMu.Lock()
	if time.Since(s.laneCheckedAt) < laneProbeCooldown {
		lane := s.CurrentLane()
		s.laneMu.Unlock()
		return lane, nil
	}
	s.laneCheckedAt = time.Now()
	s.laneMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lane, err := s.Up.DetectLane(cctx, acct)
	if err != nil {
		return s.CurrentLane(), err
	}
	s.applyLane(lane)
	return lane, nil
}

// applyLane writes the lane back to settings and all cached clients.
//
// Client.SetLane uses atomic, so switching lanes while requests are in flight is safe here;
// newly created proxy clients pick up the current lane in clientFor.
func (s *Server) applyLane(lane string) {
	lane = strings.TrimSpace(lane)
	if lane == "" {
		return
	}
	prev := s.CurrentLane()

	s.Store.UpdateSettings(func(st *store.Settings) {
		st.Lane = lane
		st.LaneBuild = deepai.LaneVersion
	})
	if err := s.Store.Save(); err != nil {
		log.Printf("保存车道版本失败: %v", err)
	}

	s.Up.SetLane(lane)
	s.proxyMu.Lock()
	for _, c := range s.proxyClients {
		c.SetLane(lane)
	}
	s.proxyMu.Unlock()

	if prev != lane {
		log.Printf("[lane] 车道切换: %s → %s", prev, lane)
	}
}
