package server

import (
	"deepaicode2api/internal/server/web"
	"regexp"
	"strings"
	"testing"
)

// The console is an embedded single page, and changes regress silently easily (e.g. mismatched field
// names between frontend and backend). These tests guard a few key conventions: theme, pagination,
// and camelCase model fields.

// consoleSource returns the concatenation of "markup + all ES module scripts + styles".
//
// The frontend has been split into multiple modules: scanning only index.html would silently disable
// assertions like "function name / selector exists" (tests stay green while the checks no longer
// cover anything).
func consoleSource() string {
	return consoleMarkup() + "\n" + consoleStyle() + "\n" + web.Scripts()
}

// consoleMarkup returns only the page markup (pure-markup assertions such as id definitions and tab structure).
func consoleMarkup() string { return string(web.HTML) }

// consoleStyle returns the extracted stylesheet.
func consoleStyle() string { return string(web.CSS) }

func TestConsoleHTMLContracts(t *testing.T) {
	html := consoleSource()
	if html == "" {
		t.Fatal("控制台资源为空")
	}

	must := map[string]string{
		"主题按钮":    `id="theme"`,
		"浅色变量":    `:root[data-theme="light"]`,
		"分页容器":    `id="acctPager"`,
		"分页信息":    `id="acctInfo"`,
		"每页选择":    `id="acctSize"`,
		"上一页":     `id="acctPrev"`,
		"下一页":     `id="acctNext"`,
		"分页渲染":    `updateAcctPager`,
		"分页状态":    `acctPageSize`,
		"主题应用":    `applyTheme`,
		"主题持久化":   `deepaicode2api_theme`,
		"分页大小持久化": `deepaicode2api_acct_page_size`,
	}
	for name, needle := range must {
		if !strings.Contains(html, needle) {
			t.Errorf("控制台缺少%s：%s", name, needle)
		}
	}

	// The model catalog uses camelCase (deepai.Model's json tags); the page must read values by camelCase.
	for _, field := range []string{"m.theosPerRequest", "m.contextWindow", "m.maxTokens", "m.input", "m.premier"} {
		if !strings.Contains(html, field) {
			t.Errorf("模型目录未按 json tag 取值：%s", field)
		}
	}
	for _, wrong := range []string{"m.TheosPerRequest", "m.ContextWindow", "m.MaxTokens", "m.Input"} {
		if strings.Contains(html, wrong) {
			t.Errorf("模型目录仍在使用 Go 字段名（会渲染成空）：%s", wrong)
		}
	}
}

// Pagination defaults to 20 and only allows 20/50/100.
func TestConsoleDefaultPageSize(t *testing.T) {
	html := consoleSource()
	// Pagination state has moved to js/state.js: the default lives in the ui object.
	if !strings.Contains(html, "acctPageSize: 20") {
		t.Error("分页默认值应为 20")
	}
	// Look only at options inside the acctSize select (other page controls also have value="200").
	start := strings.Index(html, `id="acctSize"`)
	if start < 0 {
		t.Fatal("找不到 acctSize 选择框")
	}
	end := strings.Index(html[start:], "</select>")
	if end < 0 {
		t.Fatal("acctSize 选择框未闭合")
	}
	block := html[start : start+end]
	for _, v := range []string{`value="20"`, `value="50"`, `value="100"`} {
		if !strings.Contains(block, v) {
			t.Errorf("分页选择框缺少选项 %s", v)
		}
	}
	if strings.Contains(block, `value="200"`) {
		t.Error("分页选择框不应有 200 选项")
	}
}

