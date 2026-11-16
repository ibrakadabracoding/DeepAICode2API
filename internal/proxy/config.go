package proxy

import (
	"os"
	"strings"

	"deepaicode2api/internal/envfile"
)

// Config is the source configuration for the proxy pool.
type Config struct {
	// Pool is the proxy list written directly in the environment variable (PROXY_POOL).
	Pool string
	// File points to the proxy file (PROXY_POOL_FILE), read only when Pool is empty.
	File string
}

// LoadConfig reads the proxy pool configuration; real environment variables take
// precedence over .env.
//
// DEEPAI_REGISTER_PROXY_POOL is an alias from the registration pipeline era, kept for
// compatibility with existing .env files.
func LoadConfig() *Config {
	envfile.Load()
	return &Config{
		Pool: firstNonEmpty(os.Getenv("PROXY_POOL"), os.Getenv("DEEPAI_REGISTER_PROXY_POOL")),
		File: strings.TrimSpace(os.Getenv("PROXY_POOL_FILE")),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
