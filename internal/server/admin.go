package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// --- Console admin endpoints ---

// sanitizedSettings returns the settings that may be sent to the frontend (the admin password never leaks).
func sanitizedSettings(st store.Settings) map[string]any {
	return map[string]any{
		"model_aliases":        st.ModelAliases,
		"request_timeout_sec":  st.RequestTimeout,
		"max_retries":          st.MaxRetries,
		"log_keep":             st.LogKeep,
		"cors_origin":          st.CORSOrigin,
		"fallback_model":       st.FallbackModel,
		"strict_model":         st.StrictModel,
		"lane":                 st.Lane,
		"rate_limit_rps":       st.RateLimitRPS,
		"rate_limit_burst":     st.RateLimitBurst,
		"key_concurrency":      st.KeyConcurrency,
		"checkin_block_ms":     st.CheckinBlockMS,
		"preset_desktop_shape": st.PresetDesktopShape,
		"admin_password_set":   st.AdminPassword != "",
	}
}

// --- Console API ---

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	// Compatibility endpoint: it still returns all accounts by default (external scripts
	// may rely on it). The console now uses /admin/api/overview + /admin/api/accounts
	// (paginated); add ?accounts=0 when a smaller response is needed.
	var safe []map[string]any
	if r.URL.Query().Get("accounts") != "0" {
		accts := s.Store.Accounts()
		safe = make([]map[string]any, 0, len(accts))
		for _, a := range accts {
			safe = append(safe, accountView(a))
		}
	}
	safeKeys := safeKeyList(s.Store.Keys())
	catalog := s.catalog()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"accounts":       safe,
		"keys":           safeKeys,
		"models":         registry.List(catalog, s.Store.Settings().ModelAliases),
		"default_model":  s.Store.DefaultModel(),
		"models_updated": s.Store.ModelsUpdated(),
		"machine_id":     shortMachine(s.Store.MachineID()),
		"settings":       sanitizedSettings(s.Store.Settings()),
		"stats":          s.Store.Stats(),
		"upstream":       s.Up.Origin,
	})
}

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": safeKeyList(s.Store.Keys())})
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
			Name   string `json:"name"`
			Group  string `json:"group"`
			Key    string `json:"key"`
		}
		// Malformed JSON is rejected outright; an empty body is still valid
		// (equivalent to "create a key with the defaults").
		if !httpx.DecodeOptionalJSON(w, r, &body) {
			return
		}
		// action=set-group: only changes the bound group (the key value is unchanged);
		// anything else / empty still creates a new key.
		if body.Action == "set-group" {
			key := s.resolveKeyRef(strings.TrimSpace(body.Key))
			if key == "" {
				httpx.WriteErr(w, http.StatusBadRequest, "缺少 key", "invalid_request_error")
				return
			}
			ok := s.Store.SetKeyGroup(key, body.Group)
			if !s.persistOrFail(w) {
				return
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": ok, "group": s.Store.KeyGroup(key)})
			return
		}
		k := s.Store.AddKey(body.Name, body.Group)
		if !s.persistOrFail(w) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k.Key})
	case http.MethodDelete:
		q := r.URL.Query()
		delRef := strings.TrimSpace(q.Get("key"))
		togRef := strings.TrimSpace(q.Get("toggle"))
		if delRef == "" && togRef == "" {
			httpx.WriteErr(w, http.StatusBadRequest, "缺少 key 或 toggle 参数", "invalid_request_error")
			return
		}
		// Validate before mutating: resolveKeyRef returns its input unchanged when it
		// finds nothing, so existence has to be checked here. Otherwise "delete a
		// nonexistent key" returns 200 {"ok":true} and even persists to disk while the
		// console shows success and the list stays the same (typically because another
		// tab already deleted that key). Both refs are validated for the same reason:
		// we must not delete successfully, fail the toggle, and then return 404 halfway
		// through.
		delKey, togKey := s.resolveKeyRef(delRef), s.resolveKeyRef(togRef)
		if delRef != "" && !s.keyExists(delKey) {
			httpx.WriteErr(w, http.StatusNotFound, "密钥不存在（可能已被删除或吊销）", "not_found_error")
			return
		}
		if togRef != "" && !s.keyExists(togKey) {
			httpx.WriteErr(w, http.StatusNotFound, "密钥不存在（可能已被删除或吊销）", "not_found_error")
			return
		}
		deleted, toggled := false, false
		if delRef != "" {
			deleted = s.Store.DeleteKey(delKey)
		}
		if togRef != "" {
			toggled = s.Store.ToggleKey(togKey)
		}
		if !s.persistOrFail(w) {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "toggled": toggled})
	default:
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "不支持的方法", "invalid_request_error")
	}
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		s.Store.ClearLogs()
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case http.MethodGet:
	default:
		// Only DELETE used to be rejected; POST / PUT returned logs as a query result,
		// easily mistaken for a successful write.
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET / DELETE", "invalid_request_error")
		return
	}
	// Supports limit / since / endpoint / account / min_status / q.
	// With no parameters, behavior matches the old version (latest limit entries, default 200).
	f := logFilterFromQuery(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"logs":  s.Store.QueryLogs(f),
		"limit": f.Limit,
		"filter": map[string]any{
			"since": f.Since, "endpoint": f.Endpoint, "account": f.Account,
			"min_status": f.MinStatus, "q": f.Keyword,
		},
	})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.Store.ClearStats()
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.Store.Stats())
}

