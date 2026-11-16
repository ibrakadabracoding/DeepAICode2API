package deepai

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Desktop credential file name.
const (
	CredentialFile     = "deepai-account.bin"
	CredentialUserFlag = "deepai-account-user-managed.bin"
)

// ImportResult is the result of one desktop credential import.
type ImportResult struct {
	Account *Account
	Path    string
}

// ImportAllFromDesktop scans the local DeepAI Code desktop credential files.
//
// When explicit is empty, automatically detect T3CODE_HOME / ~/.deepai-code;
// Any level path can also be passed (baseDir / stateDir / secrets / credential file).
func ImportAllFromDesktop(explicit string) ([]ImportResult, error) {
	dirs := DesktopSecretDirs(explicit)

	// If it points directly to a credential file, that file takes precedence.
	if explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			if res, err := importCredentialFile(explicit); err == nil {
				return []ImportResult{*res}, nil
			}
		}
	}

	seen := map[string]bool{}
	var out []ImportResult
	var lastErr error
	for _, dir := range dirs {
		path := filepath.Join(dir, CredentialFile)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		res, err := importCredentialFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		fp := res.Account.KeyID()
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, *res)
	}
	if len(out) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("未找到桌面端凭据（已尝试：%s）", strings.Join(dirs, ", "))
	}
	return out, nil
}

func importCredentialFile(path string) (*ImportResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取凭据失败: %w", err)
	}
	var cred Credentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("凭据解析失败（%s）: %w", path, err)
	}
	acct, err := AccountFromCredentials(&cred, "desktop")
	if err != nil {
		return nil, err
	}
	return &ImportResult{Account: acct, Path: path}, nil
}

// AccountFromCredentials converts credentials to an account.
func AccountFromCredentials(cred *Credentials, source string) (*Account, error) {
	if cred == nil {
		return nil, errors.New("凭据为空")
	}
	acct, err := AccountFromAPIKey(cred.APIKey, cred.Username, source, cred.IsPro)
	if err != nil {
		return nil, err
	}
	return acct, nil
}

// AccountFromAPIKey validates and constructs an account.
func AccountFromAPIKey(key, username, source string, isPro bool) (*Account, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("API Key 不能为空")
	}
	if len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
		return nil, errors.New("API Key 格式非法")
	}
	username = strings.TrimSpace(username)
	name := username
	if name == "" {
		name = "DeepAI " + ShortHash(key)
	}
	if source == "" {
		source = "manual"
	}
	return &Account{
		ID:       RandomHex(8),
		Name:     name,
		Source:   source,
		APIKey:   key,
		Username: username,
		IsPro:    isPro,
		Group:    "default",
		Enabled:  true,
		Status:   "unknown",
	}, nil
}

// AccountFromRegistration constructs an account from the result of the web registration flow.
func AccountFromRegistration(r Registration) (*Account, error) {
	acct, err := AccountFromAPIKey(r.APIKey, r.Username, r.Source, strings.EqualFold(r.Plan, "pro"))
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(r.Email)
	if acct.Name == "" || acct.Name == "DeepAI "+ShortHash(r.APIKey) {
		if email != "" {
			acct.Name = email
		}
	}
	if r.Username == "" && email != "" {
		acct.Username = email
	}
	acct.Email = email
	acct.Password = r.Password
	acct.WebSessionID = r.SessionID
	acct.CSRFToken = r.CSRFToken
	acct.UserPK = r.UserPK
	acct.Plan = r.Plan
	acct.EmailVerified = r.EmailVerified
	acct.RegisteredAt = timeNowUnix()
	acct.LastVerified = timeNowUnix()
	acct.Group = normalizeGroup(r.Group)
	acct.Proxy = strings.TrimSpace(r.Proxy)
	acct.Fingerprint = strings.TrimSpace(r.Fingerprint)
	acct.MachineID = strings.TrimSpace(r.MachineID)
	acct.UserAgent = strings.TrimSpace(r.UserAgent)
	acct.AcceptLanguage = strings.TrimSpace(r.AcceptLanguage)
	acct.SecCHUA = strings.TrimSpace(r.SecCHUA)
	acct.Platform = strings.TrimSpace(r.Platform)
	acct.Source = r.Source
	if acct.Source == "" {
		acct.Source = "register"
	}
	if acct.Username == "" {
		acct.Username = acct.Email
	}
	if acct.Name == "" {
		acct.Name = acct.Username
	}
	if acct.Name == "" {
		acct.Name = "DeepAI " + ShortHash(r.APIKey)
	}
	return acct, nil
}

// normalizeGroup normalizes the group name.
func normalizeGroup(g string) string {
	g = strings.TrimSpace(g)
	if g == "" {
		return "default"
	}
	return g
}

func timeNowUnix() int64 { return time.Now().Unix() }

// EnvAccount constructs an account from the DEEPAI_API_KEY environment variable (optional).
func EnvAccount() *Account {
	key := strings.TrimSpace(os.Getenv("DEEPAI_API_KEY"))
	if key == "" {
		return nil
	}
	acct, err := AccountFromAPIKey(key, EnvOr("DEEPAI_API_KEY_NAME", ""), "env", false)
	if err != nil {
		return nil
	}
	return acct
}
