package deepai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// UUIDPattern matches standard UUID strings.
var UUIDPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// SMBIOSPlaceholders are placeholder UUIDs commonly found in firmware and cannot serve as stable machine identifiers.
var SMBIOSPlaceholders = map[string]bool{
	"00000000-0000-0000-0000-000000000000": true,
	"ffffffff-ffff-ffff-ffff-ffffffffffff": true,
	"03000200-0400-0500-0006-000700080009": true,
}

var (
	machineOnce sync.Once
	machineID   string
)

// MachineID returns the stable machine fingerprint of this host (sha256 of the raw identifier).
//
// The upstream uses it to issue free Theos daily: the same machine is only issued once per day.
// The result is computed only once per process; returns an empty string when it cannot be read (the upstream will reject free quota,
// but paid requests still work).
//
// Can be explicitly overridden with DEEPAI_MACHINE_ID (write the raw UUID; sha256 is still applied internally),
// for easier deployment in containers / environments without SMBIOS.
//
// Note: this is a **host** fingerprint, suitable only for requests that do not need to distinguish accounts (model catalog, etc.).
// Inference / quota requests should prefer Account.MachineID (see MachineFingerprint),
// otherwise multiple accounts share one fingerprint and the upstream only issues the daily quota to the first account.
func MachineID() string {
	machineOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv("DEEPAI_MACHINE_ID"))
		if raw == "" {
			raw = detectMachineRaw()
		}
		machineID = MachineFingerprint(raw)
	})
	return machineID
}

// MachineFingerprint converts the raw machine identifier (UUID / machine-id) into the sha256 hex required by the upstream.
//
// An empty string returns an empty string: the caller uses this to "not send the header" rather than sending an empty value.
func MachineFingerprint(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewMachineUUID generates a new random UUID (v4).
//
// Each registered account must be assigned an independent machine fingerprint so that each can receive the daily quota the upstream issues per machine;
// when a single fingerprint is shared globally, only the first account gets the quota (the rest have balance=0).
func NewMachineUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return formatUUID(b)
}

// formatUUID formats 16 bytes as 8-4-4-4-12.
func formatUUID(b [16]byte) string {
	const hexDigits = "0123456789abcdef"
	buf := make([]byte, 36)
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = hexDigits[v>>4]
		buf[j+1] = hexDigits[v&0x0f]
		j += 2
	}
	return string(buf)
}

func detectMachineRaw() string {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "windows":
		if id := parseUUID(run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-CimInstance Win32_ComputerSystemProduct).UUID")); id != "" {
			return id
		}
		return parseMachineGuid(run(ctx, "reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid"))
	case "darwin":
		out := run(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		if m := regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`).FindStringSubmatch(out); len(m) == 2 {
			if UUIDPattern.MatchString(m[1]) {
				return strings.ToLower(m[1])
			}
		}
	case "linux":
		// 0.3.37 desktop only uses the systemd machine-id (firmware UUID is unreadable without root):
		// /etc/machine-id → /var/lib/dbus/machine-id, requiring 32 lowercase hex characters and not all zeros.
		// Earlier versions read /sys/class/dmi/id/product_uuid first, but firmware placeholder values return a
		// Unstable fingerprint (long unused on desktop); aligned with 0.3.37 here.
		for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(path); err == nil {
				if id := parseLinuxMachineID(string(b)); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

// parseUUID extracts and validates the first non-placeholder UUID.
func parseUUID(s string) string {
	m := UUIDPattern.FindString(s)
	if m == "" {
		return ""
	}
	low := strings.ToLower(m)
	if SMBIOSPlaceholders[low] {
		return ""
	}
	return low
}

// parseLinuxMachineID validates the systemd machine-id: 32 lowercase hex chars and must not be all zeros.
//
// Same as desktop parseLinuxMachineId (0.3.37): all zeros is an uninitialized marker,
// and cannot be treated as a stable fingerprint.
func parseLinuxMachineID(raw string) string {
	id := strings.ToLower(strings.TrimSpace(raw))
	if len(id) != 32 {
		return ""
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	if strings.Trim(id, "0") == "" {
		return ""
	}
	return id
}

// parseMachineGuid extracts MachineGuid from reg query output.
func parseMachineGuid(out string) string {
	m := regexp.MustCompile(`MachineGuid\s+REG_SZ\s+(\S+)`).FindStringSubmatch(out)
	if len(m) != 2 {
		return ""
	}
	if UUIDPattern.MatchString(m[1]) {
		return strings.ToLower(m[1])
	}
	return strings.TrimSpace(m[1])
}

// run executes an external command and gives up after timeout; returns an empty string when output is empty.
func run(ctx context.Context, name string, args ...string) string {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}
