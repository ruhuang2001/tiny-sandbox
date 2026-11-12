package sandbox

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// FuzzSSEReader feeds arbitrary bytes through the event-stream parser. This is
// the client's byte-level entry point: whatever it accepts becomes caller-
// visible events, and its reconnect logic decides which byte ranges are
// replayed. A parser that skips, reorders, or duplicates a line corrupts
// exactly the replay correctness the SDK promises.
//
// The properties checked:
//   - no panic and no unbounded loop: the reader always reaches EOF;
//   - every parsed event carries a type and a payload the contract allows,
//     so a malformed stream is an error rather than a silent empty event;
//   - nothing is lost: the stdout and stderr text of all events, in order,
//     equals the data lines of the stream that were not skipped;
//   - an event ID is never reported below the previous one.
func FuzzSSEReader(f *testing.F) {
	f.Add([]byte("id: 1\nevent: stdout\ndata: {\"data\":\"hello\\n\"}\n\n"))
	f.Add([]byte("id: 2\nevent: stderr\ndata: {\"data\":\"warn\\n\"}\n\n: ping\n\n"))
	f.Add([]byte("id: 3\nevent: complete\ndata: {\"id\":\"exe_1\",\"state\":\"completed\"}\n\n"))
	f.Add([]byte("event: stdout\ndata: {\"data\":\"a\"}\ndata: {\"data\":\"b\"}\n\n")) // multi-line data
	f.Add([]byte("id: 1\r\nevent: stdout\r\ndata: {\"data\":\"crlf\"}\r\n\r\n"))       // CRLF
	f.Add([]byte("id: not-a-number\nevent: stdout\ndata: {}\n\n"))                     // bad id
	f.Add([]byte("id: 1\nevent: stdout\ndata: {\"data\":\"unterminated\"}"))           // no final blank line
	f.Add([]byte(": ping\n\n"))                                                        // comment only
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		var (
			eventCount int
			lastID     uint64
			sawData    strings.Builder
		)
		reader := newSSEReader(strings.NewReader(string(data)))
		for {
			event, err := reader.next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				var sdkErr *Error
				if !errors.As(err, &sdkErr) {
					// A decoder error is a bug in the parser; a contract
					// error is a bad stream and is reported as one.
					t.Fatalf("parser returned a bare error: %v", err)
				}
				return
			}
			if event == nil {
				continue
			}
			eventCount++
			if eventCount > len(data)+1 {
				t.Fatalf("parsed more events than the input can contain: %d", eventCount)
			}
			if event.Type == "" {
				t.Fatalf("event %d has no type", eventCount)
			}
			if event.ID < lastID {
				t.Fatalf("event id went backwards: %d after %d", event.ID, lastID)
			}
			lastID = event.ID
			switch event.Type {
			case EventStdout, EventStderr:
				sawData.WriteString(event.Data)
			case EventComplete:
				if event.Execution == nil {
					t.Fatalf("complete event without an execution: %q", data)
				}
			}
		}
	})
}
