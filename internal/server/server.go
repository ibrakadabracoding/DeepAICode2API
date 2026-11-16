// Package server exposes an OpenAI-compatible HTTP API.
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/proxy"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// Server is the gateway HTTP service.
type Server struct {
	Store *store.Store
	Up    *deepai.Client
	// Login stores in-progress browser login sessions (in-memory).
	Login *loginSessions

	refreshMu    sync.Mutex
	proxyMu      sync.Mutex
	proxyClients map[string]*deepai.Client

	// enrollPool is an in-process singleton used for automatic proxy assignment when enrolling into the pool.
	//
	// A browser login enrolls only one account at a time; if a new pool is created each time, the rotation cursor will always start from 0,
	// so all accounts end up on the first egress (equivalent to no assignment). The singleton allows consecutive enrollments to actually rotate.
	enrollPoolOnce sync.Once
	enrollPool     *proxy.ProxyPool

	// inflight limits the number of concurrent upstream requests per account.
	// Scheduling only guarantees "who is selected in this round"; it does not guarantee that concurrency won't all be concentrated on a few accounts:
	// During load testing, 12 concurrent requests may all land on the same one or two accounts, amplifying the long tail after upstream rate limiting.
	inflightMu sync.Mutex
	inflight   map[string]int

	// quotaReprobeDelays is the re-probe backoff sequence for 0 quota after enrollment (tests can inject short values).
	quotaReprobeDelays []time.Duration

	// laneMu / laneCheckedAt throttle lane probing: 426 responses appear in batches,
	// without cooldown, a single lane upgrade turns into a probing storm.
	laneMu        sync.Mutex
	laneCheckedAt time.Time

	// admissionWait is the queueing limit when "all slots are full" (tests can inject 0 to return 429 immediately).
	admissionWait time.Duration

	// quotaWG tracks scheduled background persistence work (quota re-probe / enrollment liveness probe / on-demand re-probe).
	//
	// These goroutines still need to call Save() to persist to disk after writing memory; tests must wait for them to fully finish
	// before ending; otherwise t.TempDir() cleanup will collide with persistence (on CI this manifests as
	// "TempDir RemoveAll cleanup: directory not empty").
	// On process exit, Close waits for them uniformly to avoid "background writes after the last Save".
	quotaWG sync.WaitGroup

	// stopCh is closed on Close to interrupt long sleeps in background tasks (quota re-probe backoff up to 12 minutes).
	stopCh   chan struct{}
	stopOnce sync.Once

	// probing is the in-flight deduplication table for quota probing (by account ID).
	//
	// Three paths may probe the same batch of accounts simultaneously: manual checkin, delayed re-probe after enrollment,
	// On-demand re-probe during inference. Without it, the same account gets hit repeatedly, wasting quota
	// and easily triggering upstream risk control.
	probeMu sync.Mutex
	probing map[string]struct{}

	// probeSem is the global in-flight probe limit (shared by manual batches / background daemon / on-demand re-probe).
	//
	// Quota snapshots for 653 accounts all go stale at the same time; if on-demand re-probe is unbounded, a single traffic burst can
	// stack up dozens or hundreds of parallel /theos requests - that is the shape most likely to trigger upstream risk control.
	probeSem chan struct{}

	// limiter is a per-key token bucket + concurrency gate (data-plane rate limiting).
	limiter *keyRateLimiter

	// adminGuard is failure backoff for the admin password (anti-brute-force); adminGuardBase can be injected by tests.
	adminGuard     *adminGuard
	adminGuardBase time.Duration

	// metrics are in-process counters exported by /metrics (atomics, no lock needed).
	metrics *metrics

	// Version is the build version (injected by main via -ldflags); an empty value is exposed externally as dev.
	Version string

	// versionMu / versionCache / versionAt cache the result of the "latest release" query,
	// avoiding a GitHub request every time the console is opened (unauthenticated requests are rate-limited).
	// versionInFlight non-nil means a goroutine is already querying (concurrent requests wait for its result).
	versionMu       sync.Mutex
	versionCache    *versionInfo
	versionAt       time.Time
	versionInFlight chan struct{}

	// shapeNeed remembers "which callers need the desktop shape" (see shape.go).
	shapeMu   sync.Mutex
	shapeNeed map[string]time.Time
}

