package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// State file envelope.
//
// The state file contains upstream Api-Key values, web account passwords, and
// gateway keys; file permission 0600 is only effective on Unix (os.WriteFile on
// Windows does not set ACLs), so optional whole-file encryption is supported:
//
//	v1 (legacy format, bare JSON)  {"accounts":[...],...}
//
// v2 (encrypted format) {"v":2,"alg":"aes-256-gcm","nonce":"...","ct":"..."}
//
// When v1 is read and a master key is configured, the next Save automatically
// writes v2; when no master key is configured it stays v1 (without breaking
// existing deployments).
const (
	sealVersion = 2
	sealAlg     = "aes-256-gcm"

	// EnvMasterKey is the environment variable name for the state file encryption master key.
	EnvMasterKey = "DEEPAI2API_MASTER_KEY"

	aes256KeyLen = 32
)

// Errors that may occur in the envelope layer.
var (
	// ErrKeyRequired means the file is encrypted but no master key was provided.
	ErrKeyRequired = errors.New("状态文件已加密，但未设置 " + EnvMasterKey)
	// ErrKeyMismatch means the master key does not match (GCM verification fails after decryption).
	ErrKeyMismatch = errors.New("主密钥不匹配，无法解密状态文件")
)

// sealEnvelope is the outer structure of an encrypted state file.
type sealEnvelope struct {
	V     int    `json:"v"`
	Alg   string `json:"alg"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

// ParseMasterKey parses the master key: accepts 64 hex characters or 44 base64
// characters, and must decode to 32 bytes.
func ParseMasterKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == aes256KeyLen {
		return key, nil
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if key, err := enc.DecodeString(raw); err == nil && len(key) == aes256KeyLen {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%s 必须是 32 字节（64 位 hex 或 base64），可用 openssl rand -hex 32 生成", EnvMasterKey)
}

// MasterKeyFromEnv reads the master key from the environment (returns nil when
// not set).
func MasterKeyFromEnv() ([]byte, error) {
	return ParseMasterKey(os.Getenv(EnvMasterKey))
}

// isEnvelope reports whether the raw file content is an encrypted envelope.
func isEnvelope(raw []byte) bool {
	var probe sealEnvelope
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	return probe.V == sealVersion && probe.Alg != ""
}

// seal encrypts plaintext state (indented format, so the outer structure is easy
// to inspect by eye).
func seal(key, plain []byte) ([]byte, error) {
	return sealWithIndent(key, plain, true)
}

// sealLine encrypts one line of a log and produces a **single-line** envelope.
//
// Logs are JSONL: the reader splits on \n. seal's indented format would split a
// record across multiple lines; once written, decodeLine could never parse it
// (manifesting as "logs are lost on restart once encryption is enabled"). Logs
// must therefore use compact serialization.
func sealLine(key, plain []byte) ([]byte, error) {
	return sealWithIndent(key, plain, false)
}

func sealWithIndent(key, plain []byte, indent bool) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成随机数失败: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	env := sealEnvelope{
		V:     sealVersion,
		Alg:   sealAlg,
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	}
	if indent {
		return json.MarshalIndent(env, "", "  ")
	}
	return json.Marshal(env)
}

// unseal decrypts state file content.
//
// The returned encrypted value indicates whether the file was originally in
// encrypted format; plain is the plaintext bytes. Plaintext (v1) files are
// returned as-is.
func unseal(key, raw []byte) (plain []byte, encrypted bool, err error) {
	if !isEnvelope(raw) {
		return raw, false, nil
	}
	if len(key) == 0 {
		return nil, true, ErrKeyRequired
	}
	var env sealEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, true, fmt.Errorf("状态文件封装解析失败: %w", err)
	}
	if env.Alg != sealAlg {
		return nil, true, fmt.Errorf("不支持的加密算法 %q（期望 %q）", env.Alg, sealAlg)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, true, fmt.Errorf("nonce 解码失败: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.CT)
	if err != nil {
		return nil, true, fmt.Errorf("密文解码失败: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, true, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, true, fmt.Errorf("nonce 长度 %d，期望 %d", len(nonce), gcm.NonceSize())
	}
	opened, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// GCM verification failure almost always means the wrong key; give an
		// actionable hint instead of the underlying error.
		return nil, true, ErrKeyMismatch
	}
	return opened, true, nil
}

// MasterKeyFingerprint returns a short fingerprint of the master key (first 4
// bytes of sha256, 8 hex characters).
//
// Used in startup logs and -seal output: users can confirm "which key was used
// this time" without writing the key itself to the logs. Returns an empty string
// when no master key is configured.
func (s *Store) MasterKeyFingerprint() string {
	if len(s.masterKey) == 0 {
		return ""
	}
	sum := sha256.Sum256(s.masterKey)
	return hex.EncodeToString(sum[:4])
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != aes256KeyLen {
		return nil, fmt.Errorf("主密钥长度 %d，期望 %d", len(key), aes256KeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
