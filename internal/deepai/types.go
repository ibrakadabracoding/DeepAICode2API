// Package deepai implements a reverse-engineered replica of the DeepAI Code desktop upstream service.
//
// The upstream is an OpenAI-compatible HTTPS interface:
//
//	POST https://api.deepai.org/deepai_code/chat/completions
//	POST https://api.deepai.org/deepai_code/responses        (openai-responses models only)
//	GET  https://api.deepai.org/deepai_code/models
//	GET  https://api.deepai.org/deepai_code/theos
//	POST https://api.deepai.org/deepai_code/web_search
//	POST https://api.deepai.org/deepai_code/find_service
//	GET  https://api.deepai.org/get_usage_summary
//	POST https://api.deepai.org/speech_to_text
//
// Authentication uses the API Key written after desktop login (the `Api-Key` header), along with
// `X-DeepAI-Code-Theos` lane version, `X-DeepAI-Code-Instance` process instance,
// `X-DeepAI-Code-Machine` machine fingerprint; `/theos` also requires a check-in nonce and heartbeat.
//
// Two upstream capabilities are intentionally not implemented (see docs/recon.md):
//   - Managed host control plane `/deepai_code/connect/*` (device registration / remote host / TLS relay):
//     it uses Ed25519 device identity + X25519 key exchange for client authentication, serving the desktop's
//     "turn this machine into a managed host" feature, unrelated to the inference replica;
//   - Sponsored slot rendering (`/gravity-*`): part of the UI monetization chain; the gateway has no rendering surface.
//
// `/models` and lane detection used to **deliberately carry no credentials** (only the lane header + machine fingerprint).
// **Stopped working as of 2026-11-14**: `/models` without `Api-Key` always returns 401 auth_required,
// only with any account's Key does it return 200. So both paths now need to borrow an account's credentials
// (see `Client.FetchModels` / `DetectLane` and docs/recon.md §2).
package deepai

import (
	"strings"
	"time"
)

