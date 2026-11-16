package server

import (
	"deepaicode2api/internal/deepai"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// maxRequestBody limits the inbound request body size.
//
// Actual reading has moved to httpx.ReadBodyWithLimit (same constant in httpx.MaxRequestBody).
const maxRequestBody = 32 << 20

// affinityPattern matches the desktop client: only session identifiers from the safe character set are
// passed through.
var affinityPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

// affinityFor resolves the session stickiness identifier (response header first, then prompt_cache_key).
func affinityFor(r *http.Request, payload map[string]any) string {
	if r != nil {
		if v := strings.TrimSpace(r.Header.Get("X-Session-Affinity")); affinityPattern.MatchString(v) {
			return v
		}
	}
	if payload != nil {
		if v, ok := payload["prompt_cache_key"].(string); ok && affinityPattern.MatchString(strings.TrimSpace(v)) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// nonStreamBudget is the overall budget for a non-streaming request (account selection + retries + body read).
//
// Without it, "MaxRetries × Retry-After≤30s" could theoretically hang for over 90s.
const nonStreamBudget = 90 * time.Second

// requestModelName extracts the model field from the request.
func requestModelName(payload map[string]any) string {
	v, _ := payload["model"].(string)
	return strings.TrimSpace(v)
}

// estimatePromptLen roughly counts the characters in messages / input.
func estimatePromptLen(payload map[string]any) int {
	if v, ok := payload["messages"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			return len(raw)
		}
	}
	if v, ok := payload["input"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			return len(raw)
		}
	}
	return 0
}

// chatTextLen counts the characters in the content + chain-of-thought of a chat response.
func chatTextLen(m map[string]any) int {
	choices, _ := m["choices"].([]any)
	total := 0
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := cm["message"].(map[string]any); ok {
			total += lenOfString(msg["content"])
			total += lenOfString(msg["reasoning"])
		}
		if d, ok := cm["delta"].(map[string]any); ok {
			total += lenOfString(d["content"])
			total += lenOfString(d["reasoning"])
		}
	}
	return total
}

// requestGroup returns the account pool group bound to the current request key (empty = all groups when there is no key).
func (s *Server) requestGroup(r *http.Request) string {
	if r == nil {
		return ""
	}
	key := extractKey(r)
	if key == "" {
		return ""
	}
	return s.Store.KeyGroup(key)
}

// nextAccount picks a rotation account from the group the request belongs to.
func (s *Server) nextAccount(r *http.Request) *deepai.Account {
	return s.Store.NextAccountGroup(s.requestGroup(r))
}

// clientFor returns the upstream client bound to the account (cached by proxy).
func (s *Server) clientFor(acct *deepai.Account) *deepai.Client {
	if acct == nil || strings.TrimSpace(acct.Proxy) == "" {
		return s.Up
	}
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if s.proxyClients == nil {
		s.proxyClients = map[string]*deepai.Client{}
	}
	if c, ok := s.proxyClients[acct.Proxy]; ok {
		return c
	}
	c := deepai.NewClientWithProxy(acct.Proxy)
	if budget := time.Duration(s.Store.Settings().CheckinBlockMS) * time.Millisecond; budget > 0 {
		c.SetCheckInBudget(budget)
	}
	s.proxyClients[acct.Proxy] = c
	return c
}
