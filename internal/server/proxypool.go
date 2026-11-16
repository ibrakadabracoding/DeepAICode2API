package server

import (
	"log"
	"strings"

	"deepaicode2api/internal/proxy"
)

// loadProxyPool builds the rotation proxy pool in the order PROXY_POOL → PROXY_POOL_FILE.
//
// The configuration source stays consistent with batch assignment (action=assign-proxy) and browser login enrollment;
// Returns (nil, nil) when not configured, so the caller skips automatic assignment accordingly.
func loadProxyPool() (*proxy.ProxyPool, error) {
	cfg := proxy.LoadConfig()
	raw := cfg.Pool
	if strings.TrimSpace(raw) == "" && strings.TrimSpace(cfg.File) != "" {
		if fileRaw, err := proxy.ReadProxyFile(cfg.File); err == nil {
			raw = fileRaw
		}
	}
	return proxy.NewProxyPool(raw)
}

// enrollProxyPool returns the enrollment proxy pool reused within the process.
//
// On parse failure, only log and disable it (do not block enrollment): the proxy is an optimization, not a requirement.
func (s *Server) enrollProxyPool() *proxy.ProxyPool {
	s.enrollPoolOnce.Do(func() {
		pool, err := loadProxyPool()
		if err != nil {
			log.Printf("[enroll] 代理池配置错误，跳过自动分配: %v", err)
			return
		}
		s.enrollPool = pool
	})
	return s.enrollPool
}
