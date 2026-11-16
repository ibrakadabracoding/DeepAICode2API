package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// Browser login (RFC 8252 loopback callback) is exactly the same as the desktop client:
//
// GET https://api.deepai.org/deepai_code/desktop_auth/?state=...&port=...
// → After the browser completes login, it calls back to http://127.0.0.1:<port>/auth-callback?code=...&state=...
// POST https://api.deepai.org/deepai_code/desktop_auth/exchange/ {"code":...}
//
//	→ {api_key, username, is_pro}
const loginTTL = 10 * time.Minute

type loginResult struct {
	Creds *deepai.Credentials
	Err   string
}

type loginSession struct {
	ID      string
	State   string
	Port    int
	Created time.Time
	srv     *http.Server

	mu         sync.Mutex
	result     *loginResult
	exchanging bool // One path is already exchanging the code for a Key (loopback callback and "paste callback URL" are mutually exclusive)
	consumed   bool // Already added to the pool and finalized; only one of poll and paste is allowed to finalize
}

// setResult records the exchange result; does not overwrite an existing result (first come, first served).
func (s *loginSession) setResult(r *loginResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result != nil {
		return false
	}
	s.result = r
	return true
}

// snapshotResult returns the current result (nil means still waiting).
func (s *loginSession) snapshotResult() *loginResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// beginExchange claims the single slot for "exchange code for Key".
//
// The loopback callback and pasting the callback URL are two parallel entry points, but the same code can only be exchanged once
// (the upstream returns 400/401 on the second attempt). Here the exchange is serialized: whoever gets it first performs the exchange.
func (s *loginSession) beginExchange() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumed || s.exchanging || s.result != nil {
		return false
	}
	s.exchanging = true
	return true
}

// claim finalizes: returns this result and marks the session as consumed (repeated calls return nil).
func (s *loginSession) claim() *loginResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumed {
		return nil
	}
	s.consumed = true
	return s.result
}

type loginSessions struct {
	mu sync.Mutex
	m  map[string]*loginSession
}

func newLoginSessions() *loginSessions { return &loginSessions{m: map[string]*loginSession{}} }

func (ls *loginSessions) put(s *loginSession) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for k, v := range ls.m {
		if time.Since(v.Created) > loginTTL {
			delete(ls.m, k)
			go shutdownLoginServer(v.srv)
		}
	}
	ls.m[s.ID] = s
}

func (ls *loginSessions) get(id string) *loginSession {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	// Also reclaims stale sibling sessions (along with their loopback listeners).
	//
	// Cleaning up only on put is not enough: the path "start login → user gives up → never starts again" would leave
	// the session and listener around forever (each occupying a port). The login page hits this on every poll/open,
	// acting as a low-frequency janitor, so no extra background goroutine is needed.
	//
	// The requested session is returned as-is even if expired: callers need to distinguish "expired" (429/200+expired)
	// from "not found" (404), so it must not be swallowed into nil here.
	for k, v := range ls.m {
		if k == id {
			continue
		}
		if time.Since(v.Created) > loginTTL {
			delete(ls.m, k)
			go shutdownLoginServer(v.srv)
		}
	}
	return ls.m[id]
}

func (ls *loginSessions) remove(id string) *loginSession {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	s := ls.m[id]
	delete(ls.m, id)
	return s
}

func shutdownLoginServer(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// handleLoginStart starts a browser login and returns a URL that can be opened.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}

	id := deepai.RandomHex(16)
	state := deepai.RandomToken(32)
	session := &loginSession{ID: id, State: state, Created: time.Now()}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth-callback", func(cw http.ResponseWriter, cr *http.Request) {
		code := cr.URL.Query().Get("code")
		if cr.Method != http.MethodGet || code == "" || len(code) > 4096 ||
			cr.URL.Query().Get("state") != state || !secureEqual(cr.URL.Query().Get("state"), state) {
			cw.WriteHeader(http.StatusBadRequest)
			_, _ = cw.Write([]byte("Invalid sign-in callback."))
			return
		}
		// The user may have already pasted the same callback in the console; whoever exchanges first wins.
		if !session.beginExchange() {
			cw.WriteHeader(http.StatusConflict)
			_, _ = cw.Write([]byte("This sign-in link was already used."))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		creds, err := s.Up.ExchangeCode(ctx, code)
		cancel()
		if err != nil {
			s.finishLogin(session, nil, err)
			cw.WriteHeader(http.StatusBadGateway)
			_, _ = cw.Write([]byte("Sign-in failed. Return to the console and try again."))
			return
		}
		s.finishLogin(session, creds, nil)
		cw.Header().Set("Content-Type", "text/html; charset=utf-8")
		cw.Header().Set("Cache-Control", "no-store")
		_, _ = cw.Write([]byte("<h2>Signed in to DeepAI Code</h2><p>You can close this tab and return to the console.</p>"))
	})

	// The loopback callback is only the "automatic capture" path; when the loopback cannot be bound (typically: in Docker
	// 127.0.0.1 belongs to the container and the host browser cannot reach it), the flow must not break-fall back to pasting the callback URL.
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		session.Port = fallbackLoginPort()
	} else {
		session.Port = ln.Addr().(*net.TCPAddr).Port
		session.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = session.srv.Serve(ln) }()
	}
	s.Login.put(session)

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"session_id": id,
		"open_url":   "/login/open?id=" + url.QueryEscape(id),
		"port":       session.Port,
		"loopback":   session.srv != nil,
		"expires_in": int(loginTTL.Seconds()),
	})
}

