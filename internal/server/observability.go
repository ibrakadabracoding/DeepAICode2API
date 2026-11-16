package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/logx"
)

// itoa64 / ftoa are the required conversions for the Prometheus text format.
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// --- Observability: request id / access log / /metrics ---
//
// These three things are done in a single middleware layer instead of being scattered across handlers:
// - request id: generate and write back X-Request-Id (CORS already declared it in Expose-Headers,
// but it was never actually generated before), so logs and responses can be correlated;
// - access log: one line of JSON (method/path/status/ms/bytes/remote/request_id),
// excluding static assets to avoid log spam;
// - metrics: record status code classes, total latency, and in-flight request count into atomic counters for /metrics export.

type requestIDKey struct{}

// requestID gets the request id from the context (returns an empty string if absent).
func requestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// newRequestID generates a 16-character hex request number.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// When the random source is unavailable, fall back to a timestamp: request ids are only used to correlate logs and do not need cryptographic strength.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// sanitizeRequestID only accepts incoming ids with bounded length and a safe character set (to facilitate cross-service tracing).
func sanitizeRequestID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return ""
		}
	}
	return s
}

// statusWriter records the status code and response byte count, and passes through Flush / Unwrap.
//
// Unwrap is required: http.ResponseController and some middleware rely on it to get the underlying
// ResponseWriter (this project does not use ResponseController, but keep it to avoid pitfalls in the future).
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// metrics is lightweight in-process metric counting (not persisted, reset on restart).
type metrics struct {
	requests atomic.Int64
	c2xx     atomic.Int64
	c3xx     atomic.Int64
	c4xx     atomic.Int64
	c5xx     atomic.Int64
	durSumNS atomic.Int64
	durCount atomic.Int64
	inflight atomic.Int64

	// Extended metrics (see metrics.go): check-in / upstream errors / fallback / per-endpoint latency.
	checkinAttempts atomic.Int64
	checkinBlocked  atomic.Int64
	checkinTimeouts atomic.Int64
	fallbackTotal   atomic.Int64
	extra           *extraMetrics
}

func newMetrics() *metrics { return &metrics{extra: newExtraMetrics()} }

// observe records one request (status + latency + endpoint distribution).
func (m *metrics) observe(endpoint string, status int, d time.Duration) {
	m.observeBasic(status, d)
	m.observeDuration(endpoint, d.Seconds())
}

// observeBasic only records the status code and total latency, without per-endpoint / quantile histograms.
//
// /admin/* uses it: if the console's own requests also went into the latency histogram, /admin/api/overview
// The response body changes every time because of "the duration of the previous overview", so the ETag can never hit
// (this is exactly how TestOverviewETag exposed it). The admin API is not a latency SLI.
func (m *metrics) observeBasic(status int, d time.Duration) {
	m.requests.Add(1)
	switch {
	case status >= 500:
		m.c5xx.Add(1)
	case status >= 400:
		m.c4xx.Add(1)
	case status >= 300:
		m.c3xx.Add(1)
	default:
		m.c2xx.Add(1)
	}
	m.durSumNS.Add(int64(d))
	m.durCount.Add(1)
}

// isStaticAsset determines whether a resource is static and needs no access log.
func isStaticAsset(path string) bool {
	switch {
	// favicon / apple-touch-icon are also here: the browser automatically requests them every time a page is opened,
	// if they are not in this whitelist, every console visit gets two extra log lines.
	case path == "/style.css", path == "/ping",
		path == "/favicon.ico", path == "/apple-touch-icon.png":
		return true
	case strings.HasPrefix(path, "/js/"):
		return true
	}
	return false
}

// withObservability installs the request id, access log, and metrics.
func (s *Server) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)

		sw := &statusWriter{ResponseWriter: w}
		m := s.metrics
		m.inflight.Add(1)
		start := time.Now()

		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))

		d := time.Since(start)
		m.inflight.Add(-1)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			m.observeBasic(status, d)
		} else {
			m.observe(endpointLabel(r.URL.Path), status, d)
		}

		if !isStaticAsset(r.URL.Path) {
			logx.Info("access",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"ms", d.Milliseconds(),
				"bytes", sw.bytes,
				"remote", clientIP(r.RemoteAddr),
			)
		}
	})
}

// poolSnapshot is a snapshot of account pool health (the console and /metrics share the same definition).
type poolSnapshot struct {
	Total, Ready, Degraded, Blocked int
	QuotaFresh, QuotaDue            int
}

// poolSnapshot computes account pool health.
//
// The definition is exactly the same as /health (AccountReady / AccountDegraded / quota freshness),
// it was extracted to avoid the console badge and Prometheus metrics reporting contradictory numbers.
func (s *Server) poolSnapshot() poolSnapshot {
	var out poolSnapshot
	now := time.Now()
	for _, a := range s.Store.Accounts() {
		out.Total++
		if deepai.AccountReady(a, now) {
			out.Ready++
		}
		if deepai.AccountDegraded(a, now) {
			out.Degraded++
		}
		if a.Enabled && a.Status == deepai.StatusBlocked {
			out.Blocked++
		}
		if a.Theos != nil && a.Theos.CheckedAt > 0 &&
			now.Sub(time.Unix(a.Theos.CheckedAt, 0)) < quotaStaleAfter {
			out.QuotaFresh++
		}
		if quotaNeedsRefresh(a, now, quotaGuardStaleAfter) {
			out.QuotaDue++
		}
	}
	return out
}

