package server

import (
	"deepaicode2api/internal/server/web"
	"regexp"
	"strings"
	"testing"
)

// bareRefreshRe matches a bare refresh( call that is not a member access.
//
// It matches the identifier alone: `refreshLogs(` does not match (preceded by a letter),
// and neither does `obj.refresh(` (preceded by a dot); both are legal in app.js.
var bareRefreshRe = regexp.MustCompile(`(^|[^\w.$])refresh\s*\(`)

// TestConsoleModulesUseInjectedReload guards the convention that "refresh may only be injected via setReload".
//
// Background: after app.js was split, the refresh function stayed in app.js, and lower-level
// modules receive `reload` via `setReload(fn)`. During the split, actions.js and render/keys.js
// still had 4 bare `refresh()` calls: accountOp / keyOp / finishCheckin / "rebind group". They
// don't fail at load time; they throw `refresh is not a function` only at the "wrap-up after a
// successful operation" moment - and the same.catch catches it, reporting "failed: refresh is
// not a function", which looks like an API failure.
//
// A browser-only failure wouldn't block go test, so this is enforced as a static contract:
// actions.js and render/*.js must not contain a bare refresh(.
func TestConsoleModulesUseInjectedReload(t *testing.T) {
	checked := 0
	for _, name := range web.ScriptNames() {
		if name != "js/actions.js" && !strings.HasPrefix(name, "js/render/") {
			continue
		}
		checked++
		raw, err := web.Read(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		src := stripJSComments(string(raw))
		for _, m := range bareRefreshRe.FindAllStringIndex(src, -1) {
			line := 1 + strings.Count(src[:m[0]], "\n")
			t.Errorf("%s:%d 调用了裸 refresh()；下层模块必须用注入的 reload()（见 actions.js setReload）", name, line)
		}
	}
	// A directory-structure change (e.g. render/ being merged) would silently disable this contract.
	if checked < 4 {
		t.Fatalf("只检查到 %d 个模块（actions.js + render/*），断言可能已失效", checked)
	}
}

// TestConsoleActionsInjectsReload confirms the injection point still holds in actions.js:
// there is a setReload export, a default no-op implementation, and wrap-up uses reload().
func TestConsoleActionsInjectsReload(t *testing.T) {
	raw, err := web.Read("web/js/actions.js")
	if err != nil {
		t.Fatalf("读取 web/js/actions.js 失败: %v", err)
	}
	src := string(raw)
	for _, want := range []string{"export function setReload", "return reload();"} {
		if !strings.Contains(src, want) {
			t.Errorf("actions.js 里找不到 %q：刷新收尾必须继续走注入的 reload", want)
		}
	}
	// The default value must be a function: otherwise calls before setReload throw a TypeError.
	if !strings.Contains(src, "let reload = function") {
		t.Error("actions.js 的 reload 默认值必须是可以直接调用的空实现")
	}
}

// TestStripJSComments ensures the stripper itself doesn't eat real code or treat strings as comments.
func TestStripJSComments(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"行注释", "a(); // refresh()\nb();", "a(); \nb();"},
		{"块注释", "a(); /* refresh()\nstill */ b();", "a(); \n b();"},
		{"字符串里的 //", `var u = "http://x"; refresh();`, `var u = "http://x"; refresh();`},
		{"字符串里的转义", `var s = "a\"b"; refresh();`, `var s = "a\"b"; refresh();`},
	}
	for _, c := range cases {
		if got := stripJSComments(c.in); got != c.want {
			t.Errorf("%s: stripJSComments(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}
