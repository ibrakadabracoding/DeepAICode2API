package store

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deepaicode2api/internal/deepai"
)

func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestParseMasterKey(t *testing.T) {
	key := testKey()

	if _, err := ParseMasterKey(""); err != nil {
		t.Fatalf("空字符串应视为未配置: %v", err)
	}
	if got, err := ParseMasterKey("   "); err != nil || got != nil {
		t.Fatalf("空白应视为未配置: %v %v", got, err)
	}

	// 64 hex characters
	hexKey := hex.EncodeToString(key)
	got, err := ParseMasterKey(hexKey)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("hex 解析失败: %v %x", err, got)
	}
	// Both uppercase and lowercase hex should be accepted
	if got, err := ParseMasterKey(strings.ToUpper(hexKey)); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("大写 hex 解析失败: %v", err)
	}
	// Various base64 variants
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		s := enc.EncodeToString(key)
		if got, err := ParseMasterKey(s); err != nil || !bytes.Equal(got, key) {
			t.Fatalf("base64（%v）解析失败: %v", enc, err)
		}
	}
	// Leading and trailing whitespace should be trimmed (.env files often contain newlines)
	if got, err := ParseMasterKey(" " + hexKey + "\n"); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("带空白的主密钥解析失败: %v", err)
	}

	// Error cases
	for _, bad := range []string{"short", strings.Repeat("a", 63), hex.EncodeToString(make([]byte, 16))} {
		if _, err := ParseMasterKey(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestSealUnsealRoundTrip(t *testing.T) {
	key := testKey()
	plain := []byte(`{"accounts":[{"api_key":"sk-secret"}],"machine_id":"m"}`)

	sealed, err := seal(key, plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("sk-secret")) {
		t.Fatal("密文中不应出现明文")
	}
	if !isEnvelope(sealed) {
		t.Fatalf("输出不是合法封装: %s", sealed)
	}
	var env sealEnvelope
	if err := json.Unmarshal(sealed, &env); err != nil {
		t.Fatalf("封装不是合法 JSON: %v", err)
	}
	if env.V != sealVersion || env.Alg != sealAlg {
		t.Fatalf("封装头错误: %#v", env)
	}

	got, encrypted, err := unseal(key, sealed)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !encrypted {
		t.Fatal("应报告为加密文件")
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("往返不一致:\n got=%s\nwant=%s", got, plain)
	}

	// The nonce must differ on every encryption (reusing a nonce in GCM is fatal).
	again, _ := seal(key, plain)
	if bytes.Equal(sealed, again) {
		t.Fatal("两次加密产生了相同密文（nonce 未随机化）")
	}
}

func TestUnsealRejectsWrongKey(t *testing.T) {
	sealed, err := seal(testKey(), []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	wrong := make([]byte, 32)
	wrong[0] = 0xFF

	if _, _, err := unseal(wrong, sealed); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("错误密钥应返回 ErrKeyMismatch，实际 %v", err)
	}
}

func TestUnsealRequiresKey(t *testing.T) {
	sealed, err := seal(testKey(), []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := unseal(nil, sealed); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("缺密钥应返回 ErrKeyRequired，实际 %v", err)
	}
}

func TestUnsealPlainJSON(t *testing.T) {
	// v1 bare JSON must remain readable and must not error.
	plain := []byte(`{"accounts":[],"settings":{}}`)
	got, encrypted, err := unseal(testKey(), plain)
	if err != nil {
		t.Fatalf("明文文件不应报错: %v", err)
	}
	if encrypted {
		t.Fatal("明文文件不应被标记为加密")
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("明文内容被改动")
	}
}

func TestUnsealTamperedCiphertext(t *testing.T) {
	key := testKey()
	sealed, err := seal(key, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	var env sealEnvelope
	if err := json.Unmarshal(sealed, &env); err != nil {
		t.Fatal(err)
	}
	ct, _ := base64.StdEncoding.DecodeString(env.CT)
	ct[0] ^= 0xFF // flip one byte
	env.CT = base64.StdEncoding.EncodeToString(ct)
	tampered, _ := json.Marshal(env)

	if _, _, err := unseal(key, tampered); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("被篡改的密文应校验失败，实际 %v", err)
	}
}

func TestOpenWithKeyEncryptsOnSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	key := testKey()

	st, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("OpenWithKey: %v", err)
	}
	st.UpsertAccount(&deepai.Account{APIKey: "sk-do-not-leak", Enabled: true})
	st.AddKey("k", "")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !st.Encrypted() {
		t.Fatal("Encrypted() 应为 true")
	}

	// No plaintext key should appear on disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("sk-do-not-leak")) {
		t.Fatalf("磁盘上出现了明文密钥:\n%s", raw)
	}
	if !isEnvelope(raw) {
		t.Fatalf("落盘内容不是加密封装:\n%s", raw)
	}

	// Reopen with the same key.
	back, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if len(back.Accounts()) != 1 || back.Accounts()[0].APIKey != "sk-do-not-leak" {
		t.Fatalf("账号未正确解密: %#v", back.Accounts())
	}
	if !back.Encrypted() {
		t.Fatal("重新打开后 Encrypted() 应为 true")
	}
}

func TestOpenEncryptedWithoutKeyFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	key := testKey()

	st, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("OpenWithKey: %v", err)
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Opening without a key must error rather than treat it as empty state and
	// overwrite it (which would wipe the entire account pool).
	if _, err := OpenWithKey(path, nil); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("缺密钥应返回 ErrKeyRequired，实际 %v", err)
	}
	// A wrong key must error as well.
	wrong := make([]byte, 32)
	wrong[1] = 9
	if _, err := OpenWithKey(path, wrong); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("错密钥应返回 ErrKeyMismatch，实际 %v", err)
	}
	// The file must not be damaged.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("状态文件不应丢失: %v", err)
	}
}

func TestOpenMigratesPlaintextToEncrypted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	key := testKey()

	// First write a legacy-format state file as plaintext.
	plain, err := OpenWithKey(path, nil)
	if err != nil {
		t.Fatalf("OpenWithKey(nil): %v", err)
	}
	plain.UpsertAccount(&deepai.Account{APIKey: "sk-legacy", Enabled: true})
	if err := plain.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if isEnvelope(raw) {
		t.Fatal("未配置密钥时不应加密")
	}

	// Then open with a key configured: it can be read, and saving converts it to
	// encrypted format.
	st, err := OpenWithKey(path, key)
	if err != nil {
		t.Fatalf("迁移读取失败: %v", err)
	}
	if len(st.Accounts()) != 1 || st.Accounts()[0].APIKey != "sk-legacy" {
		t.Fatalf("迁移后账号丢失: %#v", st.Accounts())
	}
	if st.Encrypted() {
		t.Fatal("读到明文文件时 Encrypted() 应为 false（尚未重写）")
	}
	if err := st.Save(); err != nil {
		t.Fatalf("迁移保存: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if !isEnvelope(raw) {
		t.Fatal("保存后应迁移为加密格式")
	}
}

func TestOpenFromEnvReadsMasterKey(t *testing.T) {
	t.Setenv(EnvMasterKey, hex.EncodeToString(testKey()))

	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.AddKey("k", "")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !isEnvelope(raw) {
		t.Fatal("配置了环境变量主密钥时应加密")
	}
}

func TestOpenRejectsBadMasterKeyEnv(t *testing.T) {
	t.Setenv(EnvMasterKey, "too-short")
	if _, err := Open(filepath.Join(t.TempDir(), "state.json")); err == nil {
		t.Fatal("非法主密钥应导致启动失败，而不是静默降级为明文")
	}
}

func TestEncryptedFileHasNoKeyMaterial(t *testing.T) {
	// End to end: account passwords, upstream Keys, and gateway Keys must not
	// appear in plaintext in the file.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := OpenWithKey(path, testKey())
	if err != nil {
		t.Fatalf("OpenWithKey: %v", err)
	}
	st.UpsertAccount(&deepai.Account{
		APIKey: "sk-upstream-plaintext", Password: "hunter2-plaintext",
		Email: "a@b.com", Enabled: true,
	})
	st.AddKey("console", "")
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-upstream-plaintext", "hunter2-plaintext", "a@b.com", "sk-"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("加密文件里出现了明文 %q", secret)
		}
	}
}
