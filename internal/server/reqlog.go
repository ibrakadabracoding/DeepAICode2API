package server

import (
	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/store"
	"sync/atomic"
	"time"
)

// record records metadata for one request.
// reqLog is the log context for one request.
//
// The old record() took 11 positional parameters (key/acct/model/endpoint/stream/status/
// start/inLen/outLen/usage/err); more than twenty call sites relied entirely on memorizing the parameter order,
// and a mistake would not be caught by the compiler. Now invariants are fixed at construction time, and call sites only supply the varying parts.
type reqLog struct {
	s        *Server
	key      string
	acct     *deepai.Account
	model    string
	endpoint string
	stream   bool
	start    time.Time
	inLen    int

	// state is the shared flag for "whether already recorded". reqLog is a value type (passed by value across handlers),
	// so the flag must live behind a pointer: done may be called once by different branches, and only the first call takes effect
	// so that the same request is not logged twice.
	state *reqLogState
}

// reqLogState is the state shared across copies of reqLog.
type reqLogState struct {
	recorded atomic.Bool
}

// newReqLog fixes the common fields of one request. acct may be nil (no account selected yet).
func (s *Server) newReqLog(key string, acct *deepai.Account, model, endpoint string, stream bool, start time.Time, inLen int) reqLog {
	return reqLog{
		s: s, key: key, acct: acct, model: model,
		endpoint: endpoint, stream: stream, start: start, inLen: inLen,
		state: &reqLogState{},
	}
}

// done persists one request log; a nil err is treated as success (HTTP 200), otherwise the status code is derived from the error.
//
// Idempotent: the same request is recorded only once. This way, "a branch that returns early forgets to record / an extra layer of defer
// fallback" will not cause double counting, and callers do not need to check whether it has been recorded first.
func (l reqLog) done(outLen int, usage Usage, err error) {
	l.doneStatus(httpx.StatusOf(err), outLen, usage, err)
}

// doneStatus is the same as done, but the status code is provided by the caller.
//
// Used for scenarios where "the response has already been written to the client with a certain code, but the log still needs to explain the reason": typically when the upstream returns
// non-JSON, and we pass it through as-is as 200 - the log must retain both 200 (what the client actually received)
// and the failure reason (Record counts e.Err != "" as a failure, so it is not lost from the statistics).
func (l reqLog) doneStatus(status int, outLen int, usage Usage, err error) {
	if l.s == nil || l.state == nil {
		return
	}
	if !l.state.recorded.CompareAndSwap(false, true) {
		return
	}
	e := store.LogEntry{
		Time:         time.Now().Unix(),
		Key:          mask(l.key),
		Model:        l.model,
		Endpoint:     l.endpoint,
		Stream:       l.stream,
		Status:       status,
		LatencyMS:    time.Since(l.start).Milliseconds(),
		PromptLen:    l.inLen,
		OutputLen:    outLen,
		TokensIn:     usage.PromptTokens,
		TokensOut:    usage.CompletionTokens,
		CachedTokens: usage.CachedTokens,
		Cost:         usage.Cost,
	}
	if l.acct != nil {
		e.Account = l.acct.ID
	}
	if err != nil {
		e.Err = err.Error()
	}
	l.s.Store.Record(e, outLen)
}

// apiKey returns the calling key for this request.
//
// Only for in-process logic such as shape memory / metrics; logs and statistics persisted to disk use the redacted view of the key,
// do not write the full key into logs.
func (l reqLog) apiKey() string { return l.key }
