package server

import (
	"bytes"
	"deepaicode2api/internal/server/web"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleFaviconServed guards the fixed path browsers request automatically for the
// "tab icon".
//
// There used to be no favicon: every time the console / login page was opened, the
// browser fired a 404 at /favicon.ico (which even went into the access log). The icon
// reuses the DeepAI Code desktop app icon (see docs/assets), so this checks not only for
// 200 but that the content really is ICO / PNG - so a change cannot slip in an empty file.
func TestConsoleFaviconServed(t *testing.T) {
	s, key := newTestServer(t, nil)

	cases := []struct {
		path  string
		ctype string
		magic []byte
	}{
		// ICO header: reserved=0, type=1, count>0
		{"/favicon.ico", "image/x-icon", []byte{0x00, 0x00, 0x01, 0x00}},
		{"/apple-touch-icon.png", "image/png", []byte("\x89PNG\r\n\x1a\n")},
	}
	for _, c := range cases {
		rec := do(t, s, http.MethodGet, c.path, key, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d，期望 200", c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != c.ctype {
			t.Errorf("GET %s 的 Content-Type = %q，期望 %q", c.path, ct, c.ctype)
		}
		if body := rec.Body.Bytes(); !bytes.HasPrefix(body, c.magic) {
			t.Errorf("GET %s 不是合法的 %s（前 8 字节 %x）", c.path, c.ctype, body[:min(8, len(body))])
		}
		// Static assets use ETag revalidation: they follow binary updates, but browsers should
		// not re-download everything on every load.
		if rec.Header().Get("ETag") == "" {
			t.Errorf("GET %s 缺少 ETag", c.path)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s 的 Cache-Control = %q，期望 no-cache", c.path, cc)
		}
		if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
			t.Errorf("GET %s 缺少 nosniff", c.path)
		}

		// Conditional requests must hit 304 (the icon updates with releases, but need not be
		// re-downloaded every time).
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.RemoteAddr = "127.0.0.1:34567"
		req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
		rec2 := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec2, req)
		if rec2.Code != http.StatusNotModified {
			t.Errorf("GET %s 带 If-None-Match 应回 304，实际 %d", c.path, rec2.Code)
		}
	}
}

// TestConsolePagesReferenceFavicon verifies that both pages declare the icon.
//
// Embedding the file in the binary is not enough: if the page does not include
// <link rel="icon">, the browser still guesses the default path, and "add to home screen"
// on mobile gets no icon either.
func TestConsolePagesReferenceFavicon(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/login.html"} {
		page := string(web.MustRead(name))
		for _, want := range []string{
			`<link rel="icon" href="/favicon.ico" sizes="any">`,
			`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s 缺少图标声明：%s", name, want)
			}
		}
	}
}

// TestConsoleFaviconMatchesDocsAsset guards the rule that the icon has a single source of truth.
//
// favicon.ico in the console is a copy of docs/assets/logo.ico (embed can only read files
// inside the package). Forgetting to sync when the logo changes produces "README shows the
// new image, the tab still shows the old one" - nearly impossible to spot by eye, yet
// genuinely two sets of brand assets. This compares bytes directly.
func TestConsoleFaviconMatchesDocsAsset(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "docs", "assets", "logo.ico"))
	if err != nil {
		t.Fatalf("读取 docs/assets/logo.ico 失败: %v", err)
	}
	got := web.MustRead("web/favicon.ico")
	if !bytes.Equal(got, want) {
		t.Fatalf("web/favicon.ico 与 docs/assets/logo.ico 不一致（%d vs %d 字节）："+
			"换图标后要同时更新两份，见 docs/changes.md §58", len(got), len(want))
	}
}

// TestStaticAssetsSkipAccessLog verifies that icons are not written to the access log.
//
// Browsers automatically request /favicon.ico (and Apple's touch icon) every time a page
// is opened; like /style.css and /js/*, they are purely static assets. Logging them only
// fills the access lines with "half icons" noise and also inflates the 2xx count in /metrics.
func TestStaticAssetsSkipAccessLog(t *testing.T) {
	for _, p := range []string{"/favicon.ico", "/apple-touch-icon.png", "/style.css", "/js/app.js", "/ping"} {
		if !isStaticAsset(p) {
			t.Errorf("%s 应视为静态资源（不写访问日志）", p)
		}
	}
	for _, p := range []string{"/", "/login", "/admin/api/logs", "/v1/models"} {
		if isStaticAsset(p) {
			t.Errorf("%s 不是静态资源，不应被跳过", p)
		}
	}
}
