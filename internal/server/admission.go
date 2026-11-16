package server

import (
	"time"
)

// admissionWait / admissionPoll are the queue limit and polling interval used when
// the pool temporarily has no free slot.
//
// Previously a lack of slots returned 429 immediately (see docs/performance.md §14:
// at 14 accounts / 28 concurrent requests the success rate was only 48~70%). Brief
// queuing smooths out bursts without hanging the caller.
const (
	admissionWait = 2 * time.Second
	admissionPoll = 50 * time.Millisecond
)
