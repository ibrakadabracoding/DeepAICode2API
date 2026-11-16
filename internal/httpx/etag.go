package httpx

import "strings"

// ETagMatches reports whether If-None-Match contains the target ETag.
//
// Supports comma-separated lists, the weak validator prefix (W/), and "*" (matches
// any current representation). Console assets and /admin/api/* 304 negotiation
// share this implementation.
func ETagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == want {
			return true
		}
	}
	return false
}
