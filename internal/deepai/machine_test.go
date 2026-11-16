package deepai

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestMachineFingerprint(t *testing.T) {
	// Must be a sha256 hex string (64 lowercase characters).
	got := MachineFingerprint("11111111-2222-3333-4444-555555555555")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("指纹格式错误: %q", got)
	}
	// Case and leading/trailing whitespace should be normalized.
	if again := MachineFingerprint("  11111111-2222-3333-4444-555555555555  "); again != got {
		t.Fatalf("同一 UUID 的不同写法应得到同一指纹: %q vs %q", again, got)
	}
	if upper := MachineFingerprint("11111111-2222-3333-4444-555555555555"); upper != got {
		t.Fatalf("大小写应归一: %q vs %q", upper, got)
	}
	// An empty string returns an empty string (the caller uses this to skip sending the header).
	if MachineFingerprint("") != "" || MachineFingerprint("   ") != "" {
		t.Fatal("空输入应返回空指纹")
	}
	// Different UUIDs must produce different fingerprints.
	if MachineFingerprint("a") == MachineFingerprint("b") {
		t.Fatal("不同输入得到了相同指纹")
	}
}

func TestNewMachineUUID(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewMachineUUID()
		if !pattern.MatchString(id) {
			t.Fatalf("不是合法的 v4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("生成了重复 UUID: %q", id)
		}
		seen[id] = true
	}
}

// The account-level machine fingerprint must override the process-level default - this is the prerequisite for issuing free quota per account.
func TestClientPerAccountMachine(t *testing.T) {
	var got []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(HeaderMachine))
		_, _ = io.WriteString(w, `{}`)
	})
	c.Machine = MachineFingerprint("global-machine")

	acctA := &Account{APIKey: "k1", MachineID: "aaaaaaaa-1111-4111-8111-111111111111"}
	acctB := &Account{APIKey: "k2", MachineID: "bbbbbbbb-2222-4222-8222-222222222222"}
	noMachine := &Account{APIKey: "k3"}

	for _, acct := range []*Account{acctA, acctB, noMachine} {
		if _, err := c.OpenModel(context.Background(), acct, PathChat, []byte(`{}`), ""); err != nil {
			t.Fatalf("OpenModel: %v", err)
		}
	}

	if len(got) != 3 {
		t.Fatalf("请求数 = %d", len(got))
	}
	if want := MachineFingerprint(acctA.MachineID); got[0] != want {
		t.Fatalf("账号 A 的指纹 = %q，期望 %q", got[0], want)
	}
	if want := MachineFingerprint(acctB.MachineID); got[1] != want {
		t.Fatalf("账号 B 的指纹 = %q，期望 %q", got[1], want)
	}
	if got[0] == got[1] {
		t.Fatal("两个账号不应共用同一机器指纹（会导致额度互斥）")
	}
	// Fall back to the process-level fingerprint when there is no account-level fingerprint.
	if got[2] != c.Machine {
		t.Fatalf("无账号指纹时应回退到进程级 %q，实际 %q", c.Machine, got[2])
	}
}

func TestTheosUsesAccountMachine(t *testing.T) {
	// /theos determines quota ownership, so the account's own fingerprint must be used.
	var seen string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(HeaderMachine)
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1"}`)
	})
	c.Machine = MachineFingerprint("global")

	acct := &Account{APIKey: "k", MachineID: "cccccccc-3333-4333-8333-333333333333"}
	if _, err := c.Theos(context.Background(), acct); err != nil {
		t.Fatalf("Theos: %v", err)
	}
	if want := MachineFingerprint(acct.MachineID); seen != want {
		t.Fatalf("/theos 的机器指纹 = %q，期望 %q", seen, want)
	}
}

