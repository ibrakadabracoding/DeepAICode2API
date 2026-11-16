package deepai

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// RandomHex generates a random hex string of n bytes.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// RandomToken generates a hex string from n random bytes (used for OAuth state).
func RandomToken(n int) string { return RandomHex(n) }

// KeyFingerprint returns the first 16 hex characters of the API Key's sha256.
func KeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:8])
}

// ShortHash returns a short hash of the API Key, used for display.
func ShortHash(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:4])
}

// MaskSecret masks a sensitive string, keeping only the leading and trailing fragments.
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return s[:1] + "***"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// HomeDir returns the current user's home directory (falls back to the current directory on failure).
func HomeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("USERPROFILE"); h != "" {
		return h
	}
	return "."
}

// DesktopBaseDirs returns candidate data directories for the local DeepAI Code desktop app.
//
// Upstream logic (apps/desktop/DesktopStatePaths.ts):
//
//	baseDir = T3CODE_HOME || ~/.deepai-code
//
// stateDir = baseDir/userdata (in dev mode, when not explicitly specified, use baseDir/dev)
//
// secrets is located at stateDir/secrets.
func DesktopBaseDirs(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	var out []string
	for _, env := range []string{"T3CODE_HOME", "DEEPAI_CODE_HOME"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			out = append(out, v)
		}
	}
	out = append(out, filepath.Join(HomeDir(), ".deepai-code"))
	return dedupe(out)
}

// DesktopSecretDirs expands baseDir into directories that may contain secrets.
//
// It also accepts baseDir, stateDir, the secrets directory, or the credential file itself,
// so users can point the path at any level directly.
func DesktopSecretDirs(explicit string) []string {
	var out []string
	for _, base := range DesktopBaseDirs(explicit) {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		info, err := os.Stat(base)
		if err == nil && !info.IsDir() {
			out = append(out, filepath.Dir(base))
			continue
		}
		out = append(out,
			filepath.Join(base, "userdata", "secrets"),
			filepath.Join(base, "secrets"),
			filepath.Join(base, "dev", "secrets"),
		)
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		key := strings.ToLower(filepath.Clean(v))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
	}
	return out
}

// EnvOr returns the environment variable value or a default.
func EnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// EnvInt returns an integer environment variable or a default.
func EnvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// MaskProxy returns only the proxy's host:port (never leaks username/password).
func MaskProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
