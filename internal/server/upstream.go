package server

import (
	"context"
	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/logx"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// openModel opens one upstream inference request on the account pool.
//
// Semantics:
// - 2xx: return the response immediately; the caller is responsible for closing Body;
// - 400/404/422 and other request errors: no retry, return *deepai.HTTPError directly;
// - 401/403/429/5xx: record the account failure and retry with another account;
// - 409 session_busy: if the pool has a free account, switch to it immediately; only when the whole pool is full, wait once according to Retry-After;
// - Not enough slots: queue briefly (admissionWait), and return 429 only on timeout.
func (s *Server) openModel(ctx context.Context, path string, body []byte, affinity, group, apiKey string, stream bool) (*http.Response, *deepai.Account, error) {
	// Only the pool size is needed: AccountCounts does not unpack the snapshot, avoiding a deep copy of the whole pool for a single len().
	acctTotal, _ := s.Store.AccountCounts()
	if acctTotal == 0 {
		return nil, nil, errors.New("没有可用账号，请先在控制台导入桌面端凭据或添加 API Key")
	}
	settings := s.Store.Settings()
	// Preset desktop shape: only takes effect for callers that have already been rejected by the upstream for their shape (shape.go),
	// and only fills in system without trimming fields, so it does not alter sampling parameters explicitly provided by the caller.
	if path == deepai.PathChat && settings.PresetDesktopShape && apiKey != "" && s.shapeNeeded(apiKey) {
		if alt, inserted := deepai.EnsureSystemMessage(body, ""); inserted {
			body = alt
		}
	}
	maxTries := settings.MaxRetries
	if maxTries <= 0 {
		maxTries = 3
	}
	if maxTries > acctTotal+2 {
		maxTries = acctTotal + 2
	}

	var (
		lastErr error
		used    *deepai.Account
	)
	deadline := time.Now().Add(s.admissionWait)
	for i := 0; i < maxTries; i++ {
		acct, ok, candidates := s.pickFreeAccount(group)
		if !candidates {
			// No schedulable account in the pool (all blocked / empty group): delegate to noAccountError to explain the reason.
			break
		}
		if !ok {
			// All busy: queue briefly instead of returning 429 immediately; waiting for a slot does not consume the retry budget.
			lastErr = errAllAccountsBusy()
			if !time.Now().Before(deadline) {
				break
			}
			waitStart := time.Now()
			select {
			case <-time.After(admissionPoll):
			case <-ctx.Done():
				return nil, used, ctx.Err()
			}
			s.metrics.observeAdmissionWait(time.Since(waitStart))
			i--
			continue
		}
		// Refresh the quota snapshot asynchronously when it is too old: only refresh accounts that have already been probed but whose results are stale,
		// and do not add upstream requests for never-probed accounts on the hot path (those are handled by pool-entry re-probing / the background daemon).
		if acct.Theos != nil && quotaNeedsRefresh(acct, time.Now(), quotaStaleAfter) {
			s.probeAsync(acct)
		}
		used = acct

		client := s.clientFor(acct)
		// Consistent with the desktop client: perform check-in before inference (a request is actually sent only when the challenge has expired / was not returned).
		//
		// This is a **synchronous** call: at worst it adds checkInAttemptTimeout (10s) of inference latency.
		// It cannot be made asynchronous: the nonce return is the upstream's basis for determining a "real client"; sending the chat first and then supplementing the
		// check-in would make that request land in the stand-in branch (see docs/recon.md §5.1). Failure does not affect this
		// inference, but the slowness is user-perceivable, so leave a log line when it exceeds the threshold (previously this wait
		// was not visible in any log).
		if path == deepai.PathChat || path == deepai.PathResponses {
			outcome := client.EnsureCheckIn(ctx, acct)
			s.metrics.observeCheckIn(outcome)
			if outcome.Attempted && outcome.Waited >= checkInSlowWarn {
				logx.Warn("签到阻塞了推理请求", "account", acctLabel(acct), "ms", outcome.Waited.Milliseconds(), "timed_out", outcome.TimedOut)
			}
		}
		resp, err := client.OpenModel(ctx, acct, path, body, affinity)
		if err != nil {
			s.releaseAccount(acct.ID)
			lastErr = err
			s.metrics.observeUpstreamError(path, "transport", 0)
			s.Store.MarkAccountFailure(acct.ID, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}

		if resp.StatusCode/100 == 2 {
			s.Store.MarkAccountResult(acct.ID, true, "")
			resp.Body = s.holdResponseBody(resp.Body, acct.ID, stream)
			return resp, acct, nil
		}

		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			s.releaseAccount(acct.ID)
			lastErr = readErr
			s.Store.MarkAccountFailure(acct.ID, readErr)
			continue
		}
		httpErr := &deepai.HTTPError{Op: "上游推理", Status: resp.StatusCode, Body: payload}

		// Upstream 0.3.19 added shape validation on the request body (400 + "This request is not one
		// DeepAI Code sends."). Use the desktop field whitelist + append a system message and retry once on the same account.
		if httpErr.BodyRejected() && path == deepai.PathChat {
			s.markShapeNeeded(apiKey)
			if alt, ok := deepai.ChatBodyFallback(body, ""); ok {
				s.metrics.addFallback()
				log.Printf("[upstream] 请求体被拒，按桌面端形状重试：account=%s", acctLabel(acct))
				if retry, err2 := client.OpenModel(ctx, acct, path, alt, affinity); err2 == nil {
					if retry.StatusCode/100 == 2 {
						s.Store.MarkAccountResult(acct.ID, true, "")
						retry.Body = s.holdResponseBody(retry.Body, acct.ID, stream)
						return retry, acct, nil
					}
					payload2, _ := io.ReadAll(io.LimitReader(retry.Body, 4<<20))
					_ = retry.Body.Close()
					if len(payload2) > 0 {
						payload = payload2
						httpErr = &deepai.HTTPError{Op: httpErr.Op, Status: retry.StatusCode, Body: payload2}
					}
				} else {
					log.Printf("[upstream] 形状回退重试失败：account=%s err=%v", acctLabel(acct), err2)
				}
			}
		}
		s.releaseAccount(acct.ID)
		s.metrics.observeUpstreamError(path, httpErr.Code(), httpErr.Status)

		switch classifyUpstreamError(httpErr) {
		// Email not verified: the account's login state is invalid; switching accounts won't save it.
		// After marking, continue trying the next account (the next successful /theos call will automatically unblock it);
		// Do not treat it as an ordinary 403 and retry repeatedly.
		case actBlockAndRetry:
			s.Store.BlockAccount(acct.ID, blockedReason(httpErr))
			lastErr = httpErr
			continue
		// Account-level policy block: record as blocked and return directly (the next request scheduling will skip it).
		case actBlockAndReturn:
			s.Store.BlockAccount(acct.ID, blockedReason(httpErr))
			return nil, acct, httpErr
		// Missing machine fingerprint / a problem with the request itself: switching accounts won't help, return directly.
		case actReturn:
			return nil, acct, httpErr
		// Shape rejected: the shape was already patched and retried once above (still failing means patching doesn't help).
		case actShapeFallback:
			return nil, acct, httpErr
		// Wrong lane: first use the account credentials to probe /models once; if a new lane is hit, switch lanes and retry;
		// If still in the cooldown period / the probe fails, hand the 426 back to the caller as-is (prompting a gateway upgrade).
		case actProbeLane:
			before := s.CurrentLane()
			if lane, lerr := s.RefreshLane(ctx); lerr == nil && lane != "" && lane != before {
				lastErr = httpErr
				continue
			}
			return nil, acct, httpErr
		// Daily quota exhausted: mark until refresh_at, switch accounts; don't keep blindly hitting 403 on the same account.
		case actQuotaExhausted:
			until := httpErr.QuotaResetAt(quotaExhaustedFallback, time.Now())
			s.Store.MarkQuotaExhausted(acct.ID, until)
			logx.Warn("账号当日额度用完", "account", acctLabel(acct), "until", until.Format(time.RFC3339))
			lastErr = httpErr
			continue
		// Capacity rejection: if the pool still has a free slot, switch accounts immediately (stickiness is guaranteed by X-Session-Affinity,
		// no need to wait idly on the same account); only when the whole pool is full, wait once per Retry-After.
		case actWaitCapacity:
			lastErr = httpErr
			if s.hasFreeAccount() {
				continue
			}
			wait := deepai.RetryAfter(resp, 2*time.Second)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, acct, ctx.Err()
			}
			continue
		}

		lastErr = httpErr
		s.Store.MarkAccountFailure(acct.ID, httpErr)
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = s.noAccountError()
	}
	return nil, used, lastErr
}

