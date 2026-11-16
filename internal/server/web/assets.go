// Package web is the console static asset package: it embeds HTML / CSS / ES modules / icons into the binary,
// and is responsible for content fingerprint (ETag), Content-Type, CSP, and other response headers.
//
// These assets were previously an embed.FS + 200 lines of serving code inside internal/server, mixed together with routing and
// protocol implementation. The assets are fixed at compile time with no runtime dependencies, so after splitting into its own package:
// - internal/server only needs `web.ServeAsset(w, r, name)`;
// - validation of asset path legality (allowlist) stays in the routing layer; embed is not expanded for display;
// - contract tests (console_*.go) can assert directly against the web package.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"

	"deepaicode2api/internal/httpx"
)

// The console is a static package of "markup + styles + a set of ES modules", all embedded into the binary:
// no external dependencies, no build step, usable offline.
//
//go:embed index.html login.html style.css favicon.ico apple-touch-icon.png js/*.js js/render/*.js js/i18n/*.js
var FS embed.FS

// HTML is the console page markup (no inline scripts or styles).
//
// It is kept as a variable so contract tests can assert directly on the markup content (see internal/server/console_test.go).
var HTML = MustRead("index.html")

// LoginHTML is the /login page markup (the login page has its own DOM, different from index.html).
var LoginHTML = MustRead("login.html")

// CSS is the console stylesheet (already extracted from index.html into a standalone asset).
var CSS = MustRead("style.css")

// assetPrefix is the path prefix left over from old call sites (historically assets lived under internal/server/web/,
// and asset names were written as "web/js/app.js"). Read supports both prefixed and unprefixed forms.
const assetPrefix = "web/"

// Read reads an embedded asset.
func Read(name string) ([]byte, error) {
	return FS.ReadFile(strings.TrimPrefix(name, assetPrefix))
}

// MustRead reads an embedded asset and panics if missing (compile-time assets, so missing means a build error).
func MustRead(name string) []byte {
	b, err := Read(name)
	if err != nil {
		panic("控制台资源缺失: " + name + ": " + err.Error())
	}
	return b
}

// ScriptNames returns the asset names of all embedded scripts (sorted, with the "web/" prefix).
//
// Exposed separately because contract tests need to validate id references grouped by "which page this script belongs to"
// (login.js's #loginPw is in login.html, not in index.html).
func ScriptNames() []string {
	var names []string
	for _, dir := range []string{"js", "js/render", "js/i18n"} {
		entries, err := FS.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
				continue
			}
			names = append(names, dir+"/"+e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Scripts returns the concatenation of all modular scripts, for contract tests to make "string existence" assertions.
//
// The frontend has been split into multiple ES modules: when asserting that a function name/selector exists, all scripts must be scanned,
// otherwise the test will silently stop working after the split (which is more dangerous than testing the wrong thing).
func Scripts() string {
	var b strings.Builder
	for _, name := range ScriptNames() {
		if raw, err := Read(name); err == nil {
			b.Write(raw)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// assetType returns the Content-Type of an embedded asset.
func assetType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".ico":
		// The official type is image/vnd.microsoft.icon (RFC 7903), but image/x-icon is
		// the most common spelling in the real world and recognized by all browsers; here it is kept consistent with nginx's default.
		return "image/x-icon"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// CSP is the Content Security Policy for the console page.
//
// The console carries the admin password, making it the page in the entire system most worth adding CSP to. All resources are same-origin,
// with no external references, so no directive needs to be relaxed:
// - script-src 'self': inline scripts are strictly forbidden (the theme bootstrap script is also made an external file);
// - style-src 'self': inline style attributes have been removed from the markup (uniformly replaced with named utility classes);
// - connect-src 'self': the console only calls its own /admin/api/*;
// - frame-ancestors 'none' + form-action 'self': prevents clickjacking and outbound form submissions.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; font-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// assetETags caches the content fingerprints of embedded assets (assets are fixed at compile time, so computing once is enough).
var assetETags sync.Map // name -> string

// assetETag returns the content ETag of an asset (in the form "abc...").
func assetETag(name string, raw []byte) string {
	if v, ok := assetETags.Load(name); ok {
		return v.(string)
	}
	sum := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	assetETags.Store(name, etag)
	return etag
}

// ServeAsset outputs an embedded asset (page / style / script / icon).
func ServeAsset(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := Read(name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", assetType(name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// The page and scripts are same-origin, and CSP applies to both (there are no inline scripts / styles in the page).
	h.Set("Content-Security-Policy", CSP)

	// Assets are updated together with the binary: HTML is revalidated every time, while JS/CSS hit 304 via ETag.
	// The old implementation always used no-store, causing all 17 first-screen modules to be fully re-downloaded every time.
	etag := assetETag(name, raw)
	h.Set("ETag", etag)
	if strings.HasSuffix(name, ".html") {
		h.Set("Cache-Control", "no-store")
	} else {
		h.Set("Cache-Control", "no-cache")
		if httpx.ETagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	_, _ = w.Write(raw)
}
