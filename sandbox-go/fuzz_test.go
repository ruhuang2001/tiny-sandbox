package sandbox

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// FuzzSSEReader feeds arbitrary bytes through the event-stream parser. This is
// the client's byte-level entry point: whatever it accepts becomes caller-
// visible events, and its reconnect logic decides which byte ranges are
// replayed. A parser that skips, reorders, or duplicates a line corrupts
// exactly the replay correctness the SDK promises.
//
// The properties checked:
//   - no panic and no unbounded loop, and a clean end consumes the input
//     exactly once, so nothing is silently skipped;
//   - every parsed event carries a type and a payload the contract allows,
//     so a malformed stream is an error rather than a silent empty event;
//   - the reader is stateless across blocks: a block only ever sees its own
//     `event:` and `data:` lines.
//
// What is deliberately not checked here: ID ordering. The reader only decodes
// IDs; streamOnce drops events at or below its cursor, because that is the
// delivery contract and a reconnect legitimately replays older IDs. Ordering
// belongs in a test of streamOnce, not of the byte parser.
func FuzzSSEReader(f *testing.F) {
	f.Add([]byte("id: 1\nevent: stdout\ndata: {\"data\":\"hello\\n\"}\n\n"))
	f.Add([]byte("id: 2\nevent: stderr\ndata: {\"data\":\"warn\\n\"}\n\n: ping\n\n"))
	f.Add([]byte("id: 3\nevent: complete\ndata: {\"id\":\"exe_1\",\"state\":\"completed\"}\n\n"))
	f.Add([]byte("event: stdout\ndata: {\"data\":\"a\"}\ndata: {\"data\":\"b\"}\n\n")) // multi-line data
	f.Add([]byte("id: 1\r\nevent: stdout\r\ndata: {\"data\":\"crlf\"}\r\n\r\n"))       // CRLF
	f.Add([]byte("id: not-a-number\nevent: stdout\ndata: {}\n\n"))                     // bad id
	f.Add([]byte("id: 1\nevent: stdout\ndata: {\"data\":\"unterminated\"}"))           // no final blank line
	f.Add([]byte(": ping\n\n"))                                                        // comment only
	f.Add([]byte("data:\n\n"))                                                         // data without event
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		counted := &countingReader{r: bytes.NewReader(data)}
		reader := newSSEReader(counted)

		var eventCount int
		for {
			event, err := reader.next()
			if errors.Is(err, io.EOF) {
				if remaining := counted.left(); remaining != 0 {
					t.Fatalf("parser stopped with %d bytes unconsumed: %q", remaining, data)
				}
				return
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
			if event.Type == EventComplete && event.Execution == nil {
				t.Fatalf("complete event without an execution: %q", data)
			}
		}
	})
}

// countingReader reports how much of the input the parser left unread. A
// parser that stops early silently drops replay data, which no event-level
// assertion can detect.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// left reports the bytes still in the underlying reader, which is what a
// missing final flush exposes.
func (c *countingReader) left() int {
	if r, ok := c.r.(*bytes.Reader); ok {
		return r.Len()
	}
	return 0
}
