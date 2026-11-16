package deepai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// maxJSONBytes caps the size of upstream JSON response bodies (model catalog /
// quota / search).
const maxJSONBytes = 8 << 20

// HTTPError is a non-2xx response returned by the upstream.
type HTTPError struct {
	Op     string
	Status int
	Body   []byte
}

// Error returns a readable error message (prefers the upstream error.message).
func (e *HTTPError) Error() string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &payload) == nil && payload.Error.Message != "" {
		return fmt.Sprintf("%s: %s", e.Op, payload.Error.Message)
	}
	body := strings.TrimSpace(string(e.Body))
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, body)
}

// Code returns the upstream error.code.
func (e *HTTPError) Code() string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &payload) == nil {
		return payload.Error.Code
	}
	return ""
}

// Retryable reports whether the error is worth switching accounts / retrying.
//
// 400 / 404 / 422 are problems with the request itself; retrying is pointless;
// 401 / 403 / 409 / 429 / 5xx relate to the account or capacity and can be retried.
// Exception: policy error codes (social_login_required / machine_required /
// lane_version_unsupported) are terminal states for the account or client; switching accounts / retrying is useless.
func (e *HTTPError) Retryable() bool {
	switch e.Code() {
	case CodeSocialLogin, CodeMachineRequired, CodeLaneUnsupported:
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.Status >= 500
}

// AuthFailed reports whether this is an account authentication failure.
func (e *HTTPError) AuthFailed() bool {
	if e.Status == http.StatusUnauthorized {
		return true
	}
	code := e.Code()
	return code == CodeAuthRequired || code == CodeInvalidAPIKey || code == CodeEmailUnverified
}

// SocialLoginRequired reports whether the upstream requires the account to sign in with Google/Apple instead.
//
// Since 0.3.19, the upstream returns 403 on /theos and inference endpoints for accounts registered with "email + password";
// this is an account-level policy block, not an authentication failure, and not a retryable error.
func (e *HTTPError) SocialLoginRequired() bool { return e.Code() == CodeSocialLogin }

// MachineRequired reports whether the request was rejected for lacking a machine fingerprint (metering-model specific).
func (e *HTTPError) MachineRequired() bool { return e.Code() == CodeMachineRequired }

// EmailUnverified reports whether the upstream rejected the account because the email is unverified.
//
// Unlike a normal 403: the account's login state is invalid, so switching accounts makes no difference;
// it should be marked and skipped (rather than treated as a retryable failure and repeatedly switching accounts).
func (e *HTTPError) EmailUnverified() bool { return e.Code() == CodeEmailUnverified }

// LaneUnsupported reports that the lane version is no longer accepted by the upstream (426).
func (e *HTTPError) LaneUnsupported() bool {
	return e.Status == http.StatusUpgradeRequired || e.Code() == CodeLaneUnsupported
}

// BodyRejected reports whether the upstream rejected the request because "the request body is not the shape DeepAI Code sends".
//
// The upstream added request body validation for new lanes; the typical response is 400 + code=invalid_request_error,
// and message has two empirically observed texts:
// - `This request is not one DeepAI Code sends.` (old, since 2026-11-07)
// - `This request cannot be accepted.` (since 2026-11-13, the 0.3.36 era)
//
// Both versions must recognize it: if the new message is missed, ChatBodyFallback (adds system + normalizes fields) will not retry,
// and the caller gets a 400 directly.
//
// From 2026-11-15, add another fallback layer: the message may change again, while the code stays
// invalid_request_error. As long as it is 400 + that code, treat it as a shape rejection (only the chat path will
// actually retry); if it still fails after fixing the shape, the second error is returned as-is, without swallowing the cause.
func (e *HTTPError) BodyRejected() bool {
	if e.Status != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(e.Body))
	if strings.Contains(body, CodeNotDeepAICodeMsg) || strings.Contains(body, CodeNotAcceptedMsg) {
		return true
	}
	return e.Code() == CodeInvalidReq
}

