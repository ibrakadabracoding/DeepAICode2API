// Package logx normalizes gateway logs into "one JSON object per line".
//
// Background: when troubleshooting, what actually helps are grep-able / jq-able fields
// (request_id / account / model / latency), while free text from `log.Printf` can only be
// matched with regexes. This package does two things:
//
//  1. Setup takes over the standard library log output and turns it into JSON lines-the 50-odd
//     existing log.Printf calls become structured logs without changing a line (the msg field
//     keeps the original text), so there is no need to rewrite each call site;
//  2. Info/Warn/Error let new code write records with fields directly.
//
// Deliberately no third-party logging library: stdlib log/slog is sufficient and matches the
// "zero dependencies" goal.
package logx

import (
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var (
	mu     sync.RWMutex
	logger *slog.Logger
)

// Setup initializes global logging: JSON output goes to w (pass nil to use stderr).
//
// Must be called once before any log output (the first line of main).
func Setup(w io.Writer) {
	if w == nil {
		w = os.Stderr
	}
	mu.Lock()
	logger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	l := logger
	mu.Unlock()

	log.SetFlags(0)
	log.SetOutput(&lineWriter{logger: l, fallback: w})
}

// get returns the configured logger; falls back to the default logger when Setup has not been called.
func get() *slog.Logger {
	mu.RLock()
	l := logger
	mu.RUnlock()
	if l != nil {
		return l
	}
	return slog.Default()
}

// Info / Warn / Error write one structured log record with fields.
func Info(msg string, args ...any)  { get().Info(msg, args...) }
func Warn(msg string, args ...any)  { get().Warn(msg, args...) }
func Error(msg string, args ...any) { get().Error(msg, args...) }

// lineWriter wraps each line from the standard library log into one JSON record.
//
// The log package may deliver multiple lines in a single Write (does not happen in the current
// code, but keep it robust); this splits by line and emits each separately, so multiple lines do
// not end up in one msg and break the JSONL structure.
type lineWriter struct {
	logger   *slog.Logger
	fallback io.Writer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.logger == nil {
		return w.fallback.Write(p)
	}
	text := strings.TrimRight(string(p), "\r\n")
	if text == "" {
		return len(p), nil
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		// slog's JSON handler already provides time/level/msg fields; this only adds the original text.
		w.logger.Info(line)
	}
	return len(p), nil
}