// Every DOM id referenced by a script must exist in the markup.
//
// This is the easiest kind of change to regress silently: miss one id while editing markup and the
// console doesn't error-some feature just never works (button clicks do nothing, render functions
// write into null).
func TestConsoleScriptIDsExist(t *testing.T) {
	// Validate grouped by page: login.js's DOM lives in login.html, not index.html.
	// The most common mistake when splitting pages is "a script references another page's id"; that
	// feature then never works (button clicks do nothing, render functions write into null).
	indexIDs := idsInMarkup(consoleMarkup())
	loginIDs := idsInMarkup(string(web.LoginHTML))

	refs, files := 0, 0
	for _, name := range web.ScriptNames() {
		raw, err := web.Read(name)
		if err != nil {
			t.Fatalf("读取脚本 %s: %v", name, err)
		}
		files++
		defined := indexIDs
		if strings.HasSuffix(name, "/login.js") {
			defined = loginIDs
		}
		for id := range idsReferencedBy(string(raw)) {
			refs++
			if !defined[id] {
				t.Errorf("%s 引用了不存在的 id：%s", name, id)
			}
		}
	}
	if files < 10 {
		t.Fatalf("只扫到 %d 个脚本，按页面分组可能已失效", files)
	}
	if refs < 30 {
		t.Fatalf("只解析到 %d 个 id 引用，正则可能已失效", refs)
	}
}

// idsInMarkup collects ids defined in the markup.
func idsInMarkup(markup string) map[string]bool {
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`).FindAllStringSubmatch(markup, -1) {
		defined[m[1]] = true
	}
	return defined
}

// idsReferencedBy collects ids fetched by scripts via $("#x") / getElementById / querySelector.
func idsReferencedBy(script string) map[string]bool {
	refs := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\$\("#([A-Za-z0-9_-]+)"\)`),
		regexp.MustCompile(`getElementById\("([A-Za-z0-9_-]+)"\)`),
		regexp.MustCompile(`querySelector\("#([A-Za-z0-9_-]+)"\)`),
	} {
		for _, m := range re.FindAllStringSubmatch(script, -1) {
			refs[m[1]] = true
		}
	}
	return refs
}

// Every data-tab must have a corresponding #tab-xxx panel, and the <section> count must match.
//
// The script switches pages with querySelectorAll("section"): extra <section> elements (e.g. a card
// written as a section) are treated as tabs, lose .on during page switches, and get hidden with them.
func TestConsoleTabsHavePanels(t *testing.T) {
	html := consoleSource()
	tabs := regexp.MustCompile(`data-tab="([A-Za-z0-9_-]+)"`).FindAllStringSubmatch(html, -1)
	if len(tabs) == 0 {
		t.Fatal("没有导航按钮")
	}
	for _, m := range tabs {
		if !strings.Contains(html, `id="tab-`+m[1]+`"`) {
			t.Errorf("导航按钮 %s 缺少对应面板", m[1])
		}
	}
	if got := strings.Count(html, "<section"); got != len(tabs) {
		t.Errorf("<section> 数量 %d 与导航按钮 %d 不一致", got, len(tabs))
	}
}

// Key hooks for the shell layout and responsive column collapsing.
func TestConsoleShellHooks(t *testing.T) {
	html := consoleSource()
	for name, needle := range map[string]string{
		"侧边栏":      `class="sidebar"`,
		"移动端开关":    `id="navToggle"`,
		"抽屉遮罩":     `id="scrim"`,
		"开关只在窄屏出现": `button.icon-btn.only-mobile{display:none}`,
		"账号表收列":    `#acctTable`,
		"日志表收列":    `#logTable`,
		"深链接":      `location.hash`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("外壳缺少%s：%s", name, needle)
		}
	}
}

// Key hooks for the drawer menu / table header sorting / bilingual Chinese-English UI.
//
// All three are "silent failure" features: a missing dictionary entry doesn't error, it just lets a
// Chinese sentence pop up in the English UI; a table header without wireSort doesn't error, clicking
// just does nothing.
func TestConsoleFeatures(t *testing.T) {
	html := consoleSource()

	for name, needle := range map[string]string{
		"语言按钮":      `id="lang"`,
		"英文字典":      `_DICT = {`,
		"翻译入口":      `function translateDom(`,
		"语言应用":      `function applyLang(`,
		"语言深链接":     `[?&]lang=(en|zh)`,
		"语言持久化":     `deepaicode2api_lang`,
		"菜单构造":      `function openMenu(`,
		"账号行菜单":     `function rowMenu(`,
		"密钥行菜单":     `function keyMenu(`,
		"菜单关闭":      `function closeMenu(`,
		"账号表排序：服务端": `wireSort("#acctTable", "accounts"`,
		"模型表排序":     `wireSort("#modelTable", "models"`,
		"密钥表排序":     `wireSort("#keyTable", "keys"`,
		"日志表排序":     `wireSort("#logTable", "logs"`,
		"排序参数下推":    `"&sort=" + encodeURIComponent(st.key)`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("缺少%s：%s", name, needle)
		}
	}

	// Every table that declares data-sort must have a matching wireSort, otherwise clicking does nothing.
	for _, tbl := range []string{"acctTable", "modelTable", "keyTable", "logTable"} {
		if !strings.Contains(html, `id="`+tbl+`"`) {
			t.Errorf("表 %s 缺少 id（排序接线依赖它）", tbl)
		}
	}
	if n := strings.Count(html, "data-sort="); n < 20 {
		t.Errorf("data-sort 表头过少（%d），排序可能没接全", n)
	}
}

