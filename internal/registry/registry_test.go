package registry

import (
	"testing"

	"deepaicode2api/internal/deepai"
)

func catalog() []deepai.Model {
	return []deepai.Model{
		{ID: "glm-5.3-flash", Name: "GLM 5.3 Flash"},
		{ID: "gpt-6-luna", Name: "GPT 6 Luna"},
		{ID: "claude-opus-5-5", Name: "Claude Opus 5.5"},
	}
}

func TestResolveExact(t *testing.T) {
	got := Resolve(catalog(), nil, "gpt-6-luna", "")
	if got.ID != "gpt-6-luna" {
		t.Fatalf("精确匹配失败: %q", got.ID)
	}
	// Case-insensitive.
	if got := Resolve(catalog(), nil, "GPT-6-LUNA", ""); got.ID != "gpt-6-luna" {
		t.Fatalf("应大小写不敏感: %q", got.ID)
	}
}

func TestResolvePrefixAndWhitespace(t *testing.T) {
	for _, name := range []string{"deepai/gpt-6-luna", "openai/gpt-6-luna", "  gpt-6-luna  ", "deepai:gpt-6-luna"} {
		if got := Resolve(catalog(), nil, name, ""); got.ID != "gpt-6-luna" {
			t.Errorf("%q → %q，期望 gpt-6-luna", name, got.ID)
		}
	}
}

func TestResolveCustomAlias(t *testing.T) {
	aliases := map[string]string{"my-model": "claude-opus-5-5", "MiXeD": "gpt-6-luna"}
	if got := Resolve(catalog(), aliases, "my-model", ""); got.ID != "claude-opus-5-5" {
		t.Fatalf("自定义别名未生效: %q", got.ID)
	}
	if got := Resolve(catalog(), aliases, "mixed", ""); got.ID != "gpt-6-luna" {
		t.Fatalf("别名键应大小写不敏感: %q", got.ID)
	}
}

func TestResolveBuiltinAlias(t *testing.T) {
	// Common model names from Claude Code / Cline / OpenAI SDK should all resolve to the default free model.
	for _, name := range []string{"gpt-4o", "claude-3-5-sonnet", "deepseek-chat", "default", "deepai"} {
		got := Resolve(catalog(), nil, name, "")
		if got.ID != deepai.DefaultModel {
			t.Errorf("%q → %q，期望回落 %q", name, got.ID, deepai.DefaultModel)
		}
	}
}

func TestResolveLooseMatch(t *testing.T) {
	// There is no "opus" in the catalog, but the ID contains it.
	if got := Resolve(catalog(), nil, "opus", ""); got.ID != "claude-opus-5-5" {
		t.Fatalf("包含匹配失败: %q", got.ID)
	}
	// Match by name (spaces converted to hyphens).
	if got := Resolve(catalog(), nil, "gpt-6-luna", ""); got.ID != "gpt-6-luna" {
		t.Fatalf("匹配失败: %q", got.ID)
	}
	if got := Resolve(catalog(), nil, "glm-5.3-flash", ""); got.ID != "glm-5.3-flash" {
		t.Fatalf("匹配失败: %q", got.ID)
	}
}

func TestResolveFallbackToDefaultThenFirst(t *testing.T) {
	c := catalog()
	if got := Resolve(c, nil, "no-such-model-xyz", "gpt-6-luna"); got.ID != "gpt-6-luna" {
		t.Fatalf("无法识别时应回落到 defaultID，实际 %q", got.ID)
	}
	if got := Resolve(c, nil, "no-such-model-xyz", "also-missing"); got.ID != "glm-5.3-flash" {
		t.Fatalf("defaultID 也无效时应回落到第一个模型，实际 %q", got.ID)
	}
	if got := Resolve(c, nil, "", ""); got.ID != "glm-5.3-flash" {
		t.Fatalf("空模型名应回落到第一个模型，实际 %q", got.ID)
	}
}

func TestResolveAlwaysReturnsSomething(t *testing.T) {
	// Even an empty catalog must return a usable entry, otherwise downstream will get an empty ID.
	got := Resolve(nil, nil, "anything", "")
	if got.ID == "" {
		t.Fatal("空目录时应返回兜底模型，而不是空 ID")
	}
	if got.ID != deepai.DefaultModel {
		t.Fatalf("兜底模型 = %q，期望 %q", got.ID, deepai.DefaultModel)
	}
}