// QuotaExhausted reports whether the account's daily quota is used up (paid model / negative balance).
func (e *HTTPError) QuotaExhausted() bool { return e.Code() == CodeTheosExhausted }

// QuotaResetAt parses refresh_at (RFC3339) from quota-type errors.
//
// body looks like {"error":{"code":"theos_exhausted","refresh_at":"2026-11-14T00:05:52+00:00"}};
// when it cannot be parsed, use fallback (callers usually pass 6h, consistent with the "Try again tomorrow" cadence).
func (e *HTTPError) QuotaResetAt(fallback time.Duration, now time.Time) time.Time {
	var payload struct {
		Error struct {
			RefreshAt string `json:"refresh_at"`
		} `json:"error"`
	}
	if len(e.Body) > 0 && json.Unmarshal(e.Body, &payload) == nil && payload.Error.RefreshAt != "" {
		if ts, err := time.Parse(time.RFC3339, payload.Error.RefreshAt); err == nil && ts.After(now) {
			return ts
		}
	}
	return now.Add(fallback)
}

// CheckInRefused reports whether check-in was rate-limited by the upstream based on "restarts per day".
//
// It differs from account unavailability: inference still returns 200 as usual, but falls into the stand-in model branch
// (see docs/recon.md §5.1), so the account must not be marked as error.
func (e *HTTPError) CheckInRefused() bool { return e.Code() == CodeCheckInRefused }

// IsSocialLoginRequired reports whether err is social_login_required.
func IsSocialLoginRequired(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.SocialLoginRequired()
}

// IsCheckInRefused reports whether err is check-in rate-limited (check_in_refused).
func IsCheckInRefused(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.CheckInRefused()
}

// CapacityWaiting reports whether it is a capacity rejection for "session busy".
func (e *HTTPError) CapacityWaiting() bool {
	return e.Status == http.StatusConflict && e.Code() == "session_busy"
}

// RetryAfter parses the Retry-After header (seconds), defaulting to fallback.
//
// Same shape as the desktop waitForDeepAICapacity: a parseable number of seconds is clamped to [1, 30],
// and only when the header is missing / non-numeric (including HTTP-date) is the caller's fallback used.
func RetryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return fallback
	}
	var secs int
	if _, err := fmt.Sscanf(raw, "%d", &secs); err != nil {
		return fallback
	}
	if secs < 1 {
		secs = 1
	}
	if secs > 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

// transientMarkers are known "exit/network flakiness" error signatures.
//
// Some proxies return custom messages during tunnel establishment (e.g. "No exit node"),
// It is neither a net.Error nor wraps io.EOF, so it can only be identified by its message text.
var transientMarkers = []string{
	"no exit node",
	"unexpected eof",
	"tls handshake timeout",
	"connection reset",
	"connection refused",
	"broken pipe",
	"i/o timeout",
	"timeout awaiting response",
	"temporary failure in name resolution",
}

// IsTransient reports whether the error is a transient failure where "retrying may succeed".
//
// Used for backoff retry decisions on critical paths such as /theos liveness verification and account pool probing:
// Network jitter and 5xx/429 are worth retrying; 4xx indicates a problem with the request or the account itself, so retrying is meaningless.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// Active cancellation by the caller does not count as retryable.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return true
		}
		return he.Status >= 500
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// Client is the DeepAI upstream client.
//
// A single Client can serve multiple accounts (reused in buckets by proxy), and check-in state is isolated by API Key.
type Client struct {
	Origin    string
	HTTP      *http.Client
	Machine   string
	UserAgent string
	Proxy     string

	// lane is the currently used Theos lane version (atomic: read concurrently during probing/lane switching).
	// A zero value means using the compile-time LaneVersion.
	lane atomic.Value

	// Instance is the process-level instance ID (16-byte random hex), sent with all JSON requests.
	Instance string

	// BrowserFingerprint determines whether to carry the account's browser fingerprint headers (User-Agent /
	// sec-ch-ua / Accept-Language) to upstream requests.
	//
	// The desktop client uses Node fetch and **never sends these headers**; sending them is equivalent to self-identifying on the inference API
	// "I am not the client". The registration flow needs them (to stay consistent with web registration),
	// so only NewClientWithFingerprint enables it.
	BrowserFingerprint bool

	// CheckInBudget is the wait budget for synchronous check-in before inference (0 = default 10s).
	//
	// Read-only, no semantic change: if the budget is exceeded, inference proceeds with the existing nonce, and it is replenished in the next round.
	// The console's "check-in blocking budget" writes it via SetCheckInBudget.
	CheckInBudget time.Duration

	// checkIns caches the check-in challenge and heartbeat time for each API Key.
	checkIns *checkInStore
}

