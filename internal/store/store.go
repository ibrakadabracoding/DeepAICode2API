// Package store persists gateway state (accounts / keys / settings / logs).
//
// Account selection lives in internal/pool; this package only calls it while
// holding the lock and delivers the result as a snapshot.
package store

import (
	"sync"
	"sync/atomic"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/pool"
)

// APIKey is a call key issued by the gateway.
type APIKey struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Group     string `json:"group,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	Requests  int64  `json:"requests"`
}

// Settings holds runtime-adjustable parameters.
type Settings struct {
	ModelAliases   map[string]string `json:"model_aliases"`
	RequestTimeout int               `json:"request_timeout_sec"`
	MaxRetries     int               `json:"max_retries"`
	LogKeep        int               `json:"log_keep"`
	CORSOrigin     string            `json:"cors_origin"`
	AdminPassword  string            `json:"admin_password"`
	FallbackModel  string            `json:"fallback_model"`
	// StrictModel disables "fall back when it cannot be resolved": when a model
	// name is not in the catalog, return 404 instead of silently switching to the
	// default model. Off by default to keep it working out of the box.
	StrictModel bool `json:"strict_model"`

	// Lane is the detected Theos lane version (empty = use the compile-time default).
	//
	// When the upstream bumps the lane, the old value always returns 426; on
	// startup the gateway probes /models once using account credentials and writes
	// the result here, so upgrades need no code change.
	Lane string `json:"lane,omitempty"`

	// LaneBuild is the compile-time lane (deepai.LaneVersion) when Lane was written.
	//
	// It distinguishes "the result detected by this build" from "leftovers from an
	// older version": normally lanes only move forward, but the upstream may keep
	// only the old lane (or the detected result may be older than the compiled
	// value). CurrentLane trusts Lane unconditionally only when LaneBuild matches
	// the current compiled value; otherwise it still takes the newer value via
	// max(Lane, compiled value), so an upgrade does not keep running an old lane.
	LaneBuild string `json:"lane_build,omitempty"`

	// RateLimitRPS / RateLimitBurst are the token bucket for each key (0 = no rate limit).
	// When RateLimitBurst is 0, capacity is max(2, RPS).
	RateLimitRPS   float64 `json:"rate_limit_rps,omitempty"`
	RateLimitBurst int     `json:"rate_limit_burst,omitempty"`
	// KeyConcurrency is the in-flight concurrency cap for each key (0 = unlimited).
	KeyConcurrency int `json:"key_concurrency,omitempty"`

	// CheckinBlockMS is the wait budget for synchronous check-in before inference
	// (milliseconds, 0 = default 10000).
	//
	// Measured in production on 2026-11-13: check-in blocked inference 292 times,
	// averaging 4.8s; lowering this reduces first-packet latency, at the cost of
	// the request possibly going out with an old nonce (the previous round already
	// reported back, and the next round continues the catch-up), which does not
	// make inference fail.
	CheckinBlockMS int `json:"checkin_block_ms,omitempty"`

	// PresetDesktopShape makes the gateway preset the desktop shape for chat
	// requests that lack a system message, instead of first taking a 400 and then
	// retrying.
	//
	// Only applies to callers already confirmed to need that shape (remembered per
	// API Key for 10 minutes); the first time still follows the learning path of
	// "pass through → 400 → add shape and retry".
	PresetDesktopShape bool `json:"preset_desktop_shape,omitempty"`
}

// LogEntry is one request log entry (metadata only; no conversation content is stored).
type LogEntry struct {
	Time      int64  `json:"time"`
	Key       string `json:"key"`
	Account   string `json:"account"`
	Model     string `json:"model"`
	Endpoint  string `json:"endpoint"`
	Stream    bool   `json:"stream"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	PromptLen int    `json:"prompt_len"`
	OutputLen int    `json:"output_len"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	// CachedTokens is the number of prefix-cache-hit tokens reported by the
	// upstream (0 when the field is absent).
	CachedTokens int     `json:"cached_tokens,omitempty"`
	Cost         float64 `json:"cost"`
	Err          string  `json:"err,omitempty"`
}

// Stats holds cumulative statistics.
type Stats struct {
	TotalRequests  int64 `json:"total_requests"`
	FailedRequests int64 `json:"failed_requests"`
	TotalCharsOut  int64 `json:"total_chars_out"`
	TotalTokensIn  int64 `json:"total_tokens_in"`
	TotalTokensOut int64 `json:"total_tokens_out"`
	// TotalCachedTokens is the total prompt tokens for which the upstream reported
	// a "prefix cache hit", used to compute the cache hit rate (cached / prompt).
	//
	// Note that it is a **cumulative** value: combining it with DayTokensIn
	// (current day) to compute a ratio is wrong (it yields a number that is
	// neither cumulative nor daily). The console therefore also stores
	// DayCachedTokens.
	TotalCachedTokens int64   `json:"total_cached_tokens,omitempty"`
	TotalCost         float64 `json:"total_cost"`
	// Day / DayTokensIn / DayTokensOut / DayCachedTokens are the current day's usage
	// "by local date".
	//
	// Record resets them automatically when the day changes (no scheduled job
	// needed); Date uses YYYY-MM-DD.
	Day             string `json:"day,omitempty"`
	DayTokensIn     int64  `json:"day_tokens_in,omitempty"`
	DayTokensOut    int64  `json:"day_tokens_out,omitempty"`
	DayCachedTokens int64  `json:"day_cached_tokens,omitempty"`
	// StartedAt is when statistics collection started (first launch); ResetAt is
	// the time of the most recent manual reset.
	//
	// They are kept separate: the console needs to explain "fewer stats than logs"
	// - a reset only clears counters, and logs are retained by design. With only
	// StartedAt, it would turn into a "launch time" after a reset, which muddies
	// the semantics.
	StartedAt int64 `json:"started_at"`
	ResetAt   int64 `json:"reset_at,omitempty"`
}

// State is the entire persisted state.
//
// Logs and Stats are deliberately not written to the main file: they change with
// every request, and keeping them there would permanently defeat Save's
// "skip if content unchanged" check. Logs go to the append-only
// <base>-logs.jsonl, stats go to <base>-stats.json.
type State struct {
	Accounts      []*deepai.Account `json:"accounts"`
	Keys          []*APIKey         `json:"keys"`
	Settings      Settings          `json:"settings"`
	Stats         Stats             `json:"-"`
	Logs          []LogEntry        `json:"-"`
	Models        []deepai.Model    `json:"models"`
	DefaultModel  string            `json:"default_model"`
	ModelsUpdated int64             `json:"models_updated"`
	MachineID     string            `json:"machine_id"`
}

// legacyState is the pre-split state file format (including logs / stats), used
// only for read migration.
type legacyState struct {
	Accounts      []*deepai.Account `json:"accounts"`
	Keys          []*APIKey         `json:"keys"`
	Settings      Settings          `json:"settings"`
	Stats         Stats             `json:"stats"`
	Logs          []LogEntry        `json:"logs"`
	Models        []deepai.Model    `json:"models"`
	DefaultModel  string            `json:"default_model"`
	ModelsUpdated int64             `json:"models_updated"`
	MachineID     string            `json:"machine_id"`
}

// Store is a locked state container.
type Store struct {
	mu    sync.RWMutex
	path  string
	st    *State
	sched *pool.Scheduler

	// statsPath / logsPath are side files derived from the main path.
	statsPath string
	logsPath  string
	logs      *logStore

	// masterKey, when non-empty, encrypts the whole state file (AES-256-GCM).
	masterKey []byte
	// encrypted records whether the file was encrypted on the last read/write (for startup logs / console hints).
	encrypted bool
	// lastSaved is the plaintext snapshot from the last save, used to skip redundant writes with no changes.
	lastSaved []byte
	// statsDirty marks that statistics changed (a separate small file that changes on every request).
	statsDirty bool

	// savesTotal / savedBytes are the actual write count and plaintext byte count for the main file.
	//
	// After the account pool grew to 653 accounts, every request changes account
	// runtime state, easily defeating "skip if content unchanged" and degrading to
	// a full rewrite of about 950KB every 30s. These two counters (not persisted)
	// are used to directly observe write amplification during load tests rather
	// than relying on guesswork.
	savesTotal atomic.Int64
	savedBytes atomic.Int64

	// saveMu serializes persistence.
	//
	// Save is called by multiple goroutines: the background 30s periodic save, every
	// admin operation, and the per-account quota re-probe goroutines started after a
	// batch pool insert. Without it (measured):
	//   - two Saves share <path>.tmp and truncate each other, possibly leaving a
	//     corrupt file after rename; on Windows it is even more direct: the call
	//     fails with "file is in use by another process";
	//   - the earlier rename is overwritten by the later one, so disk may be older
	//     than memory;
	//   - lastSaved's backing array has a read/write race (reproducible with -race).
	saveMu sync.Mutex
}

// DefaultSettings returns the default settings.
func DefaultSettings() Settings {
	return Settings{
		ModelAliases:   map[string]string{},
		RequestTimeout: 600,
		MaxRetries:     3,
		LogKeep:        500,
		// By default only allow local origins; configure explicitly in the console
		// for LAN access.
		CORSOrigin:    "",
		FallbackModel: "",
	}
}
