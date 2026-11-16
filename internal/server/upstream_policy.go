package server

import (
	"time"

	"deepaicode2api/internal/deepai"
)

// upstreamAction is the action the gateway should take after an upstream rejection.
//
// The decision was extracted from openModel's if chain to:
// - make "which status code / error code should switch accounts, block, wait, or return" a table-driven testable
// pure function (account switching and terminal state semantics have changed many times historically);
// - when adding a new error code (e.g. theos_exhausted on 2026-11-13), only change one place;
// - openModel only "executes the action", and branches no longer grow with error codes.
type upstreamAction int

const (
	// actRetryOther retries with another account (ordinary 401/403 failures, 429, 5xx, etc.).
	actRetryOther upstreamAction = iota
	// actReturn returns the upstream error directly to the caller (problem with the request itself / missing machine fingerprint).
	actReturn
	// actBlockAndRetry marks blocked then switches accounts (email not verified: the account's login state itself is invalid).
	actBlockAndRetry
	// actBlockAndReturn marks blocked then returns directly (account-level policy interception).
	actBlockAndReturn
	// actProbeLane probes the lane first; if a new lane is hit, switches lanes and retries.
	actProbeLane
	// actWaitCapacity capacity rejection: if the pool has an idle account, switch accounts; only wait for Retry-After when the whole pool is busy.
	actWaitCapacity
	// actQuotaExhausted the account's daily quota is used up: mark until refresh_at and switch accounts.
	actQuotaExhausted
	// actShapeFallback the request body shape was rejected: add the desktop shape and retry once with the same account.
	actShapeFallback
)

// classifyUpstreamError is a pure function: error → action.
//
// The decision order matters (account terminal state first, then lane/quota/capacity, and only last general retryability).
func classifyUpstreamError(e *deepai.HTTPError) upstreamAction {
	switch {
	case e == nil:
		return actReturn
	case e.EmailUnverified():
		return actBlockAndRetry
	case e.SocialLoginRequired():
		return actBlockAndReturn
	case e.MachineRequired():
		return actReturn
	case e.LaneUnsupported():
		return actProbeLane
	case e.QuotaExhausted():
		return actQuotaExhausted
	case e.CapacityWaiting():
		return actWaitCapacity
	case e.BodyRejected():
		return actShapeFallback
	case e.Retryable():
		return actRetryOther
	}
	return actReturn
}

// quotaExhaustedFallback is the recovery wait when theos_exhausted does not provide refresh_at.
//
// The upstream message is "They refresh at <RFC3339>", which is usually parseable; when it cannot be parsed, use 6h,
// on the same order as check_in_refused's "Try again tomorrow".
const quotaExhaustedFallback = 6 * time.Hour
