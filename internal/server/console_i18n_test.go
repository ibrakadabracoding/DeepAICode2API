package server

import (
	"deepaicode2api/internal/server/web"
	"regexp"
	"strings"
	"testing"
)

// i18nMissingAllowlist holds Chinese fragments that are known not to need translation.
//
// They fall into three categories, none of which is a missed translation:
// - Concatenated fragments: `t("Total ") + n + t(" models")`. The right fix is to
// rewrite it as `t("Total {n} models", {n})`, which is a frontend rewrite and
//
//	outside the i18n dictionary's responsibility;
//
// - Identifiers rather than copy: the bilingual language-selector label
// (`Language / <Chinese for "language">`) and the single CJK toggle glyph;
//   - Unfindable after escaping: `Key created (shown only once):\n` evaluates with a
//     newline in JS, so a key built from the raw text can never match; it needs to
//     become a sentence without the newline.
//
// New entries must state their reason here - this list should only shrink.
var i18nMissingAllowlist = map[string]string{
	" 个模型":                    "字符串拼接片段",
	" 个账号，失败 ":                "字符串拼接片段",
	"，超过 8MB 自动轮转；支持":         "字符串拼接片段",
	"模型目录已更新：共 ":              "字符串拼接片段",
	"&lt;数据文件&gt;-logs.jsonl": "HTML 实体占位符，不是文案",
	"Language / 语言":           "语言选择器自指，故意双语",
	"DeepAICode2API 控制台":      "浏览器标签页标题：品牌名 + 固定后缀",
	"中":                       "EN/中 切换按钮的标签",
	`已创建密钥（仅显示一次）：\n`:         "JS 求值后含换行，键对不上",
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// i18nDictBlocks extracts the full text of every `export const DICT = { ... }` block.
//
// The dictionary has been split by purpose into js/i18n/dict-*.js (shell / pages),
// so there are multiple blocks; the end is still located with **string-aware brace
// matching** (literal `{n}` and JSON examples must not interfere with matching).
func i18nDictBlocks(t *testing.T, src string) []string {
	t.Helper()
	const marker = "_DICT = {"
	var out []string
	offset := 0
	for {
		rel := strings.Index(src[offset:], marker)
		if rel < 0 {
			break
		}
		start := offset + rel
		depth := 0
		inStr := false
		closed := false
		for i := start; i < len(src); i++ {
			c := src[i]
			if inStr {
				if c == '\\' {
					i++
					continue
				}
				if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					out = append(out, src[start:i+1])
					offset = i + 1
					closed = true
				}
			}
			if closed {
				break
			}
		}
		if !closed {
			t.Fatal("DICT 字典括号未闭合")
		}
	}
	if len(out) == 0 {
		t.Fatal("找不到 DICT 字典块")
	}
	return out
}

// i18nDictBlock returns the concatenation of all dictionary blocks (used for entry
// parsing / duplicate key checks).
func i18nDictBlock(t *testing.T, src string) string {
	t.Helper()
	return strings.Join(i18nDictBlocks(t, src), "\n")
}

// i18nDictionary parses I18N_EN from the script and returns "Chinese source →
// English".
func i18nDictionary(t *testing.T, src string) map[string]string {
	t.Helper()
	body := i18nDictBlock(t, src)

	entry := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*:`)
	dict := map[string]string{}
	for _, m := range entry.FindAllStringSubmatch(body, -1) {
		dict[m[1]] = ""
	}
	if len(dict) < 100 {
		t.Fatalf("只解析到 %d 条词条，正则可能已失效", len(dict))
	}
	return dict
}

// TestConsoleI18nCoverage is the ratchet test for "translation gaps".
//
// The console uses the "Chinese source text" as the i18n key: when an entry is
// missing, switching to English **silently** keeps the Chinese, so users see mixed
// Chinese/English while CI stays green. This test extracts every Chinese-containing
// string from the source and diffs it against the dictionary, allowing only the
// structural fragments listed in i18nMissingAllowlist to be absent.
func TestConsoleI18nCoverage(t *testing.T) {
	markup := consoleMarkup()
	script := web.Scripts()
	dict := i18nDictionary(t, script)

	candidates := map[string]string{} // text → origin

	// 1) JS string literals (dynamically rendered copy).
	for _, m := range regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(script, -1) {
		if v := m[1]; hasCJK(v) && len(v) < 120 {
			candidates[v] = "JS 字面量"
		}
	}
	// 2) Text nodes in the markup (static copy).
	for _, m := range regexp.MustCompile(`>([^<>]+)<`).FindAllStringSubmatch(markup, -1) {
		if v := strings.TrimSpace(m[1]); hasCJK(v) && len(v) < 80 {
			candidates[v] = "标记文本"
		}
	}
	// 3) Placeholders and titles (translateAttrs translates both).
	for _, m := range regexp.MustCompile(`(?:placeholder|title)="([^"]+)"`).FindAllStringSubmatch(markup, -1) {
		if hasCJK(m[1]) {
			candidates[m[1]] = "属性"
		}
	}

	if len(candidates) < 100 {
		t.Fatalf("只抽到 %d 条候选文案，提取逻辑可能已失效", len(candidates))
	}

	var missing []string
	for text, from := range candidates {
		if _, ok := dict[text]; ok {
			continue
		}
		if _, ok := i18nMissingAllowlist[text]; ok {
			continue
		}
		missing = append(missing, from+"："+text)
	}
	if len(missing) > 0 {
		t.Errorf("以下文案缺少英文词条（翻成英文会静默显示中文）：\n  %s\n"+
			"补进 index.html 的 I18N_EN，或（确实不该翻时）加进 i18nMissingAllowlist 并写明理由",
			strings.Join(missing, "\n  "))
	}

	// The allowlist should only shrink: entries that are already translated must be
	// removed from the list.
	for text := range i18nMissingAllowlist {
		if _, ok := dict[text]; ok {
			t.Errorf("i18nMissingAllowlist 里的 %q 已经有词条了，请从名单中删除", text)
		}
		if _, ok := candidates[text]; !ok {
			t.Errorf("i18nMissingAllowlist 里的 %q 在源码中已不存在，请从名单中删除", text)
		}
	}
}