// Upstream protocol constants.
const (
	// LaneVersion is the Theos billing lane version this gateway aligns with.
	//
	// 0.3.17 → 0.3.19: upstream raised the lane from 2 to 4; 0.3.36 (2026-11-13) raised it to 5;
	// 0.3.37 (2026-11-14) raised it to 6 again: the desktop sends `X-DeepAI-Code-Theos: 6`,
	// `/theos` also returns `lane_version: 6`. On lane mismatch it returns 426
	// lane_version_unsupported ("This version of DeepAI Code is no longer
	// supported"). 2026-11-15 measured: `/models` lane 6 → 200, 7 → 426;
	// `/theos` lanes 5 / 6 both 200, 4 → 426; inference endpoints 5 / 6 both 200.
	// Note that `/models` is very lenient toward old lanes (5 still 200), so probing must start from 6,
	// otherwise it will stay pinned to the old lane (see laneCandidates in client.go).
	LaneVersion = "6"

	// AppVersion is the application version number the upstream uses to identify the client version (the v= field in heartbeat).
	//
	// 0.3.20 → 0.3.30 → 0.3.32 → 0.3.35 → 0.3.36 → 0.3.37 verified item by item via white-box review.
	// The full `server.asar` diff from 0.3.36 → 0.3.37 (released 2026-11-14) has only 27
	// hunks, and the only one on the inference protocol surface is **lane 5 → 6**: the endpoint set, the 5
	// `X-DeepAI-Code-*` headers, instance number granularity, machine fingerprint, check-in / beacon shape, and chat
	// request body fields are all unchanged (the pi runtime is still 1.3.0). The remaining differences are long-session compression,
	// tool failure deduplication, Codex approval, SQLite indexes, and `thinkingLevelMap` (the latter only affects
	// the desktop dropdown, not the wire protocol).
	// Only the version number in the heartbeat needs to be updated; otherwise /theos will report an old client.
	AppVersion = "0.3.37"

	// DesktopUserAgent is the User-Agent used when the desktop client accesses the sponsored-slot
	// rendering proxy (/gravity-display-proxy).
	//
	// Note: it is used **only** on this one path; other root-path requests such as /get_usage_summary still send
	// Node fetch's `node` (see deepai.Client.UsageSummary).
	DesktopUserAgent = "DeepAI-Code"

	// Request header name.
	HeaderKeyName  = "Api-Key"             // The upstream uniformly uses Api-Key (case-insensitive)
	HeaderLane     = "X-DeepAI-Code-Theos" // Lane version
	HeaderMachine  = "X-DeepAI-Code-Machine"
	HeaderAffinity = "X-Session-Affinity"
	// HeaderInstance is a process-level random instance ID (hex of a 16-byte random number); on desktop, each
	// process generates it once and sends it with every JSON request.
	HeaderInstance = "X-DeepAI-Code-Instance"
	// HeaderCheckIn is the check-in nonce issued in the /theos response; it is sent back only on /theos requests.
	HeaderCheckIn = "X-DeepAI-Code-Check-In"
	// HeaderBeacon is the client heartbeat on /theos requests.
	//
	// Format `v=<version>;up=<seconds>;pi=<0|1>;focus=<0|1>`, where focus appears only
	// when the focus state is known (see deepaiBeacon): the gateway has no window focus, so it is omitted.
	HeaderBeacon = "X-DeepAI-Code-Beacon"

	// Upstream error codes.
	CodeSessionBusy     = "session_busy"
	CodeLaneUnsupported = "lane_version_unsupported"
	CodeSocialLogin     = "social_login_required"
	CodeMachineRequired = "machine_required"
	CodeEmailUnverified = "email_unverified"
	CodeAuthRequired    = "auth_required"
	CodeInvalidAPIKey   = "invalid_api_key"
	CodeInvalidReq      = "invalid_request_error"
	// CodeCheckInRefused means check-in is rate-limited: the account itself is fine and inference still works,
	// but check-in cannot be done again today - it will keep falling into the stand-in model branch until tomorrow.
	CodeCheckInRefused = "check_in_refused"
	// CodeTheosExhausted means "the account's quota for the day is used up": when the paid model's deduction is complete / the balance is negative,
	// the upstream returns 403 with refresh_at in the body (the next issuance time).
	//
	// Measured on 2026-11-13 (v0.1.3 era): with balance=-7.58, hitting minimax-m3 yields
	// {"error":{"code":"theos_exhausted","refresh_at":"..."}}.
	//
	// It is an account-level terminal state that self-heals the same day: switching accounts makes sense, retrying the same account does not.
	CodeTheosExhausted = "theos_exhausted"
	// CodeNotDeepAICodeMsg is the old message used when the upstream rejects "the request body is not desktop-shaped".
	CodeNotDeepAICodeMsg = "not one deepai code sends"
	// CodeNotAcceptedMsg is the new message for the same threshold (measured on 2026-11-13):
	// 400 {"error":{"message":"This request cannot be accepted.",
	// "type":"invalid_request_error","code":"invalid_request_error"}}.
	// Trigger conditions: UA=node + known client machine fingerprint + missing system message (adding system yields 200,
	// no tools needed); switching to an unknown machine fingerprint or changing the UA does not trigger it. Both message versions must be treated as BodyRejected.
	CodeNotAcceptedMsg = "this request cannot be accepted"

	// StatusBlocked is the account-level "upstream policy block" state (current source: social_login_required).
	//
	// Unlike ordinary errors: it is not a network/quota problem, and retrying with another account will not fix it,
	// the scheduler should skip it directly until a successful /theos sets it back to ok.
	StatusBlocked = "blocked"

	// StatusError is the "last call failed" state. It self-heals: on the next success, or
	// if no further failure occurs after ErrorStaleAfter, it is no longer considered abnormal (see AccountReady).
	StatusError = "error"

	// ErrorStaleAfter is the window in which an error can still represent the current state.
	//
	// Rationale for choosing 10 minutes: failures such as upstream EOF / 429 "Too many requests today"
	// occur per request and in bursts, while account monitoring / real traffic usually hits again within 5 minutes;
	// if no further failure occurs for more than 10 minutes, that indicates historical noise and it should no longer weigh on the badge.
	ErrorStaleAfter = 10 * time.Minute

	// Path prefix.
	ModelBasePath = "/deepai_code"

	// Model catalog path (relative to ModelBasePath).
	PathModels       = "/models"
	PathChat         = "/chat/completions"
	PathResponses    = "/responses"
	PathTheos        = "/theos"
	PathWebSearch    = "/web_search"
	PathFindService  = "/find_service"
	PathSessionsList = "/sessions/list"
	PathSessionsGet  = "/sessions/get"
	PathSessionsDel  = "/sessions/delete"
	PathSessionsSave = "/sessions/save" // Added in 0.3.20: cloud session / preference upload
	PathSessionTurn  = "/session_turn"
	PathSignIn       = "/desktop_auth/"
	PathExchange     = "/desktop_auth/exchange/"

	// Root path (not under /deepai_code).
	PathUsageSummary      = "/get_usage_summary"
	PathSpeechToText      = "/speech_to_text"
	PathAttachments       = "/chat_attachments/upload"
	PathAttachmentsGet    = "/chat_attachments/get"
	PathGravityDisplay    = "/gravity-display-proxy"
	PathGravityImpression = "/gravity-imp"

	// DefaultOrigin is the production upstream address.
	DefaultOrigin = "https://api.deepai.org"

	// DefaultModel is the fallback model when the catalog is unavailable.
	DefaultModel = "glm-5.3-flash"
)

