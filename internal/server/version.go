package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/logx"
)

// --- Version and update check ---
//
// The console top shows the current version; clicking it asks "is there a new version". The query is done on the **server side**:
// - The frontend CSP is connect-src 'self', which already disallows direct connections to api.github.com;
// - A task that only issued one request doesn't need every machine to hit GitHub separately (there are rate limits).
//
// Cache for 6 hours: this lets upgrade prompts appear in time without turning console refreshes into a load test against GitHub.
// Failure to fetch (offline / blocked / private repo) is not treated as an error path: return the current version + an error field,
// the frontend only shows the version number and pops up no error.

// updateFeedURL is the API address of the "latest release".
//
// A variable rather than a constant: tests need to point it at httptest, and offline environments can change it to a local mirror.
var updateFeedURL = "https://api.github.com/repos/2951461586/DeepAICode2API/releases/latest"

const (
	// updateCacheTTL is the validity period of the query result.
	updateCacheTTL = 6 * time.Hour
	// updateTimeout is the budget for a single upstream query.
	updateTimeout = 8 * time.Second
)

// versionInfo is the response of /admin/api/version.
type versionInfo struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	// Cached indicates this result came from cache (for debugging).
	Cached bool `json:"cached,omitempty"`
	// A non-empty Error indicates the query failed; the frontend can just ignore it and still show the version number.
	Error string `json:"error,omitempty"`
}

// SetVersion records the build version (called by main with the version injected via -ldflags).
func (s *Server) SetVersion(v string) {
	s.Version = strings.TrimSpace(v)
}

// currentVersion returns the externally displayed version number (empty values are normalized to dev).
func (s *Server) currentVersion() string {
	if v := strings.TrimSpace(s.Version); v != "" {
		return v
	}
	return "dev"
}

// handleAdminVersion returns the comparison result between the current version and the latest release.
func (s *Server) handleAdminVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.versionStatus(r.Context()))
}

// versionStatus queries the latest release with caching.
//
// When the cache is cold, concurrent requests are merged: only one goroutine actually performs the query, and the rest wait for its result. Otherwise, simultaneously
// opening multiple consoles / refreshing multiple tabs would turn "one GitHub request with an 8s budget" into N requests.
func (s *Server) versionStatus(ctx context.Context) versionInfo {
	current := s.currentVersion()

	for {
		s.versionMu.Lock()
		if s.versionCache != nil && time.Since(s.versionAt) < updateCacheTTL {
			cached := *s.versionCache
			cached.Cached = true
			cached.Current = current
			s.versionMu.Unlock()
			return cached
		}
		if wait := s.versionInFlight; wait != nil {
			s.versionMu.Unlock()
			select {
			case <-wait:
				continue // The result has already been written to the cache; go back to the top of the loop and read it directly.
			case <-ctx.Done():
				return versionInfo{Current: current, Error: ctx.Err().Error()}
			}
		}
		done := make(chan struct{})
		s.versionInFlight = done
		s.versionMu.Unlock()

		// The query must not be tied to the request context of 'the first person to open the console': once they close the tab,
		// the in-flight query is aborted, and the failed result is cached for 6 hours-after that everyone will see
		// 'Unable to check for updates'. Remove the cancellation signal; the budget is covered by updateTimeout in fetchLatestRelease as a fallback.
		info := s.queryLatestVersion(context.WithoutCancel(ctx), current)
		// Failures are also written to cache: otherwise, in an offline environment, every console refresh would wait 8s for the timeout.
		s.storeVersionCache(info)

		s.versionMu.Lock()
		s.versionInFlight = nil
		s.versionMu.Unlock()
		close(done)
		return info
	}
}

// queryLatestVersion actually hits GitHub (the caller is responsible for caching and concurrent merging).
func (s *Server) queryLatestVersion(ctx context.Context, current string) versionInfo {
	info := versionInfo{Current: current}
	// Development builds (no tag) skip comparison: the comparison would always yield 'new version available', creating only noise.
	if current == "dev" {
		info.Error = "开发构建未注入版本号，不检查更新"
		return info
	}
	latest, err := fetchLatestRelease(ctx)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Latest = latest.Tag
	info.ReleaseURL = latest.URL
	info.PublishedAt = latest.PublishedAt
	info.UpdateAvailable = versionNewer(latest.Tag, current)
	logVersionCheck(info)
	return info
}

func (s *Server) storeVersionCache(info versionInfo) {
	s.versionMu.Lock()
	s.versionCache = &info
	s.versionAt = time.Now()
	s.versionMu.Unlock()
}

// release is the field used in the GitHub release.
type release struct {
	Tag         string `json:"tag_name"`
	URL         string `json:"html_url"`
	PublishedAt string `json:"published_at"`
}

// fetchLatestRelease fetches the latest release (read-only public endpoint, no token required).
func fetchLatestRelease(ctx context.Context) (*release, error) {
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateFeedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "deepaicode2api-update-check")
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &updateError{status: resp.StatusCode}
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if strings.TrimSpace(rel.Tag) == "" {
		return nil, &updateError{status: resp.StatusCode, note: "release 缺少 tag_name"}
	}
	return &rel, nil
}

// updateHTTPClient is the client used for update checks.
//
// Do not reuse the account pool client: the latter carries proxy and browser fingerprint headers, whereas what is needed here is 'the machine where the console runs'
// direct connection'; even if a proxy pool is configured, it should not be used to hit GitHub.
var updateHTTPClient = &http.Client{Timeout: updateTimeout}

// updateError describes a failed query (with status code, for logging and frontend display).
type updateError struct {
	status int
	note   string
}

func (e *updateError) Error() string {
	if e.note != "" {
		return e.note
	}
	return "查询最新版本失败：HTTP " + strconv.Itoa(e.status)
}

// versionNewer reports whether latest is newer than current (lenient semver: compares only major.minor.patch).
//
// current may be a non-canonical value such as "dev" / "docker" / "v0.1.0-3-gabc":
// when no numbers can be parsed, always treat it as 'not newer' to avoid false upgrade reports.
func versionNewer(latest, current string) bool {
	l, ok1 := parseSemver(latest)
	c, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// parseSemver extracts three numbers from v1.2.3 / 1.2.3-rc1 / v1.2.
func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// Strip prerelease / build suffix: 1.2.3-rc1+meta → 1.2.3
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return [3]int{}, false
	}
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// logVersionCheck logs the result of an update check (avoid writing logs in the handler every time).
func logVersionCheck(info versionInfo) {
	switch {
	case info.UpdateAvailable:
		logx.Info("update available", "current", info.Current, "latest", info.Latest)
	case info.Error != "":
		logx.Warn("update check failed", "current", info.Current, "error", info.Error)
	}
}
