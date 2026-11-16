package server

import (
	"fmt"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/registry"
)

// catalog returns the current model catalog; when the catalog is empty it falls back to the built-in fallback models,
// so that the first startup (before the upstream catalog has been fetched) can still respond.
func (s *Server) catalog() []deepai.Model {
	catalog := s.Store.Models()
	if len(catalog) == 0 {
		return []deepai.Model{deepai.DefaultModelEntry()}
	}
	return catalog
}

// probeAccount returns an account that can be used for "catalog / lane probing".
//
// Since 2026-11-14 the upstream also requires an Api-Key for `/models` (without one it always returns 401 auth_required),
// and neither of these probes consumes Theos or depends on account identity, so the Key of any account will do.
// Account selection order: ready with machine fingerprint → any enabled account with a Key → nil (no credentials).
func (s *Server) probeAccount() *deepai.Account {
	var fallback *deepai.Account
	now := time.Now()
	for _, a := range s.Store.Accounts() {
		if a == nil || !a.Enabled || strings.TrimSpace(a.APIKey) == "" {
			continue
		}
		if fallback == nil {
			fallback = a
		}
		if deepai.AccountReady(a, now) && a.MachineID != "" {
			return a
		}
	}
	return fallback
}

// resolveModel resolves the requested model using the current catalog + aliases (lenient: falls back when unresolvable, never fails).
func (s *Server) resolveModel(name string) deepai.Model {
	m, _ := s.resolveModelOK(name)
	return m
}

// resolveModelOK resolves the model name and reports whether it hit the catalog with justification.
//
// Order: exact ID → custom alias → built-in alias → model name → (non-strict mode) substring
// → default model / first catalog entry. ok=false only occurs in strict mode.
//
// The point of strict mode (Settings.StrictModel): lenient fallback silently swaps a typo
// for a completely different model (e.g. gpt-6-sol → glm-5.3-flash),
// so the caller thinks they are testing A while actually running B.
func (s *Server) resolveModelOK(name string) (deepai.Model, bool) {
	catalog := s.catalog()
	settings := s.Store.Settings()
	def := s.defaultModelID()
	if m, ok := registry.ResolveStrict(catalog, settings.ModelAliases, name, def); ok {
		return m, true
	}
	if settings.StrictModel {
		return deepai.Model{}, false
	}
	return registry.Resolve(catalog, settings.ModelAliases, name, def), true
}

// defaultModelID returns the fallback target: the fallback model from settings takes priority, otherwise the catalog default.
//
// When the catalog default is an empty string it steps down further to the first catalog entry: otherwise the "default model" would display as empty,
// while inference has actually fallen back to the first catalog entry (line 37 of registry.Resolve), leaving the two inconsistent.
func (s *Server) defaultModelID() string {
	catalog := s.catalog()
	settings := s.Store.Settings()
	def := strings.TrimSpace(s.Store.DefaultModel())
	if def == "" && len(catalog) > 0 {
		def = catalog[0].ID
	}
	if settings.FallbackModel == "" {
		return def
	}
	if m, ok := registry.Find(catalog, settings.FallbackModel); ok {
		return m.ID
	}
	return settings.FallbackModel
}

// unknownModelMessage generates an actionable error message for strict mode.
//
// A bare "model does not exist" leaves the caller with nowhere to go; including near candidates (around gpt-6-sol
// that is gpt-6.1-sol) makes it obvious at a glance whether it is a typo or an unsupported channel.
func (s *Server) unknownModelMessage(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "(空)"
	}
	msg := fmt.Sprintf("模型 %q 不在上游目录中（严格模式已开启，不做静默回落）", name)
	if picks := registry.Suggest(s.catalog(), name, 6); len(picks) > 0 {
		msg += "；相近的模型：" + strings.Join(picks, "、")
	}
	msg += "。可用模型见 GET /v1/models；也可在控制台「模型别名」里把该名称映射到目录模型。"
	return msg
}

// adminModelViews converts catalog entries into the views delivered by the console.
//
// It only adds two things over a bare deepai.Model:
// - Fill in free: **same source** as Model.Free(). The frontend computes its own set of criteria,
// If the two sides disagree, the console will say free while it is actually billed (or vice versa);
// - Explicitly expose theosPerSecond: its JSON tag is omitempty, so when empty the frontend cannot get the key.
func adminModelViews(models []deepai.Model) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":              m.ID,
			"name":            m.Name,
			"reasoning":       m.Reasoning,
			"input":           m.Input,
			"contextWindow":   m.ContextWindow,
			"maxTokens":       m.MaxTokens,
			"premier":         m.Premier,
			"api":             m.API,
			"theosPerRequest": m.TheosPerRequest,
			"theosPerSecond":  m.TheosPerSecond,
			"free":            m.Free(),
		})
	}
	return out
}