// NewClient constructs the upstream client (reads the environment proxy by default).
//
// DEEPAI_API_ORIGIN can override the upstream address (only http/https accepted).
func NewClient() *Client { return inferClient(NewClientWithFingerprint("", "")) }

// NewClientWithProxy constructs an upstream client that goes through the specified proxy.
func NewClientWithProxy(proxyRaw string) *Client {
	return inferClient(NewClientWithFingerprint(proxyRaw, ""))
}

// inferClient switches the client into "inference form": without the browser fingerprint.
//
// NewClientWithFingerprint dedicated to the registration flow (carries the account's Chrome fingerprint by default);
// all other call sites are inference / probing and must match the desktop client.
func inferClient(c *Client) *Client {
	c.BrowserFingerprint = false
	return c
}

// NewClientWithFingerprint constructs an upstream client with proxy and User-Agent.
func NewClientWithFingerprint(proxyRaw, userAgent string) *Client {
	origin := strings.TrimRight(EnvOr("DEEPAI_API_ORIGIN", DefaultOrigin), "/")
	if u, err := url.Parse(origin); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		origin = DefaultOrigin
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw != "" {
		if u, err := url.Parse(proxyRaw); err == nil && u.Host != "" {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &Client{
		Origin:             origin,
		Machine:            MachineID(),
		UserAgent:          strings.TrimSpace(userAgent),
		Proxy:              proxyRaw,
		Instance:           newInstanceID(),
		BrowserFingerprint: true,
		checkIns:           newCheckInStore(),
		HTTP: &http.Client{
			Transport: transport,
			// The upstream returns a 302 redirect to the login page; following it would only mask authentication failures.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ModelURL returns the full address under /deepai_code.
func (c *Client) ModelURL(path string) string { return c.Origin + ModelBasePath + path }

// RootURL returns the full root path address.
func (c *Client) RootURL(path string) string { return c.Origin + path }

// SignInURL returns the same browser login address as the desktop client.
func (c *Client) SignInURL(state string, port int) string {
	u := c.ModelURL(PathSignIn)
	q := url.Values{}
	q.Set("state", state)
	q.Set("port", fmt.Sprintf("%d", port))
	return u + "?" + q.Encode()
}

// desktopInferenceHeaders replicates the default request headers of the desktop client's Node fetch (undici).
//
// Empirically, Node 22's global fetch sends:
//
//	user-agent: node / accept: */* / accept-language: * / sec-fetch-mode: cors
//
// no browser UA, and no sec-ch-ua. The gateway previously carried the registration Chrome fingerprint over to
// inference requests, which is inconsistent with the real client; here we align with the measured values.
var desktopInferenceHeaders = map[string]string{
	"User-Agent":      "node",
	"Accept":          "*/*",
	"Accept-Language": "*",
	"Sec-Fetch-Mode":  "cors",
}

// commonHeaders writes the common headers for all upstream JSON requests.
//
// Content-Type is set unconditionally (including bodyless GETs): the desktop client's requestDeepAIJson
// header literal also includes application/json for /models, /theos, and /get_usage_summary.
func (c *Client) commonHeaders(req *http.Request, acct *Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderLane, c.laneVersion())
	if c.Instance != "" {
		req.Header.Set(HeaderInstance, c.Instance)
	}
	// Machine fingerprint prefers the account-level one: the upstream grants the daily free quota based on it, and sharing it would cancel each other out.
	machine := c.Machine
	if acct != nil && acct.MachineID != "" {
		if fp := MachineFingerprint(acct.MachineID); fp != "" {
			machine = fp
		}
	}
	if machine != "" {
		req.Header.Set(HeaderMachine, machine)
	}

	// Choose one of the two fingerprint forms; do not mix them:
	// - BrowserFingerprint=true: registration flow, carrying the account's Chrome fingerprint;
	// - Otherwise: consistent with desktop inference, only carry Node fetch's default headers.
	//
	// Note: cannot return here - there is still Api-Key (authentication header) to write afterwards.
	if !c.BrowserFingerprint {
		for k, v := range desktopInferenceHeaders {
			req.Header.Set(k, v)
		}
	} else {
		req.Header.Set("Accept", "application/json")
		// Fingerprint headers also prefer account-level: registration and inference must use the same set of features.
		ua := c.UserAgent
		if acct != nil && acct.UserAgent != "" {
			ua = acct.UserAgent
		}
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if acct != nil {
			if acct.AcceptLanguage != "" {
				req.Header.Set("Accept-Language", acct.AcceptLanguage)
			}
			if acct.SecCHUA != "" {
				req.Header.Set("sec-ch-ua", acct.SecCHUA)
				req.Header.Set("sec-ch-ua-mobile", "?0")
			}
			if acct.Platform != "" {
				req.Header.Set("sec-ch-ua-platform", acct.Platform)
			}
		}
	}
	if acct != nil && acct.APIKey != "" {
		req.Header.Set(HeaderKeyName, acct.APIKey)
	}
}

// laneGuessAhead is the number of steps to guess upward during lane detection (the upstream may raise the lane before sending to the client).
const laneGuessAhead = 6

// laneCandidates returns the lane detection sequence: compile-time default first, then guess upward for newer lanes,
// and finally fall back to the old lane.
//
// The order cannot be reversed: `/models` is very lenient with old lanes (in the 0.3.37 era, both lanes 5 / 6 returned 200),
// starting from the old value will permanently pin the gateway to the old lane - the catalog of lane 6 is not the same as 5
// (`contextWindow` all converge to 320000), pinning to 5 is equivalent to continuing to impersonate an old client.
func laneCandidates() []string {
	base, err := strconv.Atoi(LaneVersion)
	if err != nil {
		return []string{LaneVersion}
	}
	out := make([]string, 0, laneGuessAhead+base)
	for lane := base; lane <= base+laneGuessAhead; lane++ {
		out = append(out, strconv.Itoa(lane))
	}
	for lane := base - 1; lane >= 2; lane-- {
		out = append(out, strconv.Itoa(lane))
	}
	return out
}

// SetLane sets the current lane (empty string = use compile-time default).
func (c *Client) SetLane(lane string) {
	c.lane.Store(strings.TrimSpace(lane))
}

// laneVersion returns the lane version to send with this request.
func (c *Client) laneVersion() string {
	if v, ok := c.lane.Load().(string); ok && v != "" {
		return v
	}
	return LaneVersion
}

// Lane returns the currently effective lane version.
func (c *Client) Lane() string { return c.laneVersion() }

// DetectLane probes the available lane version using /models.
//
// Since 2026-11-14, /models also requires Api-Key (without it, always 401 auth_required, see
// docs/recon.md §2), so detection must carry an account credential; when acct is nil, send an anonymous
// request, the upstream will return 401, and detection will only get "authentication failed" and cannot determine the lane.
// On a hit, write back to Client immediately, and subsequent requests automatically carry the new lane; if all candidates fail, return the last error.
func (c *Client) DetectLane(ctx context.Context, acct *Account) (string, error) {
	var lastErr error
	for _, lane := range laneCandidates() {
		ok, err := c.laneWorks(ctx, lane, acct)
		if ok {
			c.SetLane(lane)
			return lane, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("所有候选车道都被上游拒绝（426）")
	}
	return "", lastErr
}

// laneWorks requests /models once with the candidate lane.
//
// Only 2xx counts as passing: 5xx means upstream flakiness rather than a wrong lane, so continue trying the next candidate;
// 401 means missing credentials (since 2026-11-14 the upstream also requires Api-Key for /models),
// Unrelated to the lane; retries continue as well, but the result is guaranteed to be total failure.
func (c *Client) laneWorks(ctx context.Context, lane string, acct *Account) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathModels), nil)
	if err != nil {
		return false, err
	}
	c.commonHeaders(req, acct)
	req.Header.Set(HeaderLane, lane) // Overwrite with the value under test
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode/100 == 2, nil
}

// do sends the request and returns the response as-is (the caller is responsible for closing Body).
//
// All 3xx are normalized to 401: for an invalid Key, the upstream returns 302 to the login page. Following it would turn the authentication failure
// into a fake 200 (hence CheckRedirect returns ErrUseLastResponse), while passing it through as-is on
// the API client side is a bare 3xx without Location. After unifying to auth_required,
// the caller marks the account as failed and switches accounts, and the client receives a recognizable 401.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 3 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &HTTPError{
			Op:     "上游鉴权",
			Status: http.StatusUnauthorized,
			Body: []byte(fmt.Sprintf(
				`{"error":{"type":%q,"code":%q,"message":"上游把请求重定向到登录页（API Key 无效或已失效，status=%d）"}}`,
				CodeAuthRequired, CodeAuthRequired, resp.StatusCode)),
		}
	}
	return resp, nil
}

