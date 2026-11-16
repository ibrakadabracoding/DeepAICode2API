package server

import (
	"deepaicode2api/internal/server/web"
	"path"
	"regexp"
	"strings"
	"testing"
)

// After the console was split into ES modules, "forgot to import" became a failure mode
// with a missing line, zero syntax errors, and zero load errors: an unbound identifier
// only throws ReferenceError at the moment it actually executes, and is usually swallowed
// by the same .catch, disguising itself as an "API failure".
//
// Real regression on 2026-11-11: app.js's import block had only renderAccounts /
// renderKeys / renderSettings, missing ./render/logs.js; the line
// renderLogs(state.logs) in refreshLogs then threw `renderLogs is not defined`, and the
// toast showed "log loading failed: renderLogs is not defined" - while /admin/api/logs
// returned 200. go test originally could not see this kind of breakage at all (file
// present, route present, strings present), so this pins down "cross-module identifiers
// must be imported" with a static contract.

// consoleImportRe matches `import { a, b as c } from './x.js'` (including multi-line forms).
var consoleImportRe = regexp.MustCompile(`(?s)import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)

// consoleExportRe matches `export function f` / `export const f` / `export var f` / `export let f`.
var consoleExportRe = regexp.MustCompile(`(?m)^\s*export\s+(?:async\s+)?(?:function|const|let|var|class)\s+([A-Za-z_$][\w$]*)`)

// consoleLocalDeclRe matches this file's own top-level and in-function declarations
// (including `function f`, `var f =`).
var consoleLocalDeclRe = regexp.MustCompile(`(?m)(?:^|[^\w$.])(?:function|const|let|var|class)\s+([A-Za-z_$][\w$]*)`)

// consoleNamedImport is a named import (orig is the export name in the target module,
// local is the name used in this file).
type consoleNamedImport struct {
	orig, local, from string
}

// consoleModule is a static view of a script: source with comments stripped + import table.
type consoleModule struct {
	name    string
	src     string
	imports map[string]consoleNamedImport // local -> import
	locals  map[string]bool               // names declared in this file
	exports map[string]bool               // names exported by this file
}

// loadConsoleModules reads all embedded scripts (comments stripped, line numbers aligned
// with the original).
func loadConsoleModules(t *testing.T) map[string]*consoleModule {
	t.Helper()
	mods := make(map[string]*consoleModule, 16)
	for _, name := range web.ScriptNames() {
		raw, err := web.Read(name)
		if err != nil {
			t.Fatalf("读取脚本 %s: %v", name, err)
		}
		src := stripJSComments(string(raw))
		m := &consoleModule{
			name:    name,
			src:     src,
			imports: map[string]consoleNamedImport{},
			locals:  map[string]bool{},
			exports: map[string]bool{},
		}
		for _, im := range consoleImportRe.FindAllStringSubmatch(src, -1) {
			from := im[2]
			for _, spec := range strings.Split(im[1], ",") {
				spec = strings.TrimSpace(spec)
				if spec == "" {
					continue
				}
				orig, local := spec, spec
				if parts := strings.SplitN(spec, " as ", 2); len(parts) == 2 {
					orig, local = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
				}
				m.imports[local] = consoleNamedImport{orig: orig, local: local, from: from}
			}
		}
		for _, ex := range consoleExportRe.FindAllStringSubmatch(src, -1) {
			m.exports[ex[1]] = true
		}
		for _, d := range consoleLocalDeclRe.FindAllStringSubmatch(src, -1) {
			m.locals[d[1]] = true
		}
		// Parameters and catch bindings also count as available in this file (render functions
		// receive injected callbacks).
		for _, fn := range regexp.MustCompile(`(?s)function\s*[A-Za-z_$\w]*\s*\(([^)]*)\)`).FindAllStringSubmatch(src, -1) {
			for _, p := range strings.Split(fn[1], ",") {
				p = strings.TrimSpace(strings.SplitN(p, "=", 2)[0])
				p = strings.TrimSpace(strings.SplitN(p, ":", 2)[0])
				if p != "" && !strings.ContainsAny(p, "{}[]()") {
					m.locals[p] = true
				}
			}
		}
		for _, cb := range regexp.MustCompile(`(?m)catch\s*\(\s*([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(src, -1) {
			m.locals[cb[1]] = true
		}
		mods[name] = m
	}
	if len(mods) < 10 {
		t.Fatalf("只读到 %d 个脚本（期望 10+），consoleScriptNames 可能已失效", len(mods))
	}
	return mods
}

// TestConsoleModulesImportWhatTheyCall is a static defense against "forgot an import when
// splitting modules":
//
//	Any file that calls a function exported by another module (`renderLogs(...)`) must
//	import it in this file; otherwise a ReferenceError is thrown only when that line
//	runs, and the error message is often rewritten into an "API failure" by the same .catch.
func TestConsoleModulesImportWhatTheyCall(t *testing.T) {
	mods := loadConsoleModules(t)

	// Export name -> module providing it (duplicates mean the split itself is broken;
	// report them separately).
	owner := map[string]string{}
	for name, m := range mods {
		for ex := range m.exports {
			if prev, dup := owner[ex]; dup && prev != name {
				t.Errorf("%s 与 %s 都导出了 %s：重名会让 import 的指向变得不可预测", prev, name, ex)
			}
			owner[ex] = name
		}
	}
	if len(owner) < 20 {
		t.Fatalf("只解析到 %d 个导出名，export 正则可能已失效", len(owner))
	}

	calls := 0
	for _, name := range web.ScriptNames() {
		m := mods[name]
		for symbol := range owner {
			if owner[symbol] == name {
				continue // exported by this file; an unbound call will not throw ReferenceError either
			}
			// Bare call / new: `renderLogs(`, `new Foo(`; a same-named method after a dot does not count.
			call := regexp.MustCompile(`(?:^|[^\w$.])` + regexp.QuoteMeta(symbol) + `\s*\(`)
			idx := call.FindStringIndex(m.src)
			if idx == nil {
				continue
			}
			calls++
			_, imported := m.imports[symbol]
			if imported || m.locals[symbol] {
				continue
			}
			line := 1 + strings.Count(m.src[:idx[1]], "\n")
			t.Errorf("%s:%d 调用了 %s()，但本文件没有 import 它（导出在 %s）。"+
				"ESM 的未绑定标识符不会在加载时报错，只会在执行到这一行时抛 ReferenceError。",
				name, line, symbol, owner[symbol])
		}
	}
	if calls < 20 {
		t.Fatalf("只匹配到 %d 处跨模块调用，扫描可能已失效", calls)
	}
}

// TestConsoleImportsResolve validates where imports land: the path must exist, and the
// exported name must really be in the target module. The two most common slips when
// splitting - writing the path as./render/log.js (missing "s") or misspelling a name -
// fail here directly instead of becoming a 404 / undefined in the browser.
func TestConsoleImportsResolve(t *testing.T) {
	mods := loadConsoleModules(t)

	checked := 0
	for _, name := range web.ScriptNames() {
		m := mods[name]
		for _, im := range m.imports {
			checked++
			target := name
			if strings.HasPrefix(im.from, "./") || strings.HasPrefix(im.from, "../") {
				target = path.Join(path.Dir(name), im.from)
			} else {
				t.Errorf("%s 里 import 了非相对路径 %q：控制台只从内嵌资源加载", name, im.from)
				continue
			}
			tm, ok := mods[target]
			if !ok {
				t.Errorf("%s 里 import %q 解析到 %s，内嵌资源里没有这个文件", name, im.from, target)
				continue
			}
			if !tm.exports[im.orig] {
				t.Errorf("%s 里 import 的 %s 在 %s 里没有 export", name, im.orig, target)
			}
		}
	}
	if checked < 30 {
		t.Fatalf("只解析到 %d 条 import，正则可能已失效", checked)
	}
}