// handleMetrics outputs metrics in Prometheus text format.
//
// Mount withAdminAuth: it contains account pool size and request volume, the same level as /admin/api/*.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	m := s.metrics
	pool := s.poolSnapshot()
	stats := s.Store.Stats()

	var b strings.Builder
	metric := func(name, typ, help string, samples ...[2]string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, s := range samples {
			fmt.Fprintf(&b, "%s %s\n", s[0], s[1])
		}
	}
	counter := func(name, help string, samples ...[2]string) { metric(name, "counter", help, samples...) }
	gauge := func(name, help string, samples ...[2]string) { metric(name, "gauge", help, samples...) }

	counter("gateway_requests_total", "网关处理的请求总数（按状态码分类）",
		[2]string{`gateway_requests_total{class="2xx"}`, itoa64(m.c2xx.Load())},
		[2]string{`gateway_requests_total{class="3xx"}`, itoa64(m.c3xx.Load())},
		[2]string{`gateway_requests_total{class="4xx"}`, itoa64(m.c4xx.Load())},
		[2]string{`gateway_requests_total{class="5xx"}`, itoa64(m.c5xx.Load())},
	)
	// Duration is represented with sum + count: no quantiles are kept, to avoid the overhead of maintaining histograms in-process.
	counter("gateway_request_duration_seconds_sum", "请求耗时总和（秒）",
		[2]string{"gateway_request_duration_seconds_sum", ftoa(float64(m.durSumNS.Load()) / float64(time.Second))})
	counter("gateway_request_duration_seconds_count", "请求耗时样本数",
		[2]string{"gateway_request_duration_seconds_count", itoa64(m.durCount.Load())})
	gauge("gateway_inflight_requests", "当前在途请求数",
		[2]string{"gateway_inflight_requests", itoa64(m.inflight.Load())})
	gauge("gateway_accounts", "号池账号数量（按状态）",
		[2]string{`gateway_accounts{state="total"}`, itoa64(int64(pool.Total))},
		[2]string{`gateway_accounts{state="ready"}`, itoa64(int64(pool.Ready))},
		[2]string{`gateway_accounts{state="degraded"}`, itoa64(int64(pool.Degraded))},
		[2]string{`gateway_accounts{state="blocked"}`, itoa64(int64(pool.Blocked))},
		[2]string{`gateway_accounts{state="quota_fresh"}`, itoa64(int64(pool.QuotaFresh))},
		[2]string{`gateway_accounts{state="quota_due"}`, itoa64(int64(pool.QuotaDue))},
	)
	gauge("gateway_log_requests_total", "请求日志累计条数（stats 口径）",
		[2]string{"gateway_log_requests_total", itoa64(stats.TotalRequests)})
	gauge("gateway_log_failed_total", "失败请求累计条数（stats 口径）",
		[2]string{"gateway_log_failed_total", itoa64(stats.FailedRequests)})
	gauge("gateway_keys", "有效 API 密钥数量",
		[2]string{"gateway_keys", itoa64(int64(len(s.Store.Keys())))})
	gauge("go_goroutines", "当前 goroutine 数",
		[2]string{"go_goroutines", itoa64(int64(runtime.NumGoroutine()))})
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge("go_memstats_heap_alloc_bytes", "堆上已分配字节数",
		[2]string{"go_memstats_heap_alloc_bytes", itoa64(int64(ms.HeapAlloc))})

	// Extended metrics (metrics.go): per-endpoint latency, check-in, fallback, upstream error codes.
	snap := m.snapshot()
	endpoints := make([]string, 0, len(snap.ByEndpoint))
	for k := range snap.ByEndpoint {
		endpoints = append(endpoints, k)
	}
	sort.Strings(endpoints)
	for _, ep := range endpoints {
		appendHistogram(&b, "gateway_request_duration_seconds",
			"HTTP 请求耗时（按端点）", "endpoint", ep, snap.ByEndpoint[ep])
	}
	appendHistogram(&b, "gateway_checkin_seconds", "推理前同步签到的等待耗时", "", "", snap.CheckIn)
	appendHistogram(&b, "gateway_admission_wait_seconds", "账号全忙时的排队等待耗时", "", "", snap.Admission)

	counter("gateway_checkin_total", "推理前的签到尝试（attempt / 慢阻塞 / 超预算）",
		[2]string{`gateway_checkin_total{result="attempt"}`, itoa64(m.checkinAttempts.Load())},
		[2]string{`gateway_checkin_total{result="blocked"}`, itoa64(m.checkinBlocked.Load())},
		[2]string{`gateway_checkin_total{result="timeout"}`, itoa64(m.checkinTimeouts.Load())},
	)
	counter("gateway_fallback_total", "请求体形状被拒后按桌面端形状重试的次数",
		[2]string{`gateway_fallback_total{reason="body_shape"}`, itoa64(m.fallbackTotal.Load())},
	)
	errSamples := make([][2]string, 0, len(snap.UpstreamErrs))
	for _, e := range snap.UpstreamErrs {
		labels := fmt.Sprintf(`code=%q,status="%d",path=%q`, e.Code, e.Status, e.Path)
		errSamples = append(errSamples, [2]string{"gateway_upstream_errors_total{" + labels + "}", itoa64(e.Count)})
	}
	if len(errSamples) > 0 {
		counter("gateway_upstream_errors_total", "上游拒绝次数（按路径 / 错误码 / 状态码）", errSamples...)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}
