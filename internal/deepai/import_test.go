package deepai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCredential writes a desktop credential file under the specified directory.
func writeCredential(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

// TestImportAllFromDesktopExplicitFile covers the shortest path of "pointing directly at a credential file".
func TestImportAllFromDesktopExplicitFile(t *testing.T) {
	dir := t.TempDir()
	path := writeCredential(t, dir, CredentialFile,
		`{"apiKey":"sk-desktop-1","username":"alice@example.com","isPro":true}`)

	results, err := ImportAllFromDesktop(path)
	if err != nil {
		t.Fatalf("ImportAllFromDesktop: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("导入条数 = %d，期望 1", len(results))
	}
	res := results[0]
	if res.Path != path {
		t.Fatalf("Path = %q，期望 %q", res.Path, path)
	}
	if res.Account.APIKey != "sk-desktop-1" || res.Account.Username != "alice@example.com" {
		t.Fatalf("账号字段错误: %+v", res.Account)
	}
	if res.Account.Source != "desktop" {
		t.Fatalf("Source = %q，期望 desktop", res.Account.Source)
	}
	if !res.Account.IsPro || !res.Account.Enabled || res.Account.Status != "unknown" {
		t.Fatalf("账号状态错误: %+v", res.Account)
	}
	if res.Account.Name != "alice@example.com" {
		t.Fatalf("Name 应取 username，得到 %q", res.Account.Name)
	}
}

// TestImportAllFromDesktopScansLayouts covers the three secrets directories baseDir → userdata/dev,
// and verifies that the same Key appearing in multiple places is imported only once.
func TestImportAllFromDesktopScansLayouts(t *testing.T) {
	base := t.TempDir()
	sameKey := `{"apiKey":"sk-dup","username":"dup@example.com"}`

	// The same Key is deliberately written twice: userdata/secrets and secrets.
	writeCredential(t, filepath.Join(base, "userdata", "secrets"), CredentialFile, sameKey)
	writeCredential(t, filepath.Join(base, "secrets"), CredentialFile, sameKey)
	// dev/secrets contains another account.
	writeCredential(t, filepath.Join(base, "dev", "secrets"), CredentialFile,
		`{"apiKey":"sk-dev","username":"dev@example.com"}`)

	results, err := ImportAllFromDesktop(base)
	if err != nil {
		t.Fatalf("ImportAllFromDesktop: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("去重后应有 2 个账号，得到 %d: %+v", len(results), results)
	}
	keys := map[string]bool{}
	for _, r := range results {
		keys[r.Account.KeyID()] = true
	}
	if len(keys) != 2 {
		t.Fatalf("导入的 Key 指纹重复: %v", keys)
	}

	// Explicitly pointing at baseDir is just as valid as pointing at a credential file.
	if got, err := ImportAllFromDesktop(filepath.Join(base, "userdata", "secrets", CredentialFile)); err != nil || len(got) != 1 {
		t.Fatalf("显式文件导入失败: %v / %d", err, len(got))
	}
}

func TestImportAllFromDesktopMissing(t *testing.T) {
	base := filepath.Join(t.TempDir(), "no-such-dir")
	_, err := ImportAllFromDesktop(base)
	if err == nil {
		t.Fatal("没有凭据文件时应报错")
	}
	if !strings.Contains(err.Error(), "未找到桌面端凭据") {
		t.Fatalf("错误文案应解释未找到，得到 %v", err)
	}
	if !strings.Contains(err.Error(), base) {
		t.Fatalf("错误应列出尝试过的目录，得到 %v", err)
	}
}

// TestImportAllFromDesktopCorruptFile covers "file exists but parsing fails":
// it should surface the last error instead of disguising it as "not found".
func TestImportAllFromDesktopCorruptFile(t *testing.T) {
	base := t.TempDir()
	writeCredential(t, filepath.Join(base, "secrets"), CredentialFile, `{not json`)

	_, err := ImportAllFromDesktop(base)
	if err == nil || !strings.Contains(err.Error(), "凭据解析失败") {
		t.Fatalf("损坏的凭据应报解析失败，得到 %v", err)
	}
}

// TestImportSkipsFilesNamedLikeDirectories confirms that no panic occurs when a directory name matches CredentialFile.
func TestImportSkipsFilesNamedLikeDirectories(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "secrets", CredentialFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAllFromDesktop(base); err == nil || !strings.Contains(err.Error(), "未找到桌面端凭据") {
		t.Fatalf("应是“未找到”，得到 %v", err)
	}
}

func TestAccountFromCredentialsNil(t *testing.T) {
	if _, err := AccountFromCredentials(nil, "desktop"); err == nil ||
		!strings.Contains(err.Error(), "凭据为空") {
		t.Fatalf("nil 凭据应报错，得到 %v", err)
	}
}

func TestAccountFromAPIKeyValidation(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"空", "", "不能为空"},
		{"纯空白", "   ", "不能为空"},
		{"超长", strings.Repeat("k", 513), "格式非法"},
		{"含空格", "sk-a b", "格式非法"},
		{"含换行", "sk-a\nb", "格式非法"},
		{"含制表符", "sk-a\tb", "格式非法"},
	}
	for _, c := range cases {
		_, err := AccountFromAPIKey(c.key, "u", "manual", false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v，期望包含 %q", c.name, err, c.want)
		}
	}

	// 512 bytes is exactly the upper limit and should pass.
	if _, err := AccountFromAPIKey(strings.Repeat("k", 512), "", "", false); err != nil {
		t.Fatalf("512 字节 Key 应合法，得到 %v", err)
	}

	acct, err := AccountFromAPIKey("  sk-ok  ", "", "", true)
	if err != nil {
		t.Fatalf("AccountFromAPIKey: %v", err)
	}
	if acct.APIKey != "sk-ok" {
		t.Fatalf("Key 未去空白: %q", acct.APIKey)
	}
	if acct.Source != "manual" {
		t.Fatalf("空 source 应回落 manual，得到 %q", acct.Source)
	}
	if acct.Group != "default" || !acct.Enabled || acct.Status != "unknown" {
		t.Fatalf("默认字段错误: %+v", acct)
	}
	if !acct.IsPro {
		t.Fatal("isPro 未透传")
	}
	if acct.Name != "DeepAI "+ShortHash("sk-ok") {
		t.Fatalf("无用户名时应生成兜底名，得到 %q", acct.Name)
	}
	if acct.ID == "" {
		t.Fatal("ID 不应为空")
	}
	if acct.ID == (&Account{ID: "x"}).ID {
		_ = 0
	}
	// The ID must be 8 random bytes in hex (16 characters).
	if len(acct.ID) != 16 || strings.Trim(acct.ID, "0123456789abcdef") != "" {
		t.Fatalf("ID = %q，期望 16 位小写 hex", acct.ID)
	}
}

func TestAccountFromRegistration(t *testing.T) {
	r := Registration{
		Email:          "  bob@example.com  ",
		Password:       "pw",
		APIKey:         "sk-reg",
		SessionID:      "sess",
		CSRFToken:      "csrf",
		UserPK:         99,
		Plan:           "PRO",
		EmailVerified:  true,
		Group:          "  g2  ",
		Proxy:          " http://p:8080 ",
		Fingerprint:    " fp ",
		MachineID:      " machine ",
		UserAgent:      " UA ",
		AcceptLanguage: " zh-CN ",
		SecCHUA:        " chromium ",
		Platform:       " win ",
	}

	acct, err := AccountFromRegistration(r)
	if err != nil {
		t.Fatalf("AccountFromRegistration: %v", err)
	}
	if acct.Email != "bob@example.com" || acct.Username != "bob@example.com" {
		t.Fatalf("Email/Username 未规范化: %+v", acct)
	}
	if acct.Name != "bob@example.com" {
		t.Fatalf("无 username 时 Name 应取 Email，得到 %q", acct.Name)
	}
	if !acct.IsPro {
		t.Fatal("Plan=PRO 应判定 isPro")
	}
	if acct.Group != "g2" || acct.Proxy != "http://p:8080" || acct.Fingerprint != "fp" ||
		acct.MachineID != "machine" || acct.UserAgent != "UA" ||
		acct.AcceptLanguage != "zh-CN" || acct.SecCHUA != "chromium" || acct.Platform != "win" {
		t.Fatalf("注册字段未去空白/落库: %+v", acct)
	}
	if acct.Password != "pw" || acct.WebSessionID != "sess" || acct.CSRFToken != "csrf" || acct.UserPK != 99 {
		t.Fatalf("网页会话字段丢失: %+v", acct)
	}
	if !acct.EmailVerified || acct.Plan != "PRO" {
		t.Fatalf("验证状态/套餐丢失: %+v", acct)
	}
	if acct.Source != "register" {
		t.Fatalf("空 Source 应回落 register，得到 %q", acct.Source)
	}
	if acct.RegisteredAt == 0 || acct.LastVerified == 0 {
		t.Fatalf("时间戳未写入: %+v", acct)
	}

	// An explicit Source takes precedence.
	r.Source = "oauth"
	if acct, _ := AccountFromRegistration(r); acct.Source != "oauth" {
		t.Fatalf("Source = %q，期望 oauth", acct.Source)
	}
}

func TestAccountFromRegistrationEmptyGroup(t *testing.T) {
	acct, err := AccountFromRegistration(Registration{APIKey: "sk-1", Username: "u"})
	if err != nil {
		t.Fatalf("AccountFromRegistration: %v", err)
	}
	if acct.Group != "default" {
		t.Fatalf("空分组应规范化为 default，得到 %q", acct.Group)
	}
	if acct.Name != "u" {
		t.Fatalf("Name = %q，期望 u", acct.Name)
	}
	if acct.IsPro {
		t.Fatal("空 Plan 不应判定 isPro")
	}
}

func TestNormalizeGroup(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "default"},
		{"   ", "default"},
		{" g2 ", "g2"},
		{"default", "default"},
	}
	for _, c := range cases {
		if got := normalizeGroup(c.in); got != c.want {
			t.Errorf("normalizeGroup(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestEnvAccount(t *testing.T) {
	t.Setenv("DEEPAI_API_KEY", "")
	if got := EnvAccount(); got != nil {
		t.Fatalf("未设置环境变量时应返回 nil，得到 %+v", got)
	}

	t.Setenv("DEEPAI_API_KEY", "  sk-env  ")
	t.Setenv("DEEPAI_API_KEY_NAME", "  env-account  ")
	acct := EnvAccount()
	if acct == nil {
		t.Fatal("设置了有效 Key 应返回账号")
	}
	if acct.APIKey != "sk-env" || acct.Source != "env" || acct.Name != "env-account" {
		t.Fatalf("环境账号字段错误: %+v", acct)
	}

	// An invalid Key (containing spaces) should silently return nil and not block gateway startup.
	t.Setenv("DEEPAI_API_KEY", "sk-with space")
	if got := EnvAccount(); got != nil {
		t.Fatalf("非法 Key 应返回 nil，得到 %+v", got)
	}
}