// TestConsoleI18nNoDuplicateKeys prevents duplicate keys when the dictionary is
// edited by hand.
//
// Duplicate keys in a JS object are not an error; the latter silently overrides the
// former - the symptom is "changed an entry but it does not take effect".
func TestConsoleI18nNoDuplicateKeys(t *testing.T) {
	html := web.Scripts()
	body := i18nDictBlock(t, html)

	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*:`).FindAllStringSubmatch(body, -1) {
		seen[m[1]]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("I18N_EN 里 %q 出现 %d 次（后者会静默覆盖前者）", k, n)
		}
	}
}

// TestConsoleI18nNoDeadEntries is the **reverse** direction of the coverage ratchet:
// an entry must actually be used in the source, otherwise it is dead.
//
// Why this is needed: when a feature is retired (registration flow / fingerprint
// library / proxy pool input), the source is deleted but nobody cleans the
// dictionary, so entries rot in it forever. The forward ratchet (coverage) only
// looks at "what is missing" and is completely blind to "what is extra" -
// introducing this test on 2026-11-11 removed 9 dead entries.
//
// How it decides: remove the dictionary blocks from the source; if the remaining JS
// + markup contains the source text, it counts as "in use". Escaped keys of the form
// {"a\":\"b"} in the dictionary are unescaped before matching, so backslashes in the
// JSON examples do not misclassify them as dead entries.
func TestConsoleI18nNoDeadEntries(t *testing.T) {
	script := web.Scripts()
	dict := i18nDictionary(t, script)
	// Strip the dictionary: otherwise every key would match itself at its declaration.
	// Also strip comments: an entry merely mentioned in a comment (for example "the
	// phase A move") does not count as copy in use - the first version did not strip
	// comments and let a genuinely dead entry pass as "still used".
	rest := script
	for _, block := range i18nDictBlocks(t, script) {
		rest = strings.Replace(rest, block, "", 1)
	}
	rest = stripJSComments(rest) + "\n" + stripHTMLComments(consoleMarkup())

	var dead []string
	for key := range dict {
		decoded := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(key)
		if strings.Contains(rest, decoded) || strings.Contains(rest, key) {
			continue
		}
		dead = append(dead, key)
	}
	if len(dead) > 0 {
		t.Errorf("以下词条在源码里已经没人用了（删掉它们，功能下线时别只删代码）：\n  %s",
			strings.Join(dead, "\n  "))
	}
}

// stripJSComments removes // and /* */ comments, leaving string literals and template
// strings untouched.
//
// Newlines inside block comments are preserved: the console contract test relies on
// that to count line numbers directly from `src`.
func stripJSComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inStr, inTpl, inLine, inBlock := false, false, false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
				continue
			}
			if c == '\n' {
				b.WriteByte(c)
			}
		case inStr, inTpl:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
				continue
			}
			if inStr && c == '"' {
				inStr = false
			}
			if inTpl && c == '`' {
				inTpl = false
			}
		default:
			if c == '/' && i+1 < len(src) && src[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
			if c == '"' {
				inStr = true
			}
			if c == '`' {
				inTpl = true
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// stripHTMLComments removes <!-- ... --> for the same reason.
func stripHTMLComments(src string) string {
	for {
		start := strings.Index(src, "<!--")
		if start < 0 {
			return src
		}
		end := strings.Index(src[start:], "-->")
		if end < 0 {
			return src
		}
		src = src[:start] + src[start+end+3:]
	}
}
