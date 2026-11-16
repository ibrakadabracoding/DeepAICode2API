package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// MaxRequestBody limits the inbound request body size (32 MiB).
const MaxRequestBody = 32 << 20

// ReadBodyWithLimit reads the inbound request body (capped at 32 MiB).
func ReadBodyWithLimit(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBody))
}

// DecodeOptionalJSON decodes an optional JSON request body.
//
// An empty body is valid (typically: refreshing all quotas with no filter), but **malformed JSON
// must fail**: silently treating it as an empty request lets the caller believe the parameters
// took effect (historical behavior: POST /admin/api/keys with a misspelled field created a key
// with an empty name and still returned 200).
func DecodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	WriteErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error(), "invalid_request_error")
	return false
}

// DecodeJSONRequest reads and decodes the inbound JSON request body.
//
// The three inference entry points (chat / responses / anthropic) each used to reimplement
// "read body → check limit → unmarshal → write error", so a status-code or message change was
// made in one place and missed in two. Protocol differences (OpenAI / Anthropic error envelopes)
// are injected through the fail callback, leaving only this one implementation.
func DecodeJSONRequest[T any](w http.ResponseWriter, r *http.Request, fail func(code int, msg string)) (T, bool) {
	var v T
	raw, err := ReadBodyWithLimit(w, r)
	if err != nil {
		fail(http.StatusRequestEntityTooLarge, "请求体过大")
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		fail(http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return v, false
	}
	return v, true
}
