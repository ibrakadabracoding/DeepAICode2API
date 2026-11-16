package httpx

import (
	"context"
	"errors"
	"net/http"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/store"
)

// StatusOf maps a request error to the status code used in logs.
//
// The real status in HTTPError must take priority: upstream 400 (body/model not
// accepted), 426 (lane), and 429 (capacity) are returned verbatim to the client by
// writeUpstreamErr; if they were uniformly logged as 502, the console's "status ≥
// 400" filter, error rate, and attribution would all be wrong.
func StatusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var he *deepai.HTTPError
	if errors.As(err, &he) && he.Status >= 400 && he.Status < 600 {
		return he.Status
	}
	// These two must match writeUpstreamErr's actual response, otherwise the log
	// status codes would lie:
	//   - Client disconnected: writeUpstreamErr writes nothing (the connection is
	//     already gone); log 499 and do not count it as a failure (see store.Record).
	//   - Our own timeout: the response code is 504 but the log says 502, mixing
	//     "upstream 5xx" with "request timeout".
	if errors.Is(err, context.Canceled) {
		return store.StatusClientGone
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
