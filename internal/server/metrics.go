package server

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
)

// This file is the **extended** metrics layer for /metrics and /admin/api/overview.
//
// Design trade-offs:
// - Zero dependencies: maintain fixed-bucket histograms ourselves, without pulling in a Prometheus client;
// - Low cardinality: endpoints are only split into chat / responses / messages / other, and upstream errors are aggregated by
// (path, code, status) with a cap on entries, to avoid memory blowup from abnormal traffic;
// - Coexist with legacy metrics: the original requests/2xx..5xx/durSum are kept, so the console does not break.

// Latency buckets (seconds). Covers the hundreds-of-milliseconds range of free models up to the minutes-long range of deep thinking.
var defaultBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600}

// hist is a fixed-bucket histogram: it only keeps per-bucket counts + sum/count, which is enough to compute p50/p95/p99.
type hist struct {
	bounds []float64
	counts []uint64 // len(bounds)+1, the last bucket is +Inf
	sum    float64
	total  uint64
}

func newHist(bounds []float64) *hist {
	return &hist{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *hist) observe(v float64) {
	idx := sort.SearchFloat64s(h.bounds, v)
	h.counts[idx]++
	h.sum += v
	h.total++
}

// quantile returns the upper bound of the bucket containing the q quantile (q in 0~1).
//
// Returning the bucket upper bound instead of linear interpolation: callers want "p95 falls in the 2s bucket", whereas interpolation would
// give a precise-looking illusion unsupported by samples.
func (h *hist) quantile(q float64) float64 {
	if h == nil || h.total == 0 {
		return 0
	}
	if q <= 0 {
		return 0
	}
	if q > 1 {
		q = 1
	}
	target := uint64(float64(h.total) * q)
	if target == 0 {
		target = 1
	}
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= target {
			if i < len(h.bounds) {
				return h.bounds[i]
			}
			return h.bounds[len(h.bounds)-1]
		}
	}
	return h.bounds[len(h.bounds)-1]
}

// histogramView is the JSON / Prometheus view of a histogram.
type histogramView struct {
	Bounds []float64 `json:"bounds"`
	Counts []uint64  `json:"counts"`
	Sum    float64   `json:"sum"`
	Total  uint64    `json:"total"`
}

func (h *hist) view() histogramView {
	if h == nil {
		return histogramView{}
	}
	bounds := append([]float64(nil), h.bounds...)
	counts := append([]uint64(nil), h.counts...)
	return histogramView{Bounds: bounds, Counts: counts, Sum: h.sum, Total: h.total}
}

// endpointLabel collapses URL paths into low-cardinality labels.
func endpointLabel(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "chat"
	case "/v1/responses":
		return "responses"
	case "/v1/messages":
		return "messages"
	}
	return "other"
}

// extraMetrics holds the extended metrics (the part that needs a mutex).
type extraMetrics struct {
	mu sync.Mutex
	// endpointDur buckets by endpoint, to answer "which endpoint is getting slower".
	endpointDur map[string]*hist
	// upstreamErr aggregates upstream errors by (path, code, status).
	upstreamErr map[string]int64
	// upstreamErrCap limits the number of entries; once overflowed, everything is recorded under the overflow key.
	upstreamErrCap int
	// checkinDur is the latency distribution of synchronous check-in waits.
	checkinDur *hist
	// admissionDur is the queue-wait distribution when all accounts are busy.
	admissionDur *hist
	// durHist is the latency distribution of all requests (consistent with the sum of endpointDur).
	durHist *hist
}

func newExtraMetrics() *extraMetrics {
	return &extraMetrics{
		endpointDur:    map[string]*hist{},
		upstreamErr:    map[string]int64{},
		upstreamErrCap: 200,
		checkinDur:     newHist(defaultBuckets),
		admissionDur:   newHist(defaultBuckets),
		durHist:        newHist(defaultBuckets),
	}
}

// observeDuration records the duration of one request (seconds).
func (m *metrics) observeDuration(endpoint string, sec float64) {
	x := m.extra
	x.mu.Lock()
	defer x.mu.Unlock()
	x.durHist.observe(sec)
	h := x.endpointDur[endpoint]
	if h == nil {
		h = newHist(defaultBuckets)
		x.endpointDur[endpoint] = h
	}
	h.observe(sec)
}

// observeUpstreamError records one upstream rejection (including the error code).
func (m *metrics) observeUpstreamError(path, code string, status int) {
	if code == "" {
		code = "unknown"
	}
	key := path + "\x00" + code + "\x00" + itoa64(int64(status))
	x := m.extra
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.upstreamErr[key]; !ok && len(x.upstreamErr) >= x.upstreamErrCap {
		key = "overflow\x00overflow\x000"
	}
	x.upstreamErr[key]++
}

