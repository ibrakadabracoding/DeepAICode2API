package server

// Each of the 10 account management actions has its own handler function.
//
// Before the refactor, it was a 255-line switch sharing a 30-field "universal request body":
// `action=remove` only needs id, yet shares the same struct with `action=enroll`,
// making required fields and optionality completely invisible.
//
// Now it parses in two steps:
// 1. First decode only `action` (adminActionEnvelope);
// 2. Then decode its own minimal struct based on the action.
//
// The benefit is that each action's input is clearly expressed in the signature; adding a new action only requires adding one function
// and one table entry, without making any function longer.
//
// The only behavioral difference: when an action's request body contains fields used only by **another** action,
// that are illegal, it used to fail decoding entirely; now they are ignored. This only makes the interface more lenient.

import (
	"encoding/json"
	"net/http"
	"strings"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/proxy"
)

// adminActionEnvelope carries only the action itself, used for the first parse.
type adminActionEnvelope struct {
	Action string `json:"action"`
}

// adminActionFunc handles one action: it writes the complete response itself (including errors).
type adminActionFunc func(s *Server, w http.ResponseWriter, raw []byte)

// adminActions is the action → handler function table.
//
// When adding a new action: add a constant (admin_accounts.go), add a function, add a table entry;
// don't forget its own minimal request struct.
var adminActions = map[string]adminActionFunc{
	actionImport:        (*Server).adminImport,
	actionManual:        (*Server).adminManual,
	actionEnroll:        (*Server).adminEnroll,
	actionRemove:        (*Server).adminRemove,
	actionToggle:        (*Server).adminToggle,
	actionClearErrors:   (*Server).adminClearErrors,
	actionAssignMachine: (*Server).adminAssignMachine,
	actionAssignProxy:   (*Server).adminAssignProxy,
	actionMarkVerified:  (*Server).adminMarkVerified,
	actionSetGroup:      (*Server).adminSetGroup,
}

// decodeAction decodes the raw request body into an action's own struct.
func decodeAction[T any](w http.ResponseWriter, raw []byte) (T, bool) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		var zero T
		httpx.WriteErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return zero, false
	}
	return v, true
}

// --- Request structs for each action (containing only the fields actually used by that action) ---

type adminImportReq struct {
	Dir string `json:"dir"`
}

type adminManualReq struct {
	APIKey string `json:"api_key"`
	Name   string `json:"name"`
	Group  string `json:"group"`
}

// adminEnrollReq is the pool enrollment request: the registration flow provides the full identity + fingerprint + pre-probe results.
type adminEnrollReq struct {
	Email          string `json:"email"`
	Password       string `json:"password"`
	Username       string `json:"username"`
	APIKey         string `json:"api_key"`
	SessionID      string `json:"session_id"`
	CSRFToken      string `json:"csrftoken"`
	UserPK         int64  `json:"user_pk"`
	Plan           string `json:"plan"`
	EmailVerified  bool   `json:"email_verified"`
	Group          string `json:"group"`
	Proxy          string `json:"proxy"`
	Fingerprint    string `json:"fingerprint"`
	MachineID      string `json:"machine_id"`
	UserAgent      string `json:"user_agent"`
	AcceptLanguage string `json:"accept_language"`
	SecCHUA        string `json:"sec_chua"`
	Platform       string `json:"platform"`

	// SkipProbe lets the caller skip the /theos probe during pool enrollment: the registration flow just verified liveness and includes it with the request
	// Including theos_balance / theos_granted makes another probe both slow (~2s) and introduces a new failure point.
	SkipProbe    bool   `json:"skip_probe"`
	TheosBalance string `json:"theos_balance"`
	TheosGranted string `json:"theos_granted"`
}

// adminIDReq is the common shape for "operating on a single account by id".
type adminIDReq struct {
	ID string `json:"id"`
}

type adminAssignProxyReq struct {
	ID    string `json:"id"`
	Group string `json:"group"`
	// When Reassign is true, overwrite the existing proxy; when Clear is true, clear it (for rollback).
	Reassign bool `json:"reassign"`
	Clear    bool `json:"clear"`
}

type adminMarkVerifiedReq struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type adminSetGroupReq struct {
	ID        string `json:"id"`
	Group     string `json:"group"`
	FromGroup string `json:"from_group"`
}