// checkInSlowWarn is the warning threshold for "check-in blocking inference".
//
// A normal check-in is 1~2 RTTs (hundreds of milliseconds); exceeding this value means the upstream has slowed down or is going through retries,
// and the user can perceive the first-packet latency, so it needs to be recorded.
const checkInSlowWarn = 2 * time.Second

// holdResponseBody wraps the upstream response body in two layers: the outer idle timeout, the inner returning the account slot.
//
// The two "return 2xx" paths (normal response and request-body shape fallback retry) must be completely consistent:
// if either layer is missed, when the upstream hangs after sending the first frame, both the connection and the account slot will hang until the client disconnects.
// Historically the fallback path missed the outer layer; this is the point the regression test must lock down.
func (s *Server) holdResponseBody(body io.ReadCloser, acctID string, stream bool) io.ReadCloser {
	firstByte := upstreamIdleTimeout
	if !stream {
		// Non-streaming: the first byte may take a long time (long thinking), and only afterward is a timeout declared based on inter-frame idle time.
		firstByte = upstreamFirstByteTimeout
	}
	return newIdleTimeoutBody(
		&releaseBody{ReadCloser: body, release: func() { s.releaseAccount(acctID) }},
		upstreamIdleTimeout,
		firstByte,
	)
}

// callJSON initiates a non-streaming upstream request and parses the JSON.
func (s *Server) callJSON(ctx context.Context, path string, payload map[string]any, affinity, group, apiKey string) (map[string]any, *deepai.Account, error) {
	ctx, cancel := context.WithTimeout(ctx, nonStreamBudget)
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("请求序列化失败: %w", err)
	}
	resp, acct, err := s.openModel(ctx, path, body, affinity, group, apiKey, false)
	if err != nil {
		return nil, acct, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, acct, fmt.Errorf("读取上游响应失败: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, acct, fmt.Errorf("上游响应解析失败: %w", err)
	}
	return out, acct, nil
}

// writeUpstreamErr returns the upstream error to the client as-is (or wrapped).
func writeUpstreamErr(w http.ResponseWriter, err error) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		if json.Valid(he.Body) && len(he.Body) > 0 {
			// Use the unified raw JSON write path: Content-Type and status code are maintained only in one place, writeRawJSON,
			// avoiding each error branch assembling the response headers separately. What is written is JSON, not HTML.
			httpx.WriteRawJSON(w, he.Status, he.Body)
			return
		}
		httpx.WriteErr(w, he.Status, he.Error(), "upstream_error")
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		httpx.WriteErr(w, http.StatusGatewayTimeout, "上游请求超时", "upstream_error")
		return
	}
	httpx.WriteErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
}

// writeAnthropicUpstreamErr returns the upstream error according to the Anthropic error structure.
func writeAnthropicUpstreamErr(w http.ResponseWriter, err error) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		typ := "api_error"
		switch he.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
			typ = "invalid_request_error"
		case http.StatusUnauthorized, http.StatusForbidden:
			typ = "authentication_error"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		}
		writeAnthropicErr(w, he.Status, typ, he.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	writeAnthropicErr(w, http.StatusBadGateway, "api_error", err.Error())
}
