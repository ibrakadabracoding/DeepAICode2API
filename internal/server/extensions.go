package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// handleTranscriptions corresponds to OpenAI /v1/audio/transcriptions.
//
// The upstream only accepts audio/webm and audio/mp4, with the field name voiceRecording;
// The client's original bytes and MIME are preserved here; only field mapping is performed.
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "multipart 解析失败: "+err.Error(), "invalid_request_error")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "缺少 file 字段", "invalid_request_error")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8<<20+1))
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "读取音频失败: "+err.Error(), "invalid_request_error")
		return
	}
	if len(data) > 8<<20 {
		httpx.WriteErr(w, http.StatusRequestEntityTooLarge, "音频超过 8 MB", "invalid_request_error")
		return
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/webm"
	}
	// The upstream only recognizes the real MIME prefix, so the codecs parameter is stripped.
	if idx := strings.IndexByte(contentType, ';'); idx > 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	mode := strings.TrimSpace(r.FormValue("response_format"))
	key := extractKey(r)

	acct := s.nextAccount(r)
	if acct == nil {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "没有可用账号", "invalid_request_error")
		return
	}
	ctx, cancel := httpx.ContextWithTimeout(r, 60*time.Second)
	text, err := s.clientFor(acct).Transcribe(ctx, acct, header.Filename, contentType, data)
	cancel()
	rl := s.newReqLog(key, acct, "whisper-1", "audio", false, start, len(data))
	if err != nil {
		s.Store.MarkAccountFailure(acct.ID, err)
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	s.Store.MarkAccountResult(acct.ID, true, "")
	rl.done(len(text), Usage{}, nil)

	if mode == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(text))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"text": text})
}

// handleWebSearch proxies the upstream web search capability.
func (s *Server) handleWebSearch(w http.ResponseWriter, r *http.Request) {
	s.proxyExtension(w, r, deepai.PathWebSearch)
}

// handleFindService proxies the upstream service recommendation capability.
func (s *Server) handleFindService(w http.ResponseWriter, r *http.Request) {
	s.proxyExtension(w, r, deepai.PathFindService)
}

func (s *Server) proxyExtension(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	start := time.Now()
	key := extractKey(r)
	endpoint := strings.TrimPrefix(path, "/")
	body, err := httpx.ReadBodyWithLimit(w, r)
	if err != nil {
		httpx.WriteErr(w, http.StatusRequestEntityTooLarge, "请求体过大", "invalid_request_error")
		return
	}
	if !json.Valid(body) {
		httpx.WriteErr(w, http.StatusBadRequest, "请求体必须是合法 JSON", "invalid_request_error")
		return
	}

	acct := s.nextAccount(r)
	if acct == nil {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "没有可用账号", "invalid_request_error")
		return
	}
	ctx, cancel := httpx.ContextWithTimeout(r, 60*time.Second)
	defer cancel()

	var raw []byte
	switch path {
	case deepai.PathWebSearch:
		raw, err = s.clientFor(acct).WebSearch(ctx, acct, body)
	case deepai.PathFindService:
		raw, err = s.clientFor(acct).FindService(ctx, acct, body)
	default:
		err = io.ErrUnexpectedEOF
	}
	rl := s.newReqLog(key, acct, "-", endpoint, false, start, len(body))
	if err != nil {
		s.Store.MarkAccountFailure(acct.ID, err)
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	s.Store.MarkAccountResult(acct.ID, true, "")
	rl.done(len(raw), Usage{}, nil)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(raw))
}

// theosCacheTTL is the snapshot cache duration for /v1/theos.
//
// /theos is also the check-in channel: high-frequency polling from the panel / scripts continuously consumes the upstream's daily rate-limited
// "restart check-in" count (403 check_in_refused). Within 10 minutes, the cache is returned directly;
// when a forced refresh is needed, pass ?force=1.
const theosCacheTTL = 10 * time.Minute

// handleTheos returns the Theos quota of the current account (served from cache within 10 minutes).
func (s *Server) handleTheos(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := extractKey(r)
	acct := s.nextAccount(r)
	if acct == nil {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "没有可用账号", "invalid_request_error")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if !force && acct.Theos != nil && acct.Theos.CheckedAt > 0 &&
		time.Since(time.Unix(acct.Theos.CheckedAt, 0)) < theosCacheTTL {
		rl := s.newReqLog(key, acct, "-", "theos", false, start, 0)
		rl.done(0, Usage{}, nil)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"account":  acct.ID,
			"username": acct.Username,
			"theos":    acct.Theos,
			"cached":   true,
		})
		return
	}
	ctx, cancel := httpx.ContextWithTimeout(r, 30*time.Second)
	defer cancel()
	al, err := s.clientFor(acct).Theos(ctx, acct)
	rl := s.newReqLog(key, acct, "-", "theos", false, start, 0)
	if err != nil {
		s.Store.MarkAccountFailure(acct.ID, err)
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	s.Store.SetAccountAllowance(acct.ID, al, nil)
	rl.done(0, Usage{}, nil)
	s.persist()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"account":  acct.ID,
		"username": acct.Username,
		"theos":    al,
	})
}

// handleUsage returns the subscription / usage summary of the current account.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := extractKey(r)
	acct := s.nextAccount(r)
	if acct == nil {
		httpx.WriteErr(w, http.StatusServiceUnavailable, "没有可用账号", "invalid_request_error")
		return
	}
	ctx, cancel := httpx.ContextWithTimeout(r, 30*time.Second)
	defer cancel()
	usage, err := s.clientFor(acct).UsageSummary(ctx, acct)
	rl := s.newReqLog(key, acct, "-", "usage", false, start, 0)
	if err != nil {
		s.Store.MarkAccountFailure(acct.ID, err)
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	s.Store.MarkAccountResult(acct.ID, true, "")
	rl.done(0, Usage{}, nil)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"account":  acct.ID,
		"username": acct.Username,
		"usage":    usage,
	})
}
