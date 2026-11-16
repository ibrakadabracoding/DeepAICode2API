package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// --- Console read endpoints (overview / paged accounts / log queries) ---
//
// Motivation for the split: /admin/api/state delivered every account at once
// (603 accounts, about 429KB), while the console renders only 20 rows per screen.
// Here we split it into two endpoints - "rarely changing metadata" and "pageable
// account list" - both with ETags, so idle refreshes degrade to 304.

// accountView is the projection of an account sent to the console (the full key and
// machine fingerprint are never exposed).
func accountView(a *deepai.Account) map[string]any {
	// Quota freshness: the console does not have to compute the delta between
	// checked_at and the current time, otherwise every frontend would have to
	// duplicate the "how old counts as stale" rule.
	age := int64(0)
	if a.Theos != nil && a.Theos.CheckedAt > 0 {
		if d := time.Now().Unix() - a.Theos.CheckedAt; d > 0 {
			age = d
		}
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "source": a.Source,
		"key": deepai.MaskSecret(a.APIKey), "key_id": a.KeyID(),
		"username": a.Username, "email": a.Email, "is_pro": a.IsPro,
		"plan": a.Plan, "email_verified": a.EmailVerified,
		"user_pk": a.UserPK, "group": a.GroupName(),
		"proxy": deepai.MaskProxy(a.Proxy), "fingerprint": a.Fingerprint,
		// Only a digest of the machine fingerprint is sent: the upstream grants quota
		// based on it, so the full value does not need to go over the wire.
		"machine": maskMachine(a.MachineID), "has_machine": a.MachineID != "",
		"registered_at": a.RegisteredAt, "last_verified": a.LastVerified,
		"enabled": a.Enabled, "status": a.Status, "note": a.Note,
		"last_err": a.LastErr, "failures": a.Failures, "last_used": a.LastUsed,
		"theos": a.Theos,
		// quota_age is the age of the quota snapshot in seconds; quota_due means an
		// on-demand re-probe will refresh it.
		"quota_age": age,
		"quota_due": quotaNeedsRefresh(a, time.Now(), quotaStaleAfter),
	}
}

// handleAdminOverview returns metadata that does not grow with the account count.
//
// Includes: stats, keys, model catalog, settings, machine fingerprint, upstream
// address. For the account list use /admin/api/accounts (supports paging and filtering).
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	settings := s.Store.Settings()
	total, ready := s.Store.AccountCounts()
	snap := s.metrics.snapshot()
	byEndpoint := make(map[string]map[string]float64, len(snap.ByEndpoint))
	for ep, hist := range snap.ByEndpoint {
		byEndpoint[ep] = latencySummary(hist)
	}
	checkinStats := map[string]any{
		"attempts": snap.CheckInStats["attempts"],
		"blocked":  snap.CheckInStats["blocked"],
		"timeouts": snap.CheckInStats["timeouts"],
		"p50_ms":   quantileMs(snap.CheckIn, 0.50),
		"p95_ms":   quantileMs(snap.CheckIn, 0.95),
	}
	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"accounts":       total,
		"ready":          ready,
		"keys":           safeKeyList(s.Store.Keys()),
		"models":         registry.List(s.catalog(), settings.ModelAliases),
		"default_model":  s.Store.DefaultModel(),
		"models_updated": s.Store.ModelsUpdated(),
		"machine_id":     shortMachine(s.Store.MachineID()),
		"settings":       sanitizedSettings(settings),
		"stats":          s.Store.Stats(),
		"upstream":       s.Up.Origin,
		// Runtime health: latency percentiles, check-in, fallback, and upstream
		// error codes (metrics.go).
		"latency":             latencySummary(snap.Latency),
		"latency_by_endpoint": byEndpoint,
		"checkin":             checkinStats,
		"fallback_total":      snap.Fallback,
		"upstream_errors":     snap.UpstreamErrs,
	})
}

// handleAdminAccountsPage returns a page of accounts.
//
// Query parameters: offset / limit / group / status / q.
// Response: {items, total, offset, limit}, with an ETag.
func (s *Server) handleAdminAccountsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))

	sortKey := strings.TrimSpace(q.Get("sort"))
	desc := strings.EqualFold(q.Get("order"), "desc")

	page := s.Store.AccountPage(store.AccountQuery{
		Offset: offset, Limit: limit,
		Group: q.Get("group"), Status: q.Get("status"), Query: q.Get("q"),
		Sort: sortKey, Desc: desc,
	})
	items := make([]map[string]any, 0, len(page.Items))
	for _, a := range page.Items {
		items = append(items, accountView(a))
	}
	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"items":  items,
		"total":  page.Total,
		"offset": offset,
		"limit":  limit,
		"sort":   sortKey,
		"order":  map[bool]string{true: "desc", false: "asc"}[desc],
	})
}

// logFilterFromQuery parses query parameters into a store.QueryFilter.
//
// Supports: limit / since / endpoint / account / min_status / q.
func logFilterFromQuery(r *http.Request) store.QueryFilter {
	q := r.URL.Query()
	f := store.QueryFilter{
		Endpoint: strings.TrimSpace(q.Get("endpoint")),
		Account:  strings.TrimSpace(q.Get("account")),
		Keyword:  strings.TrimSpace(q.Get("q")),
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.MinStatus, _ = strconv.Atoi(q.Get("min_status"))
	if v, _ := strconv.ParseInt(q.Get("since"), 10, 64); v > 0 {
		f.Since = v
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	return f
}

// --- ETag support ---

// writeJSONCached writes JSON with an ETag; returns 304 when If-None-Match matches.
func writeJSONCached(w http.ResponseWriter, r *http.Request, code int, v any) {
	raw := jsonEvent(v)
	if raw == nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "响应序列化失败", "server_error")
		return
	}
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	httpx.WriteRawJSON(w, code, raw)
}

// etagMatches reports whether If-None-Match contains the target ETag (supports
// comma-separated values and the weak validator prefix).
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == want {
			return true
		}
	}
	return false
}