// Model is an entry in the upstream model catalog.
type Model struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Reasoning       bool              `json:"reasoning"`
	Input           []string          `json:"input"`
	ContextWindow   int               `json:"contextWindow"`
	MaxTokens       int               `json:"maxTokens"`
	Premier         bool              `json:"premier,omitempty"`
	API             string            `json:"api,omitempty"` // empty = chat/completions; openai-responses = /responses
	TheosPerRequest string            `json:"theosPerRequest,omitempty"`
	TheosPerSecond  map[string]string `json:"theosPerSecond,omitempty"`
}

// IsResponses reports whether the model uses the OpenAI Responses endpoint.
func (m Model) IsResponses() bool { return m.API == "openai-responses" }

// Free reports whether the model is free (0 Theos per request).
func (m Model) Free() bool {
	if m.TheosPerRequest != "" {
		return m.TheosPerRequest == "0" || m.TheosPerRequest == "0.0" || m.TheosPerRequest == "0.00"
	}
	return !m.Premier
}

// SupportsImage reports whether the model accepts image input.
func (m Model) SupportsImage() bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// Credentials are the DeepAI account credentials saved by the desktop app.
type Credentials struct {
	APIKey   string `json:"apiKey"`
	Username string `json:"username"`
	IsPro    bool   `json:"isPro"`
}

// CheckIn is the check-in challenge in the /theos response (issued on both success and rejection).
//
// The desktop app caches it, and the next /theos request carries X-DeepAI-Code-Check-In: <nonce>,
// Re check-in at half of the seconds period.
type CheckIn struct {
	Nonce   string `json:"nonce"`
	Seconds int    `json:"seconds"`
}