// New constructs the service.
func New(st *store.Store) *Server {
	s := &Server{
		Store: st, Up: deepai.NewClient(), Login: newLoginSessions(),
		proxyClients:       map[string]*deepai.Client{},
		inflight:           map[string]int{},
		probing:            map[string]struct{}{},
		probeSem:           make(chan struct{}, probeWorkers),
		quotaReprobeDelays: quotaReprobeDelays,
		admissionWait:      admissionWait,
		stopCh:             make(chan struct{}),
		limiter:            newKeyRateLimiter(),
		adminGuard:         newAdminGuard(),
		metrics:            newMetrics(),
	}
	// The lane detected last time takes priority (preserved across restarts); CurrentLane distinguishes "the probe value verified by this build"
	// from "leftovers of an older version"; only the latter falls back to the newer compile-time default.
	if lane := strings.TrimSpace(st.Settings().Lane); lane != "" {
		s.Up.SetLane(s.CurrentLane())
	}
	s.applyRuntimeSettings()
	return s
}

// applyRuntimeSettings syncs "runtime-adjustable" settings to already-cached clients.
//
// Lane goes through SetLane; check-in budget goes through SetCheckInBudget. Both must cover
// s.Up and the proxy-cached clients at the same time, otherwise direct accounts and proxy accounts behave inconsistently.
func (s *Server) applyRuntimeSettings() {
	budget := time.Duration(s.Store.Settings().CheckinBlockMS) * time.Millisecond
	s.Up.SetCheckInBudget(budget)
	s.proxyMu.Lock()
	for _, c := range s.proxyClients {
		c.SetCheckInBudget(budget)
	}
	s.proxyMu.Unlock()
}

// Handler returns the full routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness probe
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)

	// Inference (OpenAI-compatible)
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))

	// Anthropic-compatible
	mux.HandleFunc("/v1/messages", s.withAuth(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.withAuth(s.handleAnthropicCountTokens))

	// Audio
	mux.HandleFunc("/v1/audio/transcriptions", s.withAuth(s.handleTranscriptions))
	mux.HandleFunc("/v1/audio/translations", s.withAuth(s.handleTranscriptions))

	// DeepAI native capability extensions
	mux.HandleFunc("/v1/deepai/web_search", s.withAuth(s.handleWebSearch))
	mux.HandleFunc("/v1/deepai/find_service", s.withAuth(s.handleFindService))
	mux.HandleFunc("/v1/deepai/theos", s.withAuth(s.handleTheos))
	mux.HandleFunc("/v1/deepai/usage", s.withAuth(s.handleUsage))
	// Compatible shorthand
	mux.HandleFunc("/v1/web_search", s.withAuth(s.handleWebSearch))
	mux.HandleFunc("/v1/theos", s.withAuth(s.handleTheos))
	mux.HandleFunc("/v1/usage", s.withAuth(s.handleUsage))

	// Console API (all go through admin authentication + same-origin check)
	mux.HandleFunc("/admin/api/state", s.withAdminAuth(s.handleAdminState))
	mux.HandleFunc("/admin/api/overview", s.withAdminAuth(s.handleAdminOverview))
	mux.HandleFunc("/admin/api/accounts", s.withAdminAuth(s.handleAdminAccounts))
	mux.HandleFunc("/admin/api/keys", s.withAdminAuth(s.handleAdminKeys))
	mux.HandleFunc("/admin/api/logs", s.withAdminAuth(s.handleAdminLogs))
	mux.HandleFunc("/admin/api/stats", s.withAdminAuth(s.handleAdminStats))
	mux.HandleFunc("/admin/api/settings", s.withAdminAuth(s.handleAdminSettings))
	mux.HandleFunc("/admin/api/checkin", s.withAdminAuth(s.handleAdminCheckin))
	// Version and update check (the server queries GitHub on the client's behalf; the frontend CSP does not allow direct connections).
	mux.HandleFunc("/admin/api/version", s.withAdminAuth(s.handleAdminVersion))

	// Monitoring: Prometheus text format (same authentication level as /admin/api/*).
	mux.HandleFunc("/metrics", s.withAdminAuth(s.handleMetrics))

	mux.HandleFunc("/admin/api/models", s.withAdminAuth(s.handleAdminModels))
	mux.HandleFunc("/admin/api/login/start", s.withAdminAuth(s.handleLoginStart))
	mux.HandleFunc("/admin/api/login/poll", s.withAdminAuth(s.handleLoginPoll))
	// Paste callback URL: when the browser callback cannot be reached under Docker / remote deployment, use this entry point to complete redemption.
	mux.HandleFunc("/admin/api/login/callback", s.withAdminAuth(s.handleLoginCallback))
	// Browser login redirect: the session id is a one-time random value, and the server only redirects to the fixed upstream login page.
	mux.HandleFunc("/login/open", s.handleLoginOpen)

	// Passphrase login page: the console top bar no longer hosts a passphrase input box; unauthenticated users are redirected here.
	mux.HandleFunc("/login", serveLogin)
	// Passphrase verification entry: only answers "is this passphrase correct" and returns no sensitive information.
	mux.HandleFunc("/admin/api/auth", s.withAdminAuth(s.handleAdminAuth))

	// Console static assets: styles, tab icons, and ES module scripts (the page itself is served at /).
	mux.HandleFunc("/style.css", s.handleConsoleAsset)
	// Fixed paths the browser requests automatically: without them, every console open adds another 404.
	mux.HandleFunc("/favicon.ico", s.handleConsoleAsset)
	mux.HandleFunc("/apple-touch-icon.png", s.handleConsoleAsset)
	mux.HandleFunc("/js/", s.handleConsoleAsset)
	mux.HandleFunc("/", s.handleRoot)

	// Compression is innermost: the outer layer (observability) sees the compressed byte count.
	return s.withCORS(s.withObservability(httpx.WithGzip(mux)))
}

