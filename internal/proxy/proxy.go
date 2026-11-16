// Package proxy provides the shared proxy pool for the account pool: rotation, redacted labels, and proxy file reading.
//
// Historical context: the proxy pool originally lived inside the registration flow (internal/register). The email registration module
// was removed, but the account pool still needs "per-account egress" (the upstream daily free quota is shared per egress IP),
// so it was split into an independent package that no longer depends on any registration/receiving logic.
package proxy

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
)

// ProxyPool is a thread-safe rotating proxy pool.
type ProxyPool struct {
	entries []string

	mu   sync.Mutex
	next int
}

// NewProxyPool parses the proxy list; returns (nil, nil) when raw is empty.
//
// Supports newline / comma / semicolon separators; each element must be a full URL:
// http://user:pass@host:port, https://... or socks5://...
func NewProxyPool(raw string) (*ProxyPool, error) {
	entries := ParseProxyList(raw)
	if len(entries) == 0 {
		return nil, nil
	}
	for _, entry := range entries {
		u, err := url.Parse(entry)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("代理格式非法：%s", ProxyLabel(entry))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("不支持的代理协议 %q：%s", u.Scheme, ProxyLabel(entry))
		}
	}
	return &ProxyPool{entries: entries}, nil
}

// ParseProxyList parses a multi-line / comma-separated proxy string into a list (trims empties, deduplicates, strips comments).
func ParseProxyList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	replacer := strings.NewReplacer("\r\n", "\n", "\r", "\n", ";", "\n", ",", "\n", " ", "\n", "\t", "\n")
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(replacer.Replace(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// Len returns the number of proxies.
func (p *ProxyPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Next returns a proxy in rotation; returns an empty string when the pool is empty.
func (p *ProxyPool) Next() string {
	if p == nil || len(p.entries) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	raw := p.entries[p.next%len(p.entries)]
	p.next++
	return raw
}

// ProxyLabel returns host:port without credentials (for logs / console).
func ProxyLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "-"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid"
	}
	return u.Host
}

// ReadProxyFile reads a proxy file (one per line, supports # comments).
func ReadProxyFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
