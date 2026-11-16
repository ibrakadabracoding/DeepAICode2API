package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHistQuantile pins down bucket quantiles: returns the upper bound of the bucket the value falls in, or 0 when there are insufficient samples.
func TestHistQuantile(t *testing.T) {
	h := newHist([]float64{0.1, 0.5, 1, 5})
	if got := h.quantile(0.5); got != 0 {
		t.Fatalf("空直方图分位 = %v，期望 0", got)
	}
	h.observe(0.05)
	h.observe(0.3)
	h.observe(0.9)
	h.observe(9)
	// 4 samples, the rank of p50 = 2: the 1st falls in the <=0.1 bucket, the 2nd in the <=0.5 bucket,
	// when the cumulative count reaches 2 it hits the 0.5 bucket and returns that bucket's upper bound.
	if got := h.quantile(0.5); got != 0.5 {
		t.Fatalf("p50 = %v，期望 0.5", got)
	}
	// with few samples the rank is truncated: 4*0.95=3.8 → rank 3, falling in the <=1 bucket.
	// This is exactly the intent of "returning the bucket upper bound": better to give a known conservative value than to interpolate and fake precision.
	if got := h.quantile(0.95); got != 1 {
		t.Fatalf("p95 = %v，期望 1", got)
	}
}

// TestMetricsRecordsExtras covers the accumulation semantics of the extra metrics.
func TestMetricsRecordsExtras(t *testing.T) {
	m := newMetrics()
	m.observe("chat", 200, 100*time.Millisecond)
	m.observe("responses", 502, 3*time.Second)
	m.observeUpstreamError("/chat/completions", "session_busy", 409)
	m.observeUpstreamError("/chat/completions", "session_busy", 409)
	m.addFallback()
	m.observeAdmissionWait(250 * time.Millisecond)

	snap := m.snapshot()
	if snap.Fallback != 1 {
		t.Fatalf("fallback = %d，期望 1", snap.Fallback)
	}
	if len(snap.UpstreamErrs) != 1 || snap.UpstreamErrs[0].Code != "session_busy" || snap.UpstreamErrs[0].Count != 2 {
		t.Fatalf("上游错误聚合 = %+v", snap.UpstreamErrs)
	}
	if snap.Latency.Total != 2 {
		t.Fatalf("延迟样本 = %d，期望 2", snap.Latency.Total)
	}
	if _, ok := snap.ByEndpoint["chat"]; !ok {
		t.Fatalf("缺少 chat 端点分布: %+v", snap.ByEndpoint)
	}
	if snap.Admission.Total != 1 {
		t.Fatalf("排队样本 = %d，期望 1", snap.Admission.Total)
	}
}

// TestMetricsEndpointExposition asserts that /metrics exposes the newly added series.
func TestMetricsEndpointExposition(t *testing.T) {
	s, key := newTestServer(t, nil)
	s.metrics.observe("chat", 200, 120*time.Millisecond)
	s.metrics.addFallback()
	s.metrics.observeUpstreamError("/chat/completions", "lane_version_unsupported", 426)
	s.metrics.checkinAttempts.Add(2)
	s.metrics.checkinTimeouts.Add(1)

	rec := do(t, s, http.MethodGet, "/metrics", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, needle := range []string{
		"gateway_request_duration_seconds_bucket{endpoint=\"chat\"",
		"gateway_checkin_seconds_bucket",
		"gateway_admission_wait_seconds_bucket",
		`gateway_checkin_total{result="timeout"} 1`,
		`gateway_fallback_total{reason="body_shape"} 1`,
		`gateway_upstream_errors_total{code="lane_version_unsupported",status="426",path="/chat/completions"} 1`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("/metrics 缺少 %q\n%s", needle, body[:min(len(body), 4000)])
		}
	}
}

// TestOverviewIncludesRuntimeHealth asserts that the console overview includes the runtime health fields.
func TestOverviewIncludesRuntimeHealth(t *testing.T) {
	s, key := newTestServer(t, nil)
	s.metrics.observe("chat", 200, 100*time.Millisecond)
	s.metrics.addFallback()

	rec := do(t, s, http.MethodGet, "/admin/api/overview", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, needle := range []string{`"latency"`, `"checkin"`, `"upstream_errors"`, `"fallback_total"`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("overview 缺少 %s", needle)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