// RefreshModels pulls the model catalog from upstream and persists it to disk.
func (s *Server) RefreshModels(ctx context.Context) ([]deepai.Model, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// As of 2026-11-14, upstream /models requires an Api-Key: when no account is available, return an actionable
	// error in advance instead of passing the upstream 401 straight through to the console (which looks like "Key invalid").
	acct := s.probeAccount()
	if acct == nil {
		return nil, errors.New("上游 /models 需要 Api-Key：请先导入或添加一个账号再刷新目录")
	}
	models, def, err := s.Up.FetchModels(ctx, acct)
	if err != nil {
		return nil, err
	}
	s.Store.SetModels(models, def)
	if err := s.Store.Save(); err != nil {
		log.Printf("保存模型目录失败: %v", err)
	}
	return models, nil
}

// --- Base endpoints ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// See deepai.AccountReady / AccountDegraded and quota validity for the criteria;
	// Shares poolSnapshot with /metrics to avoid conflicting numbers in two places.
	pool := s.poolSnapshot()
	models := s.catalog()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"version":       s.currentVersion(),
		"accounts":      pool.Total,
		"ready":         pool.Ready,
		"degraded":      pool.Degraded,
		"blocked":       pool.Blocked,
		"quota_fresh":   pool.QuotaFresh,
		"quota_due":     pool.QuotaDue,
		"models":        len(models),
		"default_model": s.Store.DefaultModel(),
		"strict_model":  s.Store.Settings().StrictModel,
		"machine_id":    shortMachine(s.Store.MachineID()),
		"machine_ready": s.Store.MachineID() != "",
		// The console uses this to decide whether to send the user to /login: when no password is set, allow local access directly.
		"admin_required": s.Store.Settings().AdminPassword != "",
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	catalog := s.catalog()
	models := registry.List(catalog, s.Store.Settings().ModelAliases)
	data := make([]map[string]any, 0, len(models))
	now := time.Now().Unix()
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": "deepai",
			"deepai": map[string]any{
				"name":              m.Name,
				"reasoning":         m.Reasoning,
				"input":             m.Input,
				"context_window":    m.ContextWindow,
				"max_tokens":        m.MaxTokens,
				"premier":           m.Premier,
				"free":              m.Free(),
				"api":               m.API,
				"theos_per_request": m.TheosPerRequest,
				"theos_per_second":  m.TheosPerSecond,
			},
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleRoot returns the console single page; other unknown paths return 404.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		httpx.WriteErr(w, http.StatusNotFound, "未找到该路径", "invalid_request_error")
		return
	}
	serveConsole(w, r)
}
