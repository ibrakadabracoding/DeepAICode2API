package deepai

import (
	"bufio"
	"io"
	"strings"
)

// SSEEvent is a Server-Sent Events event.
type SSEEvent struct {
	ID    string
	Event string
	Data  string
}

// ReadSSE parses the event stream according to the SSE specification.
//
// The upstream may send multiple data lines within the same event; the specification requires joining them with "\n" and dispatching them as a whole.
func ReadSSE(r io.Reader, fn func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var (
		id, event string
		data      []string
		hasData   bool
	)
	dispatch := func() error {
		if !hasData && event == "" && id == "" {
			return nil
		}
		ev := SSEEvent{ID: id, Event: event, Data: strings.Join(data, "\n")}
		id, event, data, hasData = "", "", nil, false
		return fn(ev)
	}

	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // Comment / heartbeat
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "event":
			event = value
		case "data":
			data = append(data, value)
			hasData = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}

// WriteSSE writes a single SSE data event.
func WriteSSE(w io.Writer, payload []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n\n"))
	return err
}

// WriteSSEEvent writes an SSE event with an event name (used by Anthropic / Responses).
func WriteSSEEvent(w io.Writer, event string, payload []byte) error {
	if event != "" {
		if _, err := io.WriteString(w, "event: "+event+"\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}
