package daemon

import (
	"context"
	"sync"
	"unicode/utf8"
)

// EventType is the name of an SSE event, as documented in api.md.
type EventType string

// Event types carried by the execution stream.
const (
	EventStdout    EventType = "stdout"
	EventStderr    EventType = "stderr"
	EventTruncated EventType = "truncated"
	EventComplete  EventType = "complete"
)

// Event is one entry in an execution's event log.
type Event struct {
	// ID is the SSE event ID: decimal, starting at 1, increasing by 1.
	ID uint64
	// Type is stdout, stderr, truncated, or complete.
	Type EventType
	// Data is the chunk payload of a stdout or stderr event.
	Data []byte
	// LimitBytes accompanies a truncated event: the retention limit that was
	// hit, counting bytes and events together is not possible, so it is the
	// byte limit.
	LimitBytes int
	// Execution is set on the complete event and carries the final record,
	// including retained output.
	Execution *Execution
}

// eventLog is the bounded, append-only record of one execution's output plus
// its terminal events. It is the single source of truth for both the stream
// and the retained stdout/stderr, so a replay from ID 0 reproduces them.
//
// The events are the only copy of the output. The retained stdout and stderr a
// caller reads are assembled from them on demand, which keeps one execution at
// roughly MaxOutputBytes instead of two or three times that. Assembling costs a
// pass over the log; a snapshot does it once, and the runner writes it once into
// the complete event.
//
// Bounds come from api.md: at most MaxOutputBytes of retained output, at most
// MaxOutputEvents output events, and at most MaxEventDataBytes per chunk. Once
// either bound is hit, the log appends one truncated event and discards all
// later output; the process keeps running.
type eventLog struct {
	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every append

	events       []Event
	nextID       uint64
	retained     int
	outputEvents int
	truncated    bool
	complete     bool
}

func newEventLog() *eventLog {
	return &eventLog{changed: make(chan struct{})}
}

// notifyLocked wakes every waiter. Callers hold l.mu.
func (l *eventLog) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// appendOutput records one chunk of process output. A chunk larger than the
// per-event limit is split, so no single event exceeds 32 KiB. Once a bound is
// hit the chunk is discarded whole, keeping "stored output is exactly the data
// delivered in events" true.
//
// A cut never splits a UTF-8 sequence. Output is delivered as JSON strings,
// and each event is encoded on its own, so a half rune at the end of one
// event would be replaced by U+FFFD on the wire and could never be rejoined
// with its other half. The cut therefore backs up to the last rune boundary,
// leaving at most three bytes for the next event.
func (l *eventLog) appendOutput(kind EventType, chunk []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.truncated || l.complete || len(chunk) == 0 {
		return
	}
	for len(chunk) > 0 {
		n := min(len(chunk), MaxEventDataBytes)
		if n < len(chunk) {
			// Find the start of the rune the limit fell inside. A chunk of
			// invalid bytes has no boundary; the limit then stands, and the
			// JSON encoder turns those bytes into U+FFFD, as it must.
			if cut := runeBoundary(chunk, n); cut > 0 {
				n = cut
			}
		}
		if l.outputEvents >= MaxOutputEvents || l.retained+n > MaxOutputBytes {
			l.truncateLocked()
			return
		}
		data := append([]byte(nil), chunk[:n]...)
		l.appendLocked(Event{Type: kind, Data: data})
		l.outputEvents++
		l.retained += n
		chunk = chunk[n:]
	}
}

// runeBoundary returns the largest index i in (0, limit] at which chunk[i]
// starts a rune, so a cut at i never splits a UTF-8 sequence. It returns 0
// when the bytes before limit contain no rune start, which leaves the caller's
// limit unchanged.
func runeBoundary(chunk []byte, limit int) int {
	if limit >= len(chunk) {
		return limit
	}
	for i := limit; i > 0; i-- {
		if utf8.RuneStart(chunk[i]) {
			return i
		}
	}
	return 0
}

