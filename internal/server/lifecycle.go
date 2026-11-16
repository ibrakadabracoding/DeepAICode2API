package server

import (
	"context"
	"log"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Process lifecycle ---

// Close stops background tasks and waits for scheduled persist-to-disk work to finish.
//
// Why this is needed: quota re-probe / pool-entry liveness probe / on-demand re-probe all follow the pattern of "write memory first, then Save()" in a
// goroutine. In the old implementation, main called Save and exited directly after shutting down the HTTP listener, so background tasks could
// persist to disk only after that final Save (or be killed by the process), manifesting as "the quota just probed is gone after restart".
//
// Semantics:
// - Close stopCh to interrupt backoff sleeps of up to 12 minutes in background tasks;
// - Wait for quotaWG (bounded by ctx; on timeout only log, without blocking exit);
// - Close idle connections of each egress to avoid leaving half-open connections when the process exits.
func (s *Server) Close(ctx context.Context) {
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})

	done := make(chan struct{})
	go func() {
		s.quotaWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The exit flow must not wait indefinitely: probing itself has a 20s timeout, so this is usually not hit.
		log.Printf("[shutdown] 后台任务未在期限内收尾，继续退出（未落盘的探测结果会丢失）")
	}

	s.closeIdleConnections()
}

// stopped returns the stop signal channel (closed on Close).
//
// May be nil (zero-value Server, mainly in tests); callers must tolerate it.
func (s *Server) stopped() <-chan struct{} { return s.stopCh }

// sleepOrStop waits for d and returns false when the stop signal is received.
func (s *Server) sleepOrStop(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.stopped():
		return false
	case <-timer.C:
		return true
	}
}

// closeIdleConnections closes idle connections of all upstream clients (including each egress proxy client).
func (s *Server) closeIdleConnections() {
	closeIdle := func(c *deepai.Client) {
		if c == nil || c.HTTP == nil {
			return
		}
		c.HTTP.CloseIdleConnections()
	}
	closeIdle(s.Up)

	s.proxyMu.Lock()
	clients := make([]*deepai.Client, 0, len(s.proxyClients))
	for _, c := range s.proxyClients {
		clients = append(clients, c)
	}
	s.proxyMu.Unlock()
	for _, c := range clients {
		closeIdle(c)
	}
}
