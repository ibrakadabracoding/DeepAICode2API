// Package envfile injects .env into the process environment, shared by the gateway and the
// register CLI.
//
// Why it must be a separate package: `os.Setenv` is a process-wide side effect, and it only
// matters if it runs before configuration is read (store.Open's master key, flag default
// evaluation). This logic used to be buried in internal/register and lazily triggered by
// LoadConfig, so the gateway never saw DEEPAI2API_MASTER_KEY from .env at startup and the
// state file could only remain plaintext.
package envfile

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Load loads .env by priority, exactly once.
//
// Path order:
//  1. DEEPAI2API_ENV_FILE explicitly specified;
//  2. .env in the working directory (source / unpacked deployment);
//  3. .env next to the executable (distribution deployment);
//  4. .env in the executable's parent directory (artifacts built under bin/).
//
// Real environment variables win: existing keys are not overwritten (loadDotEnv skips them).
func Load() {
	once.Do(func() { LoadFiles(defaultPaths()...) })
}

var once sync.Once

// defaultPaths returns the default .env candidate paths in priority order.
func defaultPaths() []string {
	var paths []string
	if p := strings.TrimSpace(os.Getenv("DEEPAI2API_ENV_FILE")); p != "" {
		paths = append(paths, p)
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, ".env"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths, filepath.Join(dir, ".env"), filepath.Join(dir, "..", ".env"))
	}
	return paths
}

// LoadFiles loads the given .env files in order and returns the number of keys actually injected.
//
// Files loaded later do not overwrite previously injected values, and real environment variables
// are not overwritten either. For tests and callers that need custom paths; use Load for the
// usual path.
func LoadFiles(paths ...string) int {
	n := 0
	for _, p := range paths {
		n += loadDotEnv(p)
	}
	return n
}

// loadDotEnv parses one .env file and injects its variables, returning the number of keys injected.
//
// Supports `KEY=VALUE`, `export KEY=VALUE`, # comments, and paired quotes; lines that fail to
// parse are silently skipped (consistent with past behavior, so one bad line cannot break startup).
func loadDotEnv(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k == "" || os.Getenv(k) != "" {
			continue
		}
		if err := os.Setenv(k, v); err == nil {
			n++
		}
	}
	return n
}