// incompleteRuneTail returns how many trailing bytes of chunk form an
// incomplete UTF-8 sequence; 0 means the chunk ends on a rune boundary. The
// caller holds those bytes back until the next read completes them, so a rune
// is never split across two events.
func incompleteRuneTail(chunk []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(chunk); i++ {
		start := len(chunk) - i
		if !utf8.RuneStart(chunk[start]) {
			continue
		}
		if utf8.FullRune(chunk[start:]) {
			return 0
		}
		return i
	}
	return 0
}

// truncateLocked records that output was dropped. It is sent at most once.
func (l *eventLog) truncateLocked() {
	l.truncated = true
	l.appendLocked(Event{Type: EventTruncated, LimitBytes: MaxOutputBytes})
	l.outputEvents = MaxOutputEvents // keep both bounds visibly exhausted
}

// appendComplete appends the terminal event. It is always last.
func (l *eventLog) appendComplete(ex Execution) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.complete {
		return
	}
	l.complete = true
	l.appendLocked(Event{Type: EventComplete, Execution: &ex})
}

func (l *eventLog) appendLocked(ev Event) {
	l.nextID++
	ev.ID = l.nextID
	l.events = append(l.events, ev)
	l.notifyLocked()
}

func (l *eventLog) eventsAfterLocked(after uint64) ([]Event, bool) {
	// Event IDs are dense from 1, so ID n lives at index n-1.
	i := 0
	if after < l.nextID {
		i = int(after)
	} else {
		i = len(l.events)
	}
	events := append([]Event(nil), l.events[i:]...)
	// The log stores no second copy of the output: the complete event carries
	// the terminal record without it, and the copy a client sees is assembled
	// here, once per delivery. Rebuilding it costs one pass over at most
	// MaxOutputEvents events.
	if n := len(events); n > 0 && events[n-1].Type == EventComplete && events[n-1].Execution != nil {
		stdout, stderr, truncated := l.outputLocked()
		ex := *events[n-1].Execution
		ex.Stdout, ex.Stderr, ex.OutputTruncated = string(stdout), string(stderr), truncated
		events[n-1].Execution = &ex
	}
	// done means "the complete event was delivered in this batch": the log is
	// complete and this batch runs to the end of it. Returning done only when
	// nothing remained would make the handler wait one ping interval after
	// delivering the final event.
	return events, l.complete && i+len(events) >= len(l.events)
}

// wait returns events after after, blocking until at least one is available,
// the log completes, or ctx is done.
func (l *eventLog) wait(ctx context.Context, after uint64) ([]Event, bool, error) {
	for {
		l.mu.Lock()
		events, done := l.eventsAfterLocked(after)
		ch := l.changed
		l.mu.Unlock()

		if len(events) > 0 || done {
			return events, done, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-ch:
		}
	}
}

// truncatedOutput reports whether output was dropped, without copying it.
func (l *eventLog) truncatedOutput() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.truncated
}

// retainedBytes reports how many bytes of output the log holds.
func (l *eventLog) retainedBytes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retained
}

// output returns the retained stdout and stderr and whether output was
// truncated. It copies, so callers that only need the flag use
// truncatedOutput; a listing does.
func (l *eventLog) output() (stdout, stderr []byte, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.outputLocked()
}

// outputLocked assembles the retained output from the events, which are its
// only copy. Callers hold l.mu.
func (l *eventLog) outputLocked() (stdout, stderr []byte, truncated bool) {
	for _, ev := range l.events {
		switch ev.Type {
		case EventStdout:
			stdout = append(stdout, ev.Data...)
		case EventStderr:
			stderr = append(stderr, ev.Data...)
		}
	}
	return stdout, stderr, l.truncated
}

// lastEventID returns the ID of the newest event, 0 when there is none.
func (l *eventLog) lastEventID() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextID
}