// FetchModels fetches the upstream model catalog (as of 2026-11-14, Api-Key is required).
//
// The earlier conclusion was "the upstream catalog does not vary by account; with or without `Api-Key`, the same catalog is returned",
// so it was deliberately kept credential-free. **Empirically invalidated on 2026-11-14**: without a Key, requests are always
// 401 `{"code":"auth_required","message":"Sign in to DeepAI to use DeepAI Code."}`,
// return 200 only when carrying the Key of any account (see docs/recon.md §2). Therefore:
// - When acct is non-nil, include that account's Key (the catalog is independent of the account, so any one will do);
// - When acct is nil, still send an anonymous request: used only for "connectivity probing when there is no account",
// the upstream returns 401, and the caller uses this to distinguish "link is up" from "catalog fetch succeeded".
//
// Note that the upstream catalog **does change**: in one measurement on 2026-11-14, it grew from 21 to 27 (6 new
// free / paid models, with none removed). The catalog is cached and updates only on restart or manual refresh.
func (c *Client) FetchModels(ctx context.Context, acct *Account) ([]Model, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathModels), nil)
	if err != nil {
		return nil, "", err
	}
	c.commonHeaders(req, acct)

	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", &HTTPError{Op: "模型目录", Status: resp.StatusCode, Body: body}
	}

	var payload struct {
		Models  []Model `json:"models"`
		Default string  `json:"default"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", fmt.Errorf("模型目录解析失败: %w", err)
	}
	models := make([]Model, 0, len(payload.Models))
	seen := map[string]bool{}
	for _, m := range payload.Models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if m.Input == nil {
			m.Input = []string{"text"}
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil, "", errors.New("上游模型目录为空")
	}
	def := payload.Default
	if def == "" {
		def = models[0].ID
	}
	return models, def, nil
}

// OpenModel initiates one inference request and returns the unread response.
//
// path is PathChat or PathResponses; affinity is passed through as X-Session-Affinity
// passed through (the upstream uses it for session-sticky scheduling).
func (c *Client) OpenModel(ctx context.Context, acct *Account, path string, body []byte, affinity string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)
	// Before forwarding inference, the desktop client records lastRelayAt once; the pi field in the heartbeat is derived from this.
	c.markRelay(acct)
	if affinity != "" {
		req.Header.Set(HeaderAffinity, affinity)
	}
	return c.do(req)
}

// Theos queries the account's free quota / balance.
//
// Since 0.3.19, /theos is also the check-in channel: the response (including a 403 rejection) carries check_in,
// and the next request sends the nonce back along with a heartbeat.
func (c *Client) Theos(ctx context.Context, acct *Account) (*Allowance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathTheos), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)
	c.applyCheckInHeaders(req, acct)
	sentNonce := req.Header.Get(HeaderCheckIn)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// If the request carries a nonce, it means this one is a "callback": check-in is complete (a 403 rejection counts too,
	// and the desktop client likewise records a noteCheckIn for a rejection).
	c.markAcked(acct, sentNonce)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	// Whether it succeeds or is rejected, consume the check-in challenge first: the upstream also issues it in a 403.
	c.captureCheckIn(acct, body)
	if resp.StatusCode/100 != 2 {
		httpErr := &HTTPError{Op: "查询 Theos", Status: resp.StatusCode, Body: body}
		// Check-in is rate-limited for the day: apply a silent lock to the account so EnsureCheckIn does not hit it again every 30s.
		if httpErr.CheckInRefused() {
			c.lockCheckIn(acct, CheckInRefusedLock)
		}
		return nil, httpErr
	}
	var out Allowance
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("Theos 响应解析失败: %w", err)
	}
	out.CheckedAt = time.Now().Unix()
	return &out, nil
}

// UsageSummary queries the account's subscription / usage.
//
// Consistent with the desktop client: this root-path request does **not** override the User-Agent (it keeps Node fetch's
// `node`). The `DeepAI-Code` UA belongs only to /gravity-display-proxy sponsored-slot rendering
// (see DesktopUserAgent); carrying it over to the usage endpoint would just be another anomaly signal.
func (c *Client) UsageSummary(ctx context.Context, acct *Account) (*UsageSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.RootURL(PathUsageSummary), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "查询用量", Status: resp.StatusCode, Body: body}
	}
	var out UsageSummary
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("用量响应解析失败: %w", err)
	}
	return &out, nil
}

// WebSearch calls the upstream web search capability.
func (c *Client) WebSearch(ctx context.Context, acct *Account, payload []byte) ([]byte, error) {
	return c.postJSON(ctx, acct, PathWebSearch, payload, "联网搜索")
}

// FindService calls the upstream service recommendation capability.
func (c *Client) FindService(ctx context.Context, acct *Account, payload []byte) ([]byte, error) {
	return c.postJSON(ctx, acct, PathFindService, payload, "服务搜索")
}

func (c *Client) postJSON(ctx context.Context, acct *Account, path string, payload []byte, op string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(path), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: op, Status: resp.StatusCode, Body: body}
	}
	return body, nil
}

// Transcribe forwards audio to the upstream speech-to-text endpoint.
func (c *Client) Transcribe(ctx context.Context, acct *Account, filename, contentType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("音频内容为空")
	}
	if len(data) > 8<<20 {
		return "", errors.New("音频超过 8 MB")
	}
	if filename == "" {
		filename = "rec.webm"
	}
	if contentType == "" {
		contentType = "audio/webm"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="voiceRecording"; filename="%s"`, escapeQuotes(filename)))
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RootURL(PathSpeechToText), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if acct != nil {
		req.Header.Set("api-key", acct.APIKey)
	}

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", &HTTPError{Op: "语音转写", Status: resp.StatusCode, Body: body}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("转写响应解析失败: %w", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", errors.New("上游未返回转写文本")
	}
	return strings.TrimSpace(out.Text), nil
}

// ExchangeCode exchanges the code from the desktop client's login callback for an API Key.
func (c *Client) ExchangeCode(ctx context.Context, code string) (*Credentials, error) {
	payload, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(PathExchange), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "登录换取密钥", Status: resp.StatusCode, Body: body}
	}
	var out struct {
		APIKey   string `json:"api_key"`
		Username string `json:"username"`
		IsPro    bool   `json:"is_pro"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("登录响应解析失败: %w", err)
	}
	if out.APIKey == "" || strings.ContainsAny(out.APIKey, "\r\n") {
		return nil, errors.New("登录响应缺少有效 API Key")
	}
	return &Credentials{APIKey: out.APIKey, Username: out.Username, IsPro: out.IsPro}, nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}