// settingsPatch is the settings delta submitted by the console.
//
// Every field is a pointer: nil means "leave unchanged", which is distinct from
// "explicitly set to 0 / empty string". This matters especially for the rate limit
// parameters, where 0 is a legitimate value meaning "rate limiting off".
type settingsPatch struct {
	ModelAliases       map[string]string `json:"model_aliases"`
	RequestTimeout     *int              `json:"request_timeout_sec"`
	MaxRetries         *int              `json:"max_retries"`
	LogKeep            *int              `json:"log_keep"`
	CORSOrigin         *string           `json:"cors_origin"`
	AdminPassword      *string           `json:"admin_password"`
	FallbackModel      *string           `json:"fallback_model"`
	StrictModel        *bool             `json:"strict_model"`
	RateLimitRPS       *float64          `json:"rate_limit_rps"`
	RateLimitBurst     *int              `json:"rate_limit_burst"`
	KeyConcurrency     *int              `json:"key_concurrency"`
	CheckinBlockMS     *int              `json:"checkin_block_ms"`
	PresetDesktopShape *bool             `json:"preset_desktop_shape"`
}

// apply merges the delta into the settings (the caller holds the write lock).
func (p settingsPatch) apply(st *store.Settings) {
	if p.ModelAliases != nil {
		st.ModelAliases = p.ModelAliases
	}
	if p.RequestTimeout != nil && *p.RequestTimeout > 0 {
		st.RequestTimeout = *p.RequestTimeout
	}
	if p.MaxRetries != nil && *p.MaxRetries > 0 {
		st.MaxRetries = *p.MaxRetries
	}
	if p.LogKeep != nil && *p.LogKeep > 0 {
		st.LogKeep = *p.LogKeep
	}
	if p.CORSOrigin != nil {
		st.CORSOrigin = *p.CORSOrigin
	}
	if p.AdminPassword != nil {
		st.AdminPassword = strings.TrimSpace(*p.AdminPassword)
	}
	if p.FallbackModel != nil {
		st.FallbackModel = strings.TrimSpace(*p.FallbackModel)
	}
	if p.StrictModel != nil {
		st.StrictModel = *p.StrictModel
	}
	// Rate limit parameters may be explicitly set to 0 (rate limiting off), so check
	// for nil rather than > 0.
	if p.RateLimitRPS != nil && *p.RateLimitRPS >= 0 {
		st.RateLimitRPS = *p.RateLimitRPS
	}
	if p.RateLimitBurst != nil && *p.RateLimitBurst >= 0 {
		st.RateLimitBurst = *p.RateLimitBurst
	}
	if p.KeyConcurrency != nil && *p.KeyConcurrency >= 0 {
		st.KeyConcurrency = *p.KeyConcurrency
	}
	// The check-in budget may be explicitly set to 0 (= default), so check for nil
	// rather than > 0.
	if p.CheckinBlockMS != nil && *p.CheckinBlockMS >= 0 {
		st.CheckinBlockMS = *p.CheckinBlockMS
	}
	if p.PresetDesktopShape != nil {
		st.PresetDesktopShape = *p.PresetDesktopShape
	}
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteJSON(w, http.StatusOK, sanitizedSettings(s.Store.Settings()))
		return
	}
	var body settingsPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	s.Store.UpdateSettings(func(st *store.Settings) { body.apply(st) })
	if !s.persistOrFail(w) {
		return
	}
	// Budget-type settings must take effect immediately for already-cached clients,
	// without waiting for a restart.
	s.applyRuntimeSettings()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": sanitizedSettings(s.Store.Settings())})
}

// handleAdminModels refreshes the model catalog; GET returns the cache.
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Use s.catalog() / s.defaultModelID() rather than the raw Store values: when the
		// catalog is empty (fresh install with the upstream unreachable) they supply a
		// fallback model, keeping it consistent with /v1/models. Otherwise the console's
		// model page would be blank while the API still works with the default model.
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"models":         adminModelViews(registry.List(s.catalog(), s.Store.Settings().ModelAliases)),
			"default_model":  s.defaultModelID(),
			"models_updated": s.Store.ModelsUpdated(),
		})
		return
	}
	models, err := s.RefreshModels(r.Context())
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "刷新模型目录失败: "+err.Error(), "upstream_error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(models), "default_model": s.Store.DefaultModel(),
	})
}

// safeKeyList converts the key list into the view sent to the console (without full keys).
func safeKeyList(keys []*store.APIKey) []map[string]any {
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"key": mask(k.Key), "name": k.Name, "group": k.Group, "enabled": k.Enabled,
			"created_at": k.CreatedAt, "last_used": k.LastUsed, "requests": k.Requests,
		})
	}
	return out
}

// mask redacts a sensitive string, keeping only the leading and trailing fragments.
func mask(s string) string {
	if len(s) <= 10 {
		if s == "" {
			return ""
		}
		return s[:1] + "***"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// shortMachine shows only the first 12 characters of the machine fingerprint.
func shortMachine(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// maskMachine returns a readable summary of a machine fingerprint.
//
// The account-level machine is a raw UUID; the full value never reaches the console
// (it determines how the upstream issues quota).
func maskMachine(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) <= 8 {
		return raw
	}
	return raw[:8] + "…"
}
