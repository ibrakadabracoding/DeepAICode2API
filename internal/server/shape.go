package server

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Learned memory of request body shape.
//
// 2026-11-13 production: requests from the same machine + node UA + missing system get a 400 from upstream
// (This request cannot be accepted.). The gateway's current strategy is "pass through first → 400 →
// patch shape and retry", so each such request hits upstream one extra time (99 times in 82 minutes in production).
//
// Memory is stored for 10 minutes keyed by the API Key hash (not plaintext): once a caller is confirmed to need
// the desktop shape, subsequent requests missing system get system patched in **before the first send**, saving
// that doomed round trip; if the caller supplies system itself, no rewriting is done.
//
// Only takes effect when Settings.PresetDesktopShape is enabled (off by default):
// this is a sampling semantics change (one extra system message), so it must be opted into explicitly.
const (
	presetShapeTTL = 10 * time.Minute
	// presetShapeMax limits the number of memory entries to avoid memory blowup from random keys.
	presetShapeMax = 1024
)

func shapeCacheKey(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:6])
}

// shapeNeeded reports whether this caller was recently rejected by upstream for shape.
func (s *Server) shapeNeeded(apiKey string) bool {
	k := shapeCacheKey(apiKey)
	if k == "" {
		return false
	}
	s.shapeMu.Lock()
	defer s.shapeMu.Unlock()
	until, ok := s.shapeNeed[k]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(s.shapeNeed, k)
		return false
	}
	return true
}

// markShapeNeeded remembers that "this caller needs the desktop shape".
func (s *Server) markShapeNeeded(apiKey string) {
	k := shapeCacheKey(apiKey)
	if k == "" {
		return
	}
	s.shapeMu.Lock()
	defer s.shapeMu.Unlock()
	if s.shapeNeed == nil {
		s.shapeNeed = map[string]time.Time{}
	}
	if len(s.shapeNeed) >= presetShapeMax {
		// Simple cleanup: delete expired entries first; if still over the limit, rebuild entirely (better to forget old memories).
		now := time.Now()
		for key, until := range s.shapeNeed {
			if now.After(until) {
				delete(s.shapeNeed, key)
			}
		}
		if len(s.shapeNeed) >= presetShapeMax {
			s.shapeNeed = map[string]time.Time{}
		}
	}
	s.shapeNeed[k] = time.Now().Add(presetShapeTTL)
}