// fallbackLoginPort gives the upstream a port that "looks valid" when it cannot bind to the local loopback.
//
// The upstream only concatenates this value into http://127.0.0.1:<port>/auth-callback; when the callback cannot be opened
// the browser address bar still retains the full URL (including code and state), which is exactly suitable for the paste callback flow.
func fallbackLoginPort() int {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 47821
	}
	return 45000 + (int(b[0])<<8|int(b[1]))%10000
}

// handleLoginOpen redirects to the upstream login page.
//
// The redirect target is reconstructed entirely by the server from the state/port in the session, and is re-validated
// scheme/host/path, and request parameters are never used to construct the URL.
func (s *Server) handleLoginOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 GET", "invalid_request_error")
		return
	}
	session := s.Login.get(strings.TrimSpace(r.URL.Query().Get("id")))
	if session == nil || session.Port == 0 {
		httpx.WriteErr(w, http.StatusNotFound, "登录会话不存在或已过期", "invalid_request_error")
		return
	}
	if time.Since(session.Created) > loginTTL {
		s.Login.remove(session.ID)
		shutdownLoginServer(session.srv)
		httpx.WriteErr(w, http.StatusGone, "登录会话已过期", "invalid_request_error")
		return
	}
	target := s.Up.SignInURL(session.State, session.Port)
	if !isSafeLoginURL(target, s.Up.Origin) {
		httpx.WriteErr(w, http.StatusInternalServerError, "登录地址校验失败", "server_error")
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// isSafeLoginURL verifies whether the login redirect target falls on the configured upstream origin.
func isSafeLoginURL(raw, origin string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	o, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != o.Scheme || !strings.EqualFold(u.Host, o.Host) {
		return false
	}
	return u.Path == "/deepai_code/desktop_auth/" || u.Path == "/deepai_code/desktop_auth"
}

// handleLoginPoll polls for the browser login result.
func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if !httpx.DecodeOptionalJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.SessionID) == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "缺少 session_id", "invalid_request_error")
		return
	}

	session := s.Login.get(body.SessionID)
	if session == nil {
		httpx.WriteErr(w, http.StatusNotFound, "登录会话不存在或已过期", "invalid_request_error")
		return
	}
	if time.Since(session.Created) > loginTTL {
		s.Login.remove(body.SessionID)
		shutdownLoginServer(session.srv)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}

	result := session.snapshotResult()
	if result == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.completeLogin(session))
}

// handleLoginCallback completes login using the "pasted-back callback URL".
//
// In Docker / remote deployments, the upstream sends the browser to http://127.0.0.1:<port>/auth-callback?code=...
// - that 127.0.0.1 is the host machine's own loopback, while the listener is inside the container (and on a random session port,
// even with -p it cannot be mapped to the container's loopback address), so this page will definitely fail to open. But in the address bar
// the code/state is valid: this endpoint exchanges the code for a Key on behalf of the user and completes adding it to the account pool, equivalent to a local callback.
func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Callback  string `json:"callback"`
		Code      string `json:"code"`
		State     string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_request_error")
		return
	}
	session := s.Login.get(strings.TrimSpace(body.SessionID))
	if session == nil {
		httpx.WriteErr(w, http.StatusNotFound, "登录会话不存在、已结束或已过期（同一个 code 只能兑换一次）", "invalid_request_error")
		return
	}
	if time.Since(session.Created) > loginTTL {
		s.Login.remove(session.ID)
		shutdownLoginServer(session.srv)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}

	code, state, err := parseLoginCallback(body.Callback, body.Code, body.State)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if !secureEqual(state, session.State) {
		httpx.WriteErr(w, http.StatusBadRequest, "回调里的 state 与本次登录不匹配：请确认粘贴的是本次登录的地址栏 URL", "invalid_request_error")
		return
	}
	if !session.beginExchange() {
		httpx.WriteErr(w, http.StatusConflict, "这次登录已经处理过（同一个 code 只能兑换一次），请重新发起登录", "invalid_request_error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	creds, xerr := s.Up.ExchangeCode(ctx, code)
	cancel()
	if xerr != nil {
		s.finishLogin(session, nil, xerr)
		_ = s.completeLogin(session) // Cleanup: delete the session, close the local listener
		status, msg := loginExchangeError(xerr)
		httpx.WriteErr(w, status, msg, "invalid_request_error")
		return
	}
	s.finishLogin(session, creds, nil)
	httpx.WriteJSON(w, http.StatusOK, s.completeLogin(session))
}

// loginExchangeError translates upstream exchange failures into messages that can be displayed directly in the console.
func loginExchangeError(err error) (int, string) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == http.StatusBadRequest, he.Status == http.StatusUnauthorized,
			he.Status == http.StatusForbidden:
			return http.StatusBadRequest, "这段回调已经用过或已过期（code 只能兑换一次），请重新发起登录"
		case he.Status == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "登录尝试过多，稍后再试"
		}
	}
	return http.StatusBadGateway, "兑换失败：" + errString(err)
}

