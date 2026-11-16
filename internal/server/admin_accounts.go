package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// --- Account management endpoints and quota inspection ---

// Action values for the account management endpoint.
const (
	actionImport        = "import"
	actionManual        = "manual"
	actionEnroll        = "enroll"
	actionRemove        = "remove"
	actionToggle        = "toggle"
	actionClearErrors   = "clear-errors"
	actionAssignMachine = "assign-machine"
	actionAssignProxy   = "assign-proxy"
	actionSetGroup      = "set-group"
	actionMarkVerified  = "mark-verified"
)

func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	// Same path, dispatched by method: GET runs a paginated query, POST runs a mutation.
	if r.Method == http.MethodGet {
		s.handleAdminAccountsPage(w, r)
		return
	}
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET / POST", "invalid_request_error")
		return
	}
	// Two-step decoding: read the action first, then let that action decode its own
	// fields (see admin_actions.go).
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	var env adminActionEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "请求体解析失败", "invalid_request_error")
		return
	}
	if fn, ok := adminActions[env.Action]; ok {
		fn(s, w, raw)
		return
	}
	httpx.WriteErr(w, http.StatusBadRequest, "未知操作: "+env.Action, "invalid_request_error")
}

// handleAdminCheckin queries Theos quota for a batch of accounts.
//
// Optional parameters:
//   - id: probe only this account;
//   - group: probe only accounts in this group;
//   - stale_only: probe only accounts whose quota is stale (refresh_at passed / never probed / older than 15 minutes);
//   - limit: maximum number to probe in this batch (default and cap are both maxCheckinBatch).
//
// The response carries total / probed / remaining: a full sweep of a 653-account pool
// takes 11 minutes, so a single request only does one batch, and the console continues
// with the next batch according to remaining (probed accounts have checked_at advanced
// to now, so the next batch naturally picks the unprobed ones and does not burn quota
// twice).
func (s *Server) handleAdminCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID        string `json:"id"`
		Group     string `json:"group"`
		Limit     int    `json:"limit"`
		StaleOnly bool   `json:"stale_only"`
	}
	// Parameters may be omitted (equivalent to "probe one batch of all accounts"), but
	// malformed JSON must be rejected.
	if !httpx.DecodeOptionalJSON(w, r, &body) {
		return
	}

	now := time.Now()
	var targets []*deepai.Account
	switch {
	case strings.TrimSpace(body.ID) != "":
		if a := s.Store.Account(body.ID); a != nil {
			targets = append(targets, a)
		}
	default:
		group := strings.TrimSpace(body.Group)
		for _, a := range s.Store.Accounts() {
			if group != "" && !strings.EqualFold(a.GroupName(), group) {
				continue
			}
			if body.StaleOnly && !quotaNeedsRefresh(a, now, 0) {
				continue
			}
			targets = append(targets, a)
		}
	}
	total := len(targets)

	// Prioritize accounts whose quota just refreshed / was never probed; only then does
	// batching make sense.
	sortByQuotaDue(targets, now)
	limit := body.Limit
	if limit <= 0 || limit > maxCheckinBatch {
		limit = maxCheckinBatch
	}
	batch := targets
	if len(batch) > limit {
		batch = batch[:limit]
	}

	// Finish the batch even if the page is closed: results are persisted to disk and
	// visible when the page reopens. Without WithoutCancel, switching browser tabs
	// (which aborts the fetch) would leave half the batch in its old state, and the
	// console would look like a "refresh failed".
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	out := s.probeBatch(ctx, batch, 30*time.Second, true)

	results := make([]map[string]any, 0, len(out))
	settled := make(map[string]bool, len(out))
	probed := 0
	for _, o := range out {
		if o.Account == nil {
			continue
		}
		item := map[string]any{"id": o.Account.ID, "name": o.Account.Name}
		switch {
		case o.Skipped:
			// A probe is already running: this is neither a failure nor a probe done in
			// this round, so it does **not** go into settled. Putting it there would make
			// remaining treat it as "already probed" and converge, even though its state
			// is still unknown; leaving it out makes the next batch count it in remaining
			// again, and it clears once the other probe finishes.
			item["ok"] = true
			item["skipped"] = true
			item["note"] = "已有探测在进行"
		default:
			settled[o.Account.ID] = true
			probed++
			if o.Allowance == nil && o.Err == nil {
				item["ok"] = false
				item["err"] = "探测已取消"
			} else {
				item["ok"] = o.Err == nil
				item["theos"] = o.Allowance
				if o.Err != nil {
					item["err"] = o.Err.Error()
				}
			}
		}
		results = append(results, item)
	}

	// remaining is the number of accounts in the filtered set that are still expired and not in this batch, not
	// total - probed: otherwise each batch leaves the same in-flight/failed accounts behind, and the console will retry them repeatedly.
	remaining := 0
	for _, a := range targets {
		if settled[a.ID] {
			continue
		}
		if quotaNeedsRefresh(a, now, 0) {
			remaining++
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"total":     total,
		"probed":    probed,              // Number of accounts that actually initiated probing (skipped not counted)
		"skipped":   len(batch) - probed, // Skipped this round because a probe is already running
		"remaining": remaining,
		"results":   results,
	})
}