// Valid reports whether the check-in challenge can be sent back (nonce is 32 lowercase hex chars, seconds is positive).
func (c *CheckIn) Valid() bool {
	if c == nil || c.Seconds <= 0 || len(c.Nonce) != 32 {
		return false
	}
	for _, r := range c.Nonce {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Allowance is the quota information returned by /theos.
type Allowance struct {
	Enabled     bool     `json:"enabled"`
	Balance     string   `json:"balance,omitempty"`
	Granted     string   `json:"granted,omitempty"`
	RefreshAt   string   `json:"refresh_at,omitempty"`
	LaneVersion int      `json:"lane_version,omitempty"`
	CheckIn     *CheckIn `json:"check_in,omitempty"`
	CheckedAt   int64    `json:"checked_at,omitempty"`
}

// Clone returns a copy of the quota information (nil-safe).
func (a *Allowance) Clone() *Allowance {
	if a == nil {
		return nil
	}
	cp := *a
	if a.CheckIn != nil {
		ci := *a.CheckIn
		cp.CheckIn = &ci
	}
	return &cp
}

// UsageSummary is the return of /get_usage_summary (fields retained as needed).
type UsageSummary struct {
	IsPro             bool    `json:"is_pro"`
	Plan              string  `json:"plan"`
	IsAuthenticated   bool    `json:"is_authenticated"`
	CreditsRemaining  float64 `json:"credits_remaining"`
	TotalOverageCost  float64 `json:"total_overage_cost"`
	HasOverages       bool    `json:"has_overages"`
	PeriodDescription string  `json:"period_description"`
}

// Account is a DeepAI upstream account (= one API Key).
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"` // desktop / manual / oauth / register
	APIKey   string `json:"api_key"`
	Username string `json:"username"`
	IsPro    bool   `json:"is_pro"`

	// Web account information (written by the registration flow; used for re-runs and auditing).
	Email         string `json:"email,omitempty"`
	Password      string `json:"password,omitempty"`
	WebSessionID  string `json:"session_id,omitempty"`
	CSRFToken     string `json:"csrf_token,omitempty"`
	UserPK        int64  `json:"user_pk,omitempty"`
	Plan          string `json:"plan,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	RegisteredAt  int64  `json:"registered_at,omitempty"`
	LastVerified  int64  `json:"last_verified,omitempty"`

	// Group is the account pool group (empty string is equivalent to default).
	Group string `json:"group,omitempty"`

	// Proxy is the upstream proxy used by this account for inference (full URL, stored locally only).
	Proxy       string `json:"proxy,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`

	// Browser fingerprint snapshot (written at registration). The inference side carries the same set of headers; otherwise "registering with Chrome,
	// inferring with an empty UA" is itself an identifiable anomaly signal.
	UserAgent      string `json:"user_agent,omitempty"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	SecCHUA        string `json:"sec_ch_ua,omitempty"`
	Platform       string `json:"platform,omitempty"`

	// MachineID is the raw machine fingerprint value dedicated to this account (UUID).
	//
	// The upstream grants free quota once per day based on X-DeepAI-Code-Machine; when multiple accounts share
	// one fingerprint, only the first one can get the quota. One is generated per account at registration.
	// An empty string means fall back to the process-level MachineID() (legacy accounts + desktop imports).
	MachineID string `json:"machine_id,omitempty"`

	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"` // ok / error / blocked / unknown
	Note     string `json:"note"`
	LastUsed int64  `json:"last_used"`
	LastErr  string `json:"last_err"`
	Failures int    `json:"failures"`

	// LastErrAt is the time the account was last marked as error (Unix seconds, 0 = early field missing).
	//
	// Rationale for its existence: status=error is the result of the **previous request**, not a permanent property of the account.
	// Without a timestamp, a single probabilistic EOF / 429 will make the account show "unhealthy" forever,
	// with the badge stuck at 1/16 for a long time while the account pool is actually working normally (see docs/changes.md §45).
	// With it, "whether the error is recent enough" can be used as a health criterion.
	LastErrAt int64 `json:"last_err_at,omitempty"`

	// ProbeAfter is the next allowed time for quota probing (Unix seconds, 0 = no cooldown).
	//
	// After a probe failure, back off per attempt: without this field, the background quota guard re-probes the same batch of
	// failed accounts every 5 minutes (another source of write amplification beyond docs/performance.md §14),
	// and failures often come in batches (egress outage / upstream flakiness).
	ProbeAfter int64 `json:"probe_after,omitempty"`
	// ProbeFailures is the number of consecutive probe failures (reset to zero on a successful probe).
	ProbeFailures int `json:"probe_failures,omitempty"`

	// Theos is the most recent /theos probe result (display only, not used for scheduling).
	Theos *Allowance `json:"theos,omitempty"`

	// QuotaUntil is the recovery time for "daily quota exhausted" (Unix seconds, 0 = not in this state).
	//
	// Source: inference returns 403 theos_exhausted (paid model / negative balance). It differs from
	// Blocked: no manual intervention is needed, and it automatically returns to available after this time; before then
	// scheduling skips it, avoiding a wasted 403 on every request.
	QuotaUntil int64 `json:"quota_until,omitempty"`
}

// Clone returns a deep copy of the account (including the Theos pointer).
//
// The account pool holds the only mutable instance; externally it always hands out Clone, preventing callers from
// modifying fields outside the lock and racing with scheduling / background probing.
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Theos != nil {
		theos := *a.Theos
		cp.Theos = &theos
	}
	return &cp
}

