package daemon

import (
	"context"
	"sync"
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
// Bounds come from api.md: at most MaxOutputBytes of retained output, at most
// MaxOutputEvents output events, and at most MaxEventDataBytes per chunk. Once
// either bound is hit, the log appends one truncated event and discards all
// later output; the process keeps running.
type eventLog struct {
	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every append

	events        []Event
	nextID        uint64
	retainedBytes int
	outputEvents  int
	truncated     bool
	complete      bool

	stdout []byte
	stderr []byte
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
func (l *eventLog) appendOutput(kind EventType, chunk []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.truncated || l.complete || len(chunk) == 0 {
		return
	}
	for len(chunk) > 0 {
		n := min(len(chunk), MaxEventDataBytes)
		if l.outputEvents >= MaxOutputEvents || l.retainedBytes+n > MaxOutputBytes {
			l.truncateLocked()
			return
		}
		data := append([]byte(nil), chunk[:n]...)
		l.appendLocked(Event{Type: kind, Data: data})
		l.outputEvents++
		l.retainedBytes += n
		if kind == EventStdout {
			l.stdout = append(l.stdout, data...)
		} else {
			l.stderr = append(l.stderr, data...)
		}
		chunk = chunk[n:]
	}
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

// output returns the retained stdout and stderr and whether output was
// truncated.
func (l *eventLog) output() (stdout, stderr []byte, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.stdout...), append([]byte(nil), l.stderr...), l.truncated
}

// lastEventID returns the ID of the newest event, 0 when there is none.
func (l *eventLog) lastEventID() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextID
}