// observeCheckIn records one check-in attempt before inference.
func (m *metrics) observeCheckIn(outcome deepai.CheckInOutcome) {
	if !outcome.Attempted {
		return
	}
	m.checkinAttempts.Add(1)
	if outcome.Waited >= checkInSlowWarn {
		m.checkinBlocked.Add(1)
	}
	if outcome.TimedOut {
		m.checkinTimeouts.Add(1)
	}
	sec := outcome.Waited.Seconds()
	x := m.extra
	x.mu.Lock()
	x.checkinDur.observe(sec)
	x.mu.Unlock()
}

// observeAdmissionWait records one "all accounts busy" queue wait (seconds).
func (m *metrics) observeAdmissionWait(d time.Duration) {
	if d <= 0 {
		return
	}
	x := m.extra
	x.mu.Lock()
	x.admissionDur.observe(d.Seconds())
	x.mu.Unlock()
}

// addFallback records one "request body shape rejected → patch shape and retry".
func (m *metrics) addFallback() { m.fallbackTotal.Add(1) }

// metricsSnapshot is the aggregated view for the console overview.
type metricsSnapshot struct {
	Latency      histogramView            `json:"latency"`
	ByEndpoint   map[string]histogramView `json:"by_endpoint"`
	CheckIn      histogramView            `json:"checkin"`
	Admission    histogramView            `json:"admission"`
	UpstreamErrs []upstreamErrView        `json:"upstream_errors"`
	CheckInStats map[string]int64         `json:"checkin_stats"`
	Fallback     int64                    `json:"fallback_total"`
}

type upstreamErrView struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Status int    `json:"status"`
	Count  int64  `json:"count"`
}

// snapshot takes a consistent snapshot (sorted by count, at most 8 upstream errors).
func (m *metrics) snapshot() metricsSnapshot {
	x := m.extra
	x.mu.Lock()
	byEndpoint := make(map[string]histogramView, len(x.endpointDur))
	for k, h := range x.endpointDur {
		byEndpoint[k] = h.view()
	}
	errs := make([]upstreamErrView, 0, len(x.upstreamErr))
	for k, n := range x.upstreamErr {
		path, code, status := splitErrKey(k)
		errs = append(errs, upstreamErrView{Path: path, Code: code, Status: status, Count: n})
	}
	latency := x.durHist.view()
	checkin := x.checkinDur.view()
	admission := x.admissionDur.view()
	x.mu.Unlock()

	sort.Slice(errs, func(i, j int) bool { return errs[i].Count > errs[j].Count })
	if len(errs) > 8 {
		errs = errs[:8]
	}
	return metricsSnapshot{
		Latency:      latency,
		ByEndpoint:   byEndpoint,
		CheckIn:      checkin,
		Admission:    admission,
		UpstreamErrs: errs,
		CheckInStats: map[string]int64{
			"attempts": m.checkinAttempts.Load(),
			"blocked":  m.checkinBlocked.Load(),
			"timeouts": m.checkinTimeouts.Load(),
		},
		Fallback: m.fallbackTotal.Load(),
	}
}

func splitErrKey(key string) (string, string, int) {
	parts := splitNUL(key)
	if len(parts) < 2 {
		return key, "", 0
	}
	status := 0
	for _, r := range parts[2] {
		if r < '0' || r > '9' {
			status = 0
			break
		}
		status = status*10 + int(r-'0')
	}
	return parts[0], parts[1], status
}

// splitNUL avoids introducing the extra semantics of strings.Split on the hot path (here it only splits on \x00).
func splitNUL(s string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// quantileMs returns the millisecond quantile (0 means no samples).
func quantileMs(view histogramView, q float64) float64 {
	h := &hist{bounds: view.Bounds, counts: view.Counts, sum: view.Sum, total: view.Total}
	return h.quantile(q) * 1000
}

// appendHistogram outputs a Prometheus histogram (including the +Inf bucket and sum/count).
//
// An empty labelName means no label; otherwise output as labelName="labelValue".
// The output order is fixed (buckets ascending) to make diffing and scrape-side stability checks easier.
func appendHistogram(b *strings.Builder, name, help, labelName, labelValue string, v histogramView) {
	label := ""
	if labelName != "" {
		label = labelName + "=" + strconv.Quote(labelValue) + ","
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	var cum uint64
	for i, bound := range v.Bounds {
		cum += v.Counts[i]
		le := strconv.Quote(strconv.FormatFloat(bound, 'f', -1, 64))
		fmt.Fprintf(b, "%s_bucket{%sle=%s} %d\n", name, label, le, cum)
	}
	cum += v.Counts[len(v.Bounds)]
	fmt.Fprintf(b, "%s_bucket{%sle=\"+Inf\"} %d\n", name, label, cum)
	fmt.Fprintf(b, "%s_sum %s\n", name, ftoa(v.Sum))
	fmt.Fprintf(b, "%s_count %d\n", name, v.Total)
}

// latencySummary reduces the histogram to the three quantiles the console needs (milliseconds).
func latencySummary(v histogramView) map[string]float64 {
	return map[string]float64{
		"p50_ms": quantileMs(v, 0.50),
		"p95_ms": quantileMs(v, 0.95),
		"p99_ms": quantileMs(v, 0.99),
	}
}