// --- action implementations ---

// adminImport imports credentials from the local desktop client.
func (s *Server) adminImport(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminImportReq](w, raw)
	if !ok {
		return
	}
	results, err := deepai.ImportAllFromDesktop(body.Dir)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "导入失败: "+err.Error(), "invalid_request_error")
		return
	}
	imported := make([]map[string]any, 0, len(results))
	for _, res := range results {
		isNew := s.Store.UpsertAccount(res.Account)
		imported = append(imported, map[string]any{
			"id": res.Account.ID, "name": res.Account.Name,
			"username": res.Account.Username, "path": res.Path, "new": isNew,
		})
	}
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(imported), "imported": imported})
}

// adminManual creates an account from a manually pasted API Key.
func (s *Server) adminManual(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminManualReq](w, raw)
	if !ok {
		return
	}
	acct, err := deepai.AccountFromAPIKey(body.APIKey, body.Name, "manual", false)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.Group) != "" {
		acct.Group = strings.TrimSpace(body.Group)
	}
	isNew := s.Store.UpsertAccount(acct)
	if !s.persistOrFail(w) {
		return
	}
	s.probeAndSaveAsync(acct)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "new": isNew, "id": acct.ID})
}

// adminEnroll writes a verified-live account into the account pool (the only write entry point).
func (s *Server) adminEnroll(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminEnrollReq](w, raw)
	if !ok {
		return
	}
	reg := deepai.Registration{
		Email: body.Email, Password: body.Password, Username: body.Username,
		APIKey: body.APIKey, SessionID: body.SessionID, CSRFToken: body.CSRFToken,
		UserPK: body.UserPK, Plan: body.Plan, EmailVerified: body.EmailVerified,
		Group: body.Group, Proxy: body.Proxy, Fingerprint: body.Fingerprint,
		MachineID: strings.TrimSpace(body.MachineID),
		UserAgent: strings.TrimSpace(body.UserAgent), AcceptLanguage: strings.TrimSpace(body.AcceptLanguage),
		SecCHUA: strings.TrimSpace(body.SecCHUA), Platform: strings.TrimSpace(body.Platform),
		Source: "register",
	}
	// Each account must have its own machine fingerprint; otherwise the upstream's daily free quota will be mutually exclusive with other accounts.
	if reg.MachineID == "" {
		reg.MachineID = deepai.NewMachineUUID()
	}
	acct, err := deepai.AccountFromRegistration(reg)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	// If the caller has already verified liveness and passes the result, trust it directly; otherwise probe it yourself (manual console enrollment takes this path).
	var al *deepai.Allowance
	balance, granted := strings.TrimSpace(body.TheosBalance), strings.TrimSpace(body.TheosGranted)
	if body.SkipProbe && balance != "" {
		al = allowanceFromEnroll(balance, granted, "")
	} else {
		probed, err := s.probeAccountQuota(acct)
		if err != nil {
			if deepai.IsSocialLoginRequired(err) {
				httpx.WriteErr(w, http.StatusBadRequest, "上游已限制为 Google/Apple 登录（social_login_required），该 Key 无法推理", "social_login_required")
				return
			}
			httpx.WriteErr(w, http.StatusBadRequest, "Key 验活失败: "+err.Error(), "invalid_request_error")
			return
		}
		al = probed
		if balance != "" {
			al.Balance = balance
		}
		if granted != "" {
			al.Granted = granted
		}
	}

	id, isNew, err := s.enrollRegistration(reg, al)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "入池失败: "+err.Error(), "server_error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "new": isNew,
		"email_verified": body.EmailVerified, "theos": al,
	})
}

func (s *Server) adminRemove(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminIDReq](w, raw)
	if !ok {
		return
	}
	removed := s.Store.RemoveAccount(body.ID)
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": removed})
}

