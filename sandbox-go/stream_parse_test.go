package sandbox

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSSEReaderRejectsDataWithoutEvent pins the parser fix found by fuzzing:
// a block that carries data but no event name used to reach onEvent as an
// Event with an empty type, which a callback can neither render nor treat as
// terminal. It must be a stream error instead.
func TestSSEReaderRejectsDataWithoutEvent(t *testing.T) {
	for _, stream := range []string{
		"data:",
		"data: {\"data\":\"x\"}\n\n",
		"id: 7\ndata: {\"data\":\"x\"}\n\n",
	} {
		reader := newSSEReader(strings.NewReader(stream))
		event, err := reader.next()
		if err == nil {
			t.Fatalf("%q: event = %+v, want an error", stream, event)
		}
		var sdkErr *Error
		if !errors.As(err, &sdkErr) || sdkErr.Code != "stream" {
			t.Fatalf("%q: err = %v, want a stream Error", stream, err)
		}
	}
}

// TestSSEReaderHandlesFieldOrderAndCRLF pins two things the fuzz corpus
// relies on: the event name may follow the data lines, and CRLF line endings
// parse exactly like LF.
func TestSSEReaderHandlesFieldOrderAndCRLF(t *testing.T) {
	for name, stream := range map[string]string{
		"lf":   "id: 1\ndata: {\"data\":\"hello\"}\nevent: stdout\n\n",
		"crlf": "id: 1\r\ndata: {\"data\":\"hello\"}\r\nevent: stdout\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			reader := newSSEReader(strings.NewReader(stream))
			event, err := reader.next()
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if event.Type != EventStdout || event.Data != "hello" || event.ID != 1 {
				t.Fatalf("event = %+v, want stdout/hello/id=1", event)
			}
			if _, err := reader.next(); !errors.Is(err, io.EOF) {
				t.Fatalf("second next = %v, want EOF", err)
			}
		})
	}
}