// parseLoginCallback extracts code and state from the "pasted content".
//
// Accepts two forms: a full callback URL (copied directly from the address bar) and a bare query string (only the part after ? was copied).
// Pasting only code is rejected: state cannot be obtained that way, so it cannot be confirmed that it belongs to this login.
func parseLoginCallback(callback, code, state string) (string, string, error) {
	code, state = strings.TrimSpace(code), strings.TrimSpace(state)
	raw := strings.TrimSpace(callback)
	if raw != "" {
		c, st, ok := splitCallbackQuery(raw)
		if !ok {
			return "", "", errors.New("无法从粘贴内容里解析出 code：请粘贴地址栏里完整的 http://127.0.0.1:…/auth-callback?code=…&state=… 链接")
		}
		code, state = c, st
	}
	if code == "" || len(code) > 4096 {
		return "", "", errors.New("缺少 code：请粘贴完整的回调 URL")
	}
	if state == "" {
		return "", "", errors.New("回调里没有 state：请粘贴完整的回调 URL，不要只复制 code")
	}
	return code, state, nil
}

// splitCallbackQuery supports both paste forms: a "full URL" and a "query string only".
func splitCallbackQuery(raw string) (string, string, bool) {
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", "", false
		}
		q := u.Query()
		return q.Get("code"), q.Get("state"), q.Get("code") != ""
	case strings.Contains(raw, "="):
		q, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			return "", "", false
		}
		return q.Get("code"), q.Get("state"), q.Get("code") != ""
	}
	return "", "", false
}

// completeLogin finalizes a login for which credentials have already been exchanged: add to account pool + probe + close session.
//
// Rotation and the local callback drive it; pasting the callback URL also drives it; claim guarantees the same session
// is only added to the pool once (otherwise two concurrent entry points would write the same account twice and probe liveness twice).
func (s *Server) completeLogin(session *loginSession) map[string]any {
	result := session.claim()
	if result == nil {
		return map[string]any{"status": "pending"}
	}
	// Regardless of success or failure, the session ends here: the code is one-time, and the listening port should not stay open either.
	s.Login.remove(session.ID)
	shutdownLoginServer(session.srv)

	if result.Err != "" || result.Creds == nil {
		return map[string]any{"status": "error", "error": result.Err}
	}
	acct, err := deepai.AccountFromCredentials(result.Creds, "oauth")
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	// Browser login (including Google/Apple) only yields an API Key, without a machine fingerprint. If the local
	// fingerprint is reused, multiple accounts would share the same daily free quota, and only the first one can claim it; when adding to the pool, supplement an
	// independent UUID, consistent with the rules of action=enroll.
	if strings.TrimSpace(acct.MachineID) == "" {
		acct.MachineID = deepai.NewMachineUUID()
	}
	// Browser login uses a direct connection by default; the daily free quota is shared for the same direct-connection IP,
	// so if a proxy pool is configured, assign an egress by rotation to avoid multiple accounts crowding onto the same IP.
	// Must be written before UpsertAccount: the liveness probe after joining the pool will reuse it directly.
	if strings.TrimSpace(acct.Proxy) == "" {
		if pool := s.enrollProxyPool(); pool.Len() > 0 {
			acct.Proxy = pool.Next()
		}
	}
	isNew := s.Store.UpsertAccount(acct)
	s.persist()
	s.probeAndSaveAsync(acct)

	return map[string]any{
		"status": "ok", "new": isNew, "id": acct.ID,
		"username": acct.Username, "is_pro": acct.IsPro,
	}
}

// finishLogin records the redemption result and wakes the waiter (the first result takes effect).
func (s *Server) finishLogin(session *loginSession, creds *deepai.Credentials, err error) {
	session.setResult(&loginResult{Creds: creds, Err: errString(err)})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}