func (s *Server) adminToggle(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminIDReq](w, raw)
	if !ok {
		return
	}
	s.Store.UpdateAccount(body.ID, func(a *deepai.Account) { a.Enabled = !a.Enabled })
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminClearErrors(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminIDReq](w, raw)
	if !ok {
		return
	}
	s.Store.UpdateAccount(body.ID, func(a *deepai.Account) {
		a.Failures = 0
		a.Status = "unknown"
		a.LastErr = ""
	})
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminAssignMachine backfills an independent machine fingerprint for existing accounts.
//
// Only fill empty values, never overwrite an existing fingerprint (overwriting invalidates the quota attribution the upstream has already remembered for that account).
func (s *Server) adminAssignMachine(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminIDReq](w, raw)
	if !ok {
		return
	}
	var targets []*deepai.Account
	assigned := s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		if body.ID != "" && a.ID != body.ID {
			return false
		}
		if strings.TrimSpace(a.MachineID) != "" {
			return false
		}
		a.MachineID = deepai.NewMachineUUID()
		targets = append(targets, a.Clone())
		return true
	})
	if !s.persistOrFail(w) {
		return
	}
	// Re-probe the quota so scheduling can immediately see the quota under the new fingerprint.
	// Goes through the registered batch entry point: on process shutdown, wait for this persist-to-disk to finish.
	s.probeBatchAsync(targets)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "assigned": assigned})
}

// adminClearProxy is the clear branch of assign-proxy: sets the proxy of matching accounts to empty (for rollback).
func (s *Server) adminClearProxy(w http.ResponseWriter, match func(*deepai.Account) bool) {
	cleared := s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		if !match(a) || strings.TrimSpace(a.Proxy) == "" {
			return false
		}
		a.Proxy = ""
		return true
	})
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": cleared})
}

// adminAssignProxy assigns proxies from the registered proxy pool (PROXY_POOL / PROXY_POOL_FILE) to account pool accounts in rotation.
//
// By default only fills empty values; reassign=true overwrites, clear=true clears (rollback).
func (s *Server) adminAssignProxy(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminAssignProxyReq](w, raw)
	if !ok {
		return
	}
	id := strings.TrimSpace(body.ID)
	group := strings.TrimSpace(body.Group)
	match := func(a *deepai.Account) bool {
		if id != "" {
			return a.ID == id
		}
		if group != "" {
			return strings.EqualFold(a.GroupName(), group)
		}
		return true
	}

	if body.Clear {
		s.adminClearProxy(w, match)
		return
	}

	pool, err := loadProxyPool()
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "代理池配置错误: "+err.Error(), "invalid_request_error")
		return
	}
	if pool == nil || pool.Len() == 0 {
		httpx.WriteErr(w, http.StatusBadRequest, "未配置代理池（PROXY_POOL / DEEPAI_REGISTER_PROXY_POOL / PROXY_POOL_FILE）", "invalid_request_error")
		return
	}

	dist := map[string]int{}
	assigned := s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		if !match(a) {
			return false
		}
		if !body.Reassign && strings.TrimSpace(a.Proxy) != "" {
			return false
		}
		p := pool.Next()
		if p == "" {
			return false
		}
		a.Proxy = p
		dist[proxy.ProxyLabel(p)]++
		return true
	})
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "assigned": assigned, "pool_size": pool.Len(), "proxies": dist,
	})
}

// adminMarkVerified backfills email_verified from the background verification pipeline (it may still be false when entering the pool).
func (s *Server) adminMarkVerified(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminMarkVerifiedReq](w, raw)
	if !ok {
		return
	}
	if err := s.markAccountVerified(body.ID, body.Email); err != nil {
		httpx.WriteErr(w, http.StatusNotFound, err.Error(), "invalid_request_error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminSetGroup moves an account to another group (e.g. merging a stress-test batch into the daily account pool).
//
// When id is empty, migrate in bulk by from_group, making it easy to move a whole batch back into place.
func (s *Server) adminSetGroup(w http.ResponseWriter, raw []byte) {
	body, ok := decodeAction[adminSetGroupReq](w, raw)
	if !ok {
		return
	}
	target := strings.TrimSpace(body.Group)
	if target == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "group 不能为空", "invalid_request_error")
		return
	}
	moved := s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		if body.ID != "" && a.ID != body.ID {
			return false
		}
		if body.FromGroup != "" && !strings.EqualFold(a.GroupName(), body.FromGroup) {
			return false
		}
		if a.GroupName() == target {
			return false
		}
		a.Group = target
		return true
	})
	if !s.persistOrFail(w) {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved, "group": target})
}
