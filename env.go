package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Startup flag and environment variable parsing.
// Extracted so main() only handles assembly, and environment defaults live in one place that is
// easy to cross-check against README §5.

func defaultDataPath() string {
	const name = "deepaicode2api-data.json"

	if exe, err := os.Executable(); err == nil {
		legacy := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "deepaicode2api", name)
	}
	return name
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// isLoopbackListen reports whether the listen address accepts only local connections.
//
// An empty string / 0.0.0.0 / :: all mean listening on every interface, which is external
// exposure and does not count as loopback.
func isLoopbackListen(host string) bool {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
