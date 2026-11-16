package server

import (
	"net/http"
	"strings"

	"deepaicode2api/internal/server/web"
)

// This file only contains the route whitelist: maps URLs to embedded asset names.
//
// The assets themselves, ETag, Content-Type, and CSP all live in the internal/server/web package;
// It remains in the server package because the whitelist is part of the routing contract (which URLs are valid),
// and is unrelated to asset content.

// serveConsole serves the console single page.
func serveConsole(w http.ResponseWriter, r *http.Request) {
	web.ServeAsset(w, r, "index.html")
}

// serveLogin serves the admin passphrase login page (/login).
//
// Uses the same static assets and response headers as the console; the page does only one thing: verify the passphrase once, then redirect to /.
func serveLogin(w http.ResponseWriter, r *http.Request) {
	web.ServeAsset(w, r, "login.html")
}

// handleConsoleAsset serves /style.css, /favicon.ico, /apple-touch-icon.png, and /js/<name>.js.
//
// Use a whitelist instead of path concatenation: the console has only these two entry points, so write the allowed names as explicit rules,
// and there is no need to discuss path traversal on the embed FS.
func (s *Server) handleConsoleAsset(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	var name string
	switch {
	case p == "style.css":
		name = "style.css"
	case p == "favicon.ico":
		// Fixed paths requested automatically by browsers; tab/bookmark icon (multi-size ICO, see docs/assets)
		name = "favicon.ico"
	case p == "apple-touch-icon.png":
		// iOS home screen icons only accept PNG (ICO is ignored), so a separate 180×180 image is provided
		name = "apple-touch-icon.png"
	case strings.HasPrefix(p, "js/render/"):
		// Only flat filenames one level deep are allowed under render/
		rest := strings.TrimPrefix(p, "js/render/")
		if rest == "" || strings.ContainsAny(rest, "/\\") || strings.Contains(rest, "..") {
			http.NotFound(w, r)
			return
		}
		name = "js/render/" + rest
	case strings.HasPrefix(p, "js/i18n/"):
		// i18n/ follows the same rule as render/: only flat filenames one level deep are allowed
		rest := strings.TrimPrefix(p, "js/i18n/")
		if rest == "" || strings.ContainsAny(rest, "/\\") || strings.Contains(rest, "..") {
			http.NotFound(w, r)
			return
		}
		name = "js/i18n/" + rest
	case strings.HasPrefix(p, "js/"):
		rest := strings.TrimPrefix(p, "js/")
		// Only flat filenames are allowed: no subdirectories, backslashes, or ..
		if rest == "" || strings.ContainsAny(rest, "/\\") || strings.Contains(rest, "..") {
			http.NotFound(w, r)
			return
		}
		name = "js/" + rest
	default:
		http.NotFound(w, r)
		return
	}
	web.ServeAsset(w, r, name)
}