func TestListIncludesAliases(t *testing.T) {
	aliases := map[string]string{"fast": "glm-5.3-flash", "broken": "not-in-catalog", "": "glm-5.3-flash"}
	list := List(catalog(), aliases)

	if len(list) != len(catalog())+1 {
		t.Fatalf("别名应追加 1 条（空键与目标不存在的应跳过），实际 %d: %#v", len(list), list)
	}
	var found bool
	for _, m := range list {
		if m.ID == "fast" {
			found = true
			if m.Name == "" {
				t.Fatal("别名条目应有可读名称")
			}
		}
	}
	if !found {
		t.Fatalf("缺少别名条目: %#v", list)
	}

	// The original catalog must not be modified.
	if catalog()[0].ID != "glm-5.3-flash" {
		t.Fatal("List 不应修改入参")
	}
}

func TestFind(t *testing.T) {
	if m, ok := Find(catalog(), "deepai/gpt-6-luna"); !ok || m.ID != "gpt-6-luna" {
		t.Fatalf("Find 失败: %#v %v", m, ok)
	}
	if _, ok := Find(catalog(), "nope"); ok {
		t.Fatal("不存在的模型应返回 false")
	}
}

// Strict mode: no fallback, no substring matching, but aliases and explicit intents such as "model name" are preserved.
func TestResolveStrictNoImplicitFallback(t *testing.T) {
	for _, name := range []string{"gpt-6-sol", "gpt-5.6-sol", "mimo-v2.6-flash", "nope"} {
		if m, ok := ResolveStrict(catalog(), nil, name, "glm-5.3-flash"); ok {
			t.Errorf("%q 不应解析成功，实际 %q", name, m.ID)
		}
		// Lenient mode keeps the original behavior (fallback) to ensure it works out of the box.
		if got := Resolve(catalog(), nil, name, "glm-5.3-flash"); got.ID != "glm-5.3-flash" {
			t.Errorf("%q 宽松模式应回落默认模型，实际 %q", name, got.ID)
		}
	}

	// Substring matching does not count as "justified": gpt-6 must not silently become the first gpt-6-* in the catalog.
	if _, ok := ResolveStrict(catalog(), nil, "gpt-6", "glm-5.3-flash"); ok {
		t.Error("严格模式不应做子串匹配")
	}
	if _, ok := ResolveStrict(catalog(), nil, "opus", ""); ok {
		t.Error("严格模式不应把 opus 子串命中 claude-opus-5-5")
	}

	// A model name (with spaces) is an explicit intent.
	if m, ok := ResolveStrict(catalog(), nil, "  Claude Opus 5.5 ", ""); !ok || m.ID != "claude-opus-5-5" {
		t.Errorf("名称匹配失败: %#v %v", m, ok)
	}
	// Aliases still take effect.
	if m, ok := ResolveStrict(catalog(), map[string]string{"mine": "gpt-6-luna"}, "mine", ""); !ok || m.ID != "gpt-6-luna" {
		t.Errorf("自定义别名失败: %#v %v", m, ok)
	}
	if m, ok := ResolveStrict(catalog(), nil, "gpt-4o", "glm-5.3-flash"); !ok || m.ID != "glm-5.3-flash" {
		t.Errorf("内置别名失败: %#v %v", m, ok)
	}
}

func TestSuggest(t *testing.T) {
	picks := Suggest(catalog(), "gpt-6-sol", 5)
	found := false
	for _, id := range picks {
		if id == "gpt-6-luna" {
			found = true
		}
		if id == "glm-5.3-flash" {
			t.Errorf("不相关的模型不应被推荐: %v", picks)
		}
	}
	if !found {
		t.Fatalf("gpt-6-sol 应推荐 gpt-6-luna 附近的模型，实际 %v", picks)
	}
	if got := Suggest(catalog(), "gpt-6-sol", 0); got != nil {
		t.Fatalf("max<=0 应返回 nil，实际 %v", got)
	}
}