// MergeFrom merges the runtime state and missing identity fields of an existing account ex into a.
//
// Used for the "same account re-imported / re-registered" scenario, with two policies:
// - Runtime state (Failures / Status / LastErr / LastUsed / Theos) follows ex:
// a single import should not wipe failure counts and quota caches;
// - Identity fields follow a's non-empty values, falling back to ex only when empty: re-importing should not clear
// information already "earned" such as machine fingerprint, proxy, and fingerprint snapshot (clearing them would break
// the upstream quota attribution, or make registration/inference features inconsistent).
func (a *Account) MergeFrom(ex *Account) {
	if a == nil || ex == nil {
		return
	}
	a.ID = ex.ID
	a.Failures = ex.Failures
	a.LastUsed = ex.LastUsed
	a.Status = ex.Status
	a.LastErr = ex.LastErr
	a.Theos = ex.Theos
	a.ProbeAfter = ex.ProbeAfter
	a.ProbeFailures = ex.ProbeFailures

	fill := []struct {
		dst *string
		src string
	}{
		{&a.Name, ex.Name},
		{&a.Email, ex.Email},
		{&a.Password, ex.Password},
		{&a.Group, ex.Group},
		{&a.Proxy, ex.Proxy},
		{&a.Fingerprint, ex.Fingerprint},
		{&a.MachineID, ex.MachineID},
		{&a.UserAgent, ex.UserAgent},
		{&a.AcceptLanguage, ex.AcceptLanguage},
		{&a.SecCHUA, ex.SecCHUA},
		{&a.Platform, ex.Platform},
	}
	for _, f := range fill {
		if *f.dst == "" {
			*f.dst = f.src
		}
	}
	if a.RegisteredAt == 0 {
		a.RegisteredAt = ex.RegisteredAt
	}
}

// GroupName returns the normalized group name.
func (a *Account) GroupName() string {
	if a == nil {
		return "default"
	}
	g := strings.TrimSpace(a.Group)
	if g == "" {
		return "default"
	}
	return g
}

// Registration is the account information produced by the web registration flow.
type Registration struct {
	Email          string
	Password       string
	Username       string
	APIKey         string
	SessionID      string
	CSRFToken      string
	UserPK         int64
	Plan           string
	EmailVerified  bool
	Group          string
	Proxy          string
	Fingerprint    string
	MachineID      string
	UserAgent      string
	AcceptLanguage string
	SecCHUA        string
	Platform       string
	Source         string
}

// KeyID returns a stable short identifier for the API Key (used for deduplication and logging, without leaking plaintext).
func (a *Account) KeyID() string { return KeyFingerprint(a.APIKey) }

// SessionID returns the session identifier used for display/troubleshooting.
func (a *Account) SessionID() string { return ShortHash(a.APIKey) }

// DefaultModelEntry is a built-in fallback entry used when the catalog is unavailable.
func DefaultModelEntry() Model {
	return Model{
		ID:            DefaultModel,
		Name:          "GLM 5.3 Flash",
		Reasoning:     true,
		Input:         []string{"text", "image"},
		ContextWindow: 1048576,
		MaxTokens:     131072,
	}
}
