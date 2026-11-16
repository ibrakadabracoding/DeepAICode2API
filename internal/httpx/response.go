// Package httpx collects business-agnostic HTTP utilities for the gateway: JSON
// responses, error envelopes, request body decoding, the gzip middleware, and error
// status code mapping.
//
// These functions used to live in internal/server, mixed in with 83 files and 14.6k
// lines of routing/protocol/console code. They are pure leaves: they depend only on
// the standard library, deepai's HTTPError, and store's status code constants, never
// touching *Server, so they could be extracted wholesale and tested in isolation.
package httpx

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

// WriteErr writes the gateway's unified error envelope.
//
// The code field holds the HTTP status code (historical behavior; the front end
// categorizes by error.type and does not rely on the semantics of code).
func WriteErr(w http.ResponseWriter, code int, msg, typ string) {
	WriteJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

// WriteRawJSON writes already-generated JSON bytes directly, avoiding re-serialization.
func WriteRawJSON(w http.ResponseWriter, code int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

// ContextWithTimeout adds a timeout to the request context.
func ContextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