func TestRegistrationCarriesMachineID(t *testing.T) {
	acct, err := AccountFromRegistration(Registration{
		APIKey:    "sk-abc",
		Email:     "a@b.com",
		MachineID: "dddddddd-4444-4444-8444-444444444444",
	})
	if err != nil {
		t.Fatalf("AccountFromRegistration: %v", err)
	}
	if acct.MachineID != "dddddddd-4444-4444-8444-444444444444" {
		t.Fatalf("注册信息里的机器指纹丢失: %q", acct.MachineID)
	}
	// Leading and trailing whitespace should be trimmed.
	acct2, err := AccountFromRegistration(Registration{
		APIKey: "sk-abc", Email: "a@b.com", MachineID: "  eeee  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acct2.MachineID != "eeee" {
		t.Fatalf("机器指纹未裁剪: %q", acct2.MachineID)
	}
}

func TestMachineUUIDIsStableThroughFingerprint(t *testing.T) {
	// The same account must send the same fingerprint across multiple requests; otherwise the upstream will consider the machine to be drifting.
	id := NewMachineUUID()
	if id == "" {
		t.Fatal("生成 UUID 失败")
	}
	first := MachineFingerprint(id)
	for i := 0; i < 5; i++ {
		if got := MachineFingerprint(id); got != first {
			t.Fatalf("同一 UUID 的指纹不稳定: %q vs %q", got, first)
		}
	}
	if strings.EqualFold(id, first) {
		t.Fatal("指纹不应该是 UUID 本身")
	}
}

// The inference side must reuse the browser fingerprint from registration; otherwise "Chrome registration + empty UA inference" is itself an anomaly signal.
func TestClientPerAccountFingerprintHeaders(t *testing.T) {
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	})

	acct := &Account{
		APIKey:         "k",
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/156.0.0.0",
		AcceptLanguage: "en-US,en;q=0.9",
		SecCHUA:        `"Not:A-Brand";v="8", "Chromium";v="156"`,
		Platform:       `"Windows"`,
	}
	if _, err := c.OpenModel(context.Background(), acct, PathChat, []byte(`{}`), ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}

	if v := got.Get("User-Agent"); v != acct.UserAgent {
		t.Fatalf("User-Agent = %q，期望 %q", v, acct.UserAgent)
	}
	if v := got.Get("Accept-Language"); v != acct.AcceptLanguage {
		t.Fatalf("Accept-Language = %q", v)
	}
	if v := got.Get("sec-ch-ua"); v != acct.SecCHUA {
		t.Fatalf("sec-ch-ua = %q", v)
	}
	if v := got.Get("sec-ch-ua-platform"); v != acct.Platform {
		t.Fatalf("sec-ch-ua-platform = %q", v)
	}
	if v := got.Get("sec-ch-ua-mobile"); v != "?0" {
		t.Fatalf("sec-ch-ua-mobile = %q", v)
	}
}

func TestClientFallsBackToClientFingerprint(t *testing.T) {
	// Fall back to the client default when there is no account-level fingerprint (desktop import / manual addition).
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	})
	c.UserAgent = "fallback-ua/2"
	if _, err := c.OpenModel(context.Background(), &Account{APIKey: "k"}, PathChat, []byte(`{}`), ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	if v := got.Get("User-Agent"); v != "fallback-ua/2" {
		t.Fatalf("User-Agent = %q，期望回退值", v)
	}
	// Do not send the fingerprint header when the account has no fingerprint, to avoid sending an empty value.
	if v := got.Get("Accept-Language"); v != "" {
		t.Fatalf("无账号指纹时不应发送 Accept-Language，实际 %q", v)
	}
}

// Aligned with the 0.3.37 desktop parseLinuxMachineId: 32 lowercase hex characters, not all zeros.
func TestParseLinuxMachineID(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{"  0123456789ABCDEF0123456789ABCDEF\n", "0123456789abcdef0123456789abcdef"},
		{"00000000000000000000000000000000", ""},
		{"", ""},
		{"not-a-machine-id", ""},
		{"0123456789abcdef0123456789abcde", ""},   // 31 characters
		{"0123456789abcdef0123456789abcdef0", ""}, // 33 characters
		{"0123456789abcdef0123456789abcdeg", ""},  // non-hex
	}
	for _, c := range cases {
		if got := parseLinuxMachineID(c.raw); got != c.want {
			t.Fatalf("parseLinuxMachineID(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}