// The console must stay aligned with backend capabilities: filters, lifecycle actions, and K/M units
// must not be silently dropped.
func TestConsoleBackendParity(t *testing.T) {
	html := consoleSource()

	// Filters: the frontend must actually send the parameters supported by backend AccountPage / QueryLogs.
	for _, needle := range []string{
		`id="acctFilters"`, `id="acctGroup"`, `id="acctStatus"`, `id="acctQuery"`,
		`id="logFilters"`, `id="logEndpoint"`, `id="logAccount"`, `id="logMinStatus"`,
		`id="logKeyword"`, `id="logSince"`,
		`"&group="`, `"&status="`, `"&q="`, `"&endpoint="`, `"&min_status="`, `"&since="`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("控制台缺少筛选接线：%s", needle)
		}
	}

	// Lifecycle actions / key grouping: backend action names must match the frontend.
	for _, action := range []string{"assign-proxy", "assign-machine", "set-group", "mark-verified"} {
		if !strings.Contains(html, `"`+action+`"`) {
			t.Errorf("控制台缺少 action=%s 的入口", action)
		}
	}

	// Unit compaction: tokens / characters go through fmtCompact, and both KPI and logs use it.
	if !strings.Contains(html, "function fmtCompact(") {
		t.Fatal("缺少 fmtCompact")
	}
	if !strings.Contains(html, `fmtCompact(s.total_chars_out)`) ||
		!strings.Contains(html, `fmtCompact(s.total_tokens_in)`) {
		t.Error("概览的字符 / Token 应改用 fmtCompact")
	}
	for _, needle := range []string{`fmtCompact(l.prompt_len)`, `fmtCompact(l.output_len)`, `fmtCompact(l.tokens_in)`, `fmtCompact(l.tokens_out)`} {
		if !strings.Contains(html, needle) {
			t.Errorf("日志应改用 fmtCompact：%s", needle)
		}
	}

	// The strict model mode toggle must read the backend field.
	if !strings.Contains(html, `strict_model`) {
		t.Error("设置页未接 strict_model")
	}
}

// The theme button and login link are two UI conventions that regress easily:
//   - The theme icon is inline SVG (no longer emoji); labels still use the i18n "dark / light" strings;
//   - The browser login panel must allow copying the login link (absolute address) for opening in
//     another browser/machine.
func TestConsoleThemeAndLoginLink(t *testing.T) {
	html := consoleSource()

	for name, needle := range map[string]string{
		"太阳图标":  `id="themeIconSun"`,
		"月亮图标":  `id="themeIconMoon"`,
		"文案节点":  `id="themeLabel"`,
		"登录链接框": `id="oauthLink"`,
		"复制按钮":  `id="oauthCopy"`,
		"复制实现":  `function copyText(`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("控制台缺少%s：%s", name, needle)
		}
	}

	// After the icon became SVG, applyTheme must no longer inject emoji text into the button.
	if strings.Contains(html, "☀️ 浅色") || strings.Contains(html, "🌙 深色") {
		t.Error("主题按钮仍残留 emoji 文案")
	}
	if !strings.Contains(html, `label.textContent = mode === "light" ? "浅色" : "深色";`) {
		t.Error("applyTheme 应只切换「浅色 / 深色」文案，图标交给 SVG 控制显隐")
	}

	// Copy the gateway's absolute address; a relative path alone breaks when changing pages/machines.
	if !strings.Contains(html, "location.origin") {
		t.Error("登录链接应拼成绝对地址（location.origin）")
	}
}
