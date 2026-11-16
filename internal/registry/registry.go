// Package registry maintains the externally exposed model list and alias resolution.
//
// The DeepAI Code model catalog is dynamically delivered by the upstream /deepai_code/models; this package only performs
// resolution and alias expansion on the cached snapshot, and does not handle network requests.
package registry

import (
	"strings"

	"deepaicode2api/internal/deepai"
)

// Resolve resolves the model name passed in by the user to a model in the catalog.
//
// Tries in order: strip the deepai/ prefix → exact ID → custom alias → built-in alias →
// substring match → fall back to defaultID → fall back to the first model.
//
// When unrecognized, it does not error but falls back: this way any client (Claude Code / Cline /
// OpenAI SDK) can work out of the box with the model names it is used to.
func Resolve(catalog []deepai.Model, aliases map[string]string, name, defaultID string) deepai.Model {
	if m, ok := ResolveStrict(catalog, aliases, name, defaultID); ok {
		return m
	}
	n := normalize(name)
	if n != "" {
		// Loose matching: the model name contains the user input, or the user input contains the model ID.
		for _, m := range catalog {
			if strings.Contains(strings.ToLower(m.ID), n) {
				return m
			}
		}
	}

	if m, ok := lookup(catalog, normalize(defaultID)); ok {
		return m
	}
	if len(catalog) > 0 {
		return catalog[0]
	}
	return deepai.DefaultModelEntry()
}

// ResolveStrict matches the "evidence-based" resolution part of Resolve, but performs no implicit fallback.
//
// Tries in order: strip the prefix → exact ID → custom alias → built-in alias → model name match.
// When nothing matches, returns ok=false, and does **not** fall back to the default model and does **not** do substring matching.
//
// This is the basis for strict mode (Settings.StrictModel): substring matching would resolve "gpt-6" to
// the first "gpt-6-*" in the catalog (possibly a different model), and default fallback would turn any typo
// into the default model - both make the caller think they are testing A while actually running B.
func ResolveStrict(catalog []deepai.Model, aliases map[string]string, name, defaultID string) (deepai.Model, bool) {
	n := normalize(name)
	if n == "" {
		return deepai.Model{}, false
	}
	if m, ok := lookup(catalog, n); ok {
		return m, true
	}
	if target, ok := lookupAlias(aliases, n); ok {
		if m, ok := lookup(catalog, target); ok {
			return m, true
		}
	}
	if target, ok := builtinAliases[n]; ok {
		if m, ok := lookup(catalog, target); ok {
			return m, true
		}
	}
	for _, m := range catalog {
		if n == strings.ToLower(strings.ReplaceAll(m.Name, " ", "-")) {
			return m, true
		}
	}
	return deepai.Model{}, false
}

// Suggest returns catalog IDs related to name, for actionable error messages in strict mode.
//
// It is only a hint and does not participate in resolution. Similarity only requires "close enough": substring hit, or
// the first 4 characters are the same after ignoring punctuation (`gpt-6-sol` ↔ `gpt-6.1-sol` / `gpt-6-luna`;
// `mimo-v2.6-flash` ↔ `mimo-2.6-flash`).
func Suggest(catalog []deepai.Model, name string, max int) []string {
	n := normalize(name)
	if n == "" || max <= 0 {
		return nil
	}
	key := similarityKey(n)
	out := make([]string, 0, max)
	for _, m := range catalog {
		id := strings.ToLower(m.ID)
		label := normalize(m.Name)
		near := strings.Contains(id, n) || strings.Contains(n, id) || strings.Contains(label, n)
		if !near {
			near = commonPrefix(key, similarityKey(m.ID)) >= 4
		}
		if !near {
			continue
		}
		out = append(out, m.ID)
		if len(out) >= max {
			break
		}
	}
	return out
}

// similarityKey strips separators to avoid differences between `6.1` and `6-1`, "v2.6" and "2.6".
func similarityKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func commonPrefix(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// List returns the external model list (including custom aliases).
func List(catalog []deepai.Model, aliases map[string]string) []deepai.Model {
	out := make([]deepai.Model, len(catalog))
	copy(out, catalog)
	for alias, target := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if m, ok := lookup(catalog, normalize(target)); ok {
			m.ID = alias
			m.Name = alias + " → " + m.Name
			out = append(out, m)
		}
	}
	return out
}

// builtinAliases maps common client model names to DeepAI models.
//
// Contains only the default free models: users can override them in the console to any target.
var builtinAliases = map[string]string{
	"deepai":            deepai.DefaultModel,
	"deepai-code":       deepai.DefaultModel,
	"default":           deepai.DefaultModel,
	"gpt-3.5-turbo":     deepai.DefaultModel,
	"gpt-4":             deepai.DefaultModel,
	"gpt-4o":            deepai.DefaultModel,
	"gpt-4o-mini":       deepai.DefaultModel,
	"gpt-4.1":           deepai.DefaultModel,
	"claude-3-5-sonnet": deepai.DefaultModel,
	"claude-sonnet-4":   deepai.DefaultModel,
	"claude-3-haiku":    deepai.DefaultModel,
	"deepseek-chat":     deepai.DefaultModel,
	"glm-4-flash":       deepai.DefaultModel,
}

func normalize(name string) string {
	n := strings.TrimSpace(strings.ToLower(name))
	// Clients commonly use the deepai/<id> or openai/<id> prefix.
	for _, prefix := range []string{"deepai/", "deepai:", "openai/", "openai:"} {
		if strings.HasPrefix(n, prefix) {
			n = strings.TrimSpace(strings.TrimPrefix(n, prefix))
			break
		}
	}
	// Whitespace in model names is treated as a separator: "Claude Opus 5.5" is equivalent to "claude-opus-5.5".
	return strings.Join(strings.Fields(n), "-")
}

func lookup(catalog []deepai.Model, id string) (deepai.Model, bool) {
	if id == "" {
		return deepai.Model{}, false
	}
	for _, m := range catalog {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return deepai.Model{}, false
}

func lookupAlias(aliases map[string]string, key string) (string, bool) {
	for k, v := range aliases {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return normalize(v), true
		}
	}
	return "", false
}

// Find returns the model with an exact ID match.
func Find(catalog []deepai.Model, id string) (deepai.Model, bool) {
	return lookup(catalog, normalize(id))
}
