package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StreamExecution connects to the event stream and calls onEvent for stdout,
// stderr, truncated, and complete, in order, each exactly once.
//
// On a disconnect it reconnects with Last-Event-ID set to the last delivered
// ID and drops any event not newer than it, which makes the replay lossless
// and duplicate-free. Reconnects use the retry backoff and reset their attempt
// counter whenever a new event arrives; after MaxRetries consecutive failures
// the call returns an error.
//
// If onEvent returns an error the stream closes and the error is returned. The
// execution keeps running: a rendering bug must not kill remote work.
func (c *Client) StreamExecution(ctx context.Context, sandboxID, executionID string, onEvent func(Event) error) (*Execution, error) {
	if onEvent == nil {
		onEvent = func(Event) error { return nil }
	}
	lastID := uint64(0)
	failures := 0

	for {
		ex, complete, err := c.streamOnce(ctx, sandboxID, executionID, lastID, onEvent, &lastID)
		if complete {
			return ex, nil
		}
		if ctx.Err() != nil {
			return c.cancelAfterContext(ctx, sandboxID, executionID, err)
		}
		var sdkErr *Error
		if errors.As(err, &sdkErr) && sdkErr.Code == "callback" {
			return nil, err
		}
		if err == nil || !retryableStreamError(err) {
			return nil, err
		}

		failures++
		if failures > c.maxRetries {
			return nil, &Error{
				StatusCode: statusOf(err),
				Code:       "stream",
				Message:    fmt.Sprintf("sandbox: stream failed %d times without delivering an event", failures),
				Cause:      err,
			}
		}
		if !sleep(ctx, backoff(failures-1, 0)) {
			return c.cancelAfterContext(ctx, sandboxID, executionID, err)
		}
	}
}

// streamOnce reads one connection. It reports complete when the terminal
// event arrived; otherwise the returned error says whether another attempt is
// worth making.
func (c *Client) streamOnce(ctx context.Context, sandboxID, executionID string, lastID uint64, onEvent func(Event) error, cursor *uint64) (ex *Execution, complete bool, err error) {
	url := c.domain + executionPath(sandboxID, executionID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: build stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, &Error{Code: "transport", Message: "sandbox: stream failed", Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, false, statusError(&response{status: resp.StatusCode, body: data})
	}

	reader := newSSEReader(resp.Body)
	for {
		event, err := reader.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The server closes only after complete, so an EOF before it
				// means the connection dropped.
				return nil, false, &Error{Code: "transport", Message: "sandbox: stream ended before the execution completed", Cause: err}
			}
			return nil, false, err
		}
		if event == nil {
			continue
		}
		// A reconnect replays from the last ID; anything at or below the
		// cursor was already delivered.
		if event.ID <= *cursor {
			continue
		}
		*cursor = event.ID

		if event.Type == EventComplete {
			// parseEvent already decoded the payload into Execution; the raw
			// Data field is only used by output events.
			if event.Execution == nil {
				return nil, false, &Error{Code: "stream", Message: "sandbox: the complete event carried no execution"}
			}
			if err := onEvent(*event); err != nil {
				return nil, false, callbackError(err)
			}
			return event.Execution, true, nil
		}
		if err := onEvent(*event); err != nil {
			return nil, false, callbackError(err)
		}
	}
}

// cancelAfterContext cancels the execution with a fresh context, because the
// caller's can no longer carry a request. Success is ErrCanceled with the
// Execution; failure is ErrCancelUnconfirmed, so nothing runs unnoticed.
func (c *Client) cancelAfterContext(_ context.Context, sandboxID, executionID string, cause error) (*Execution, error) {
	fresh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ex, err := c.CancelExecution(fresh, sandboxID, executionID)
	if err != nil {
		return nil, &Error{
			Code:      "cancel_unconfirmed",
			Message:   "sandbox: the caller ended the stream but the execution could not be canceled",
			Execution: &Execution{ID: executionID, SandboxID: sandboxID},
			Cause:     err,
		}
	}
	return ex, &Error{
		Code:      string(ReasonCanceled),
		Message:   "sandbox: execution canceled",
		Execution: ex,
		Cause:     cause,
	}
}

// callbackError wraps an onEvent failure, which is never retried.
func callbackError(err error) error {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return err
	}
	return &Error{Code: "callback", Message: "sandbox: event handler failed", Cause: err}
}

// retryableStreamError decides whether a broken connection is worth another
// attempt. A contract error such as 404 is not.
func retryableStreamError(err error) bool {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.Retryable()
	}
	return true
}

// statusOf extracts a status for the final stream error.
func statusOf(err error) int {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.StatusCode
	}
	return 0
}

// ---------------------------------------------------------------- SSE parsing

// sseReader parses the event-stream format into Events. Multi-line data fields
// are joined with newlines, and comment lines (": ping") are skipped.
type sseReader struct {
	scanner *bufio.Scanner
	// pending holds an event read from the final, unterminated block; atEOF
	// makes the call after it report EOF.
	pending *Event
	atEOF   bool
}

// maxEventBytes bounds one SSE line.
//
// A complete event carries the whole Execution, which has two parts that can
// carry arbitrary bytes: `command` (accepted from a request body of up to
// 1 MiB) and the retained stdout and stderr (1 MiB together). JSON escaping
// turns one control byte into six characters ("\u0001"), and encoding/json
// also escapes <, > and & the same way, so each part can expand sixfold:
// 6 MiB of command plus 6 MiB of output, plus the other fields. 16 MiB covers
// that with room to spare. The buffer grows lazily, so a small event costs a
// small allocation.
const maxEventBytes = 16 << 20

func newSSEReader(r io.Reader) *sseReader {
	scanner := bufio.NewScanner(r)
	// A cap below the worst case makes a command that printed control bytes
	// fail to stream even though it completed.
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	return &sseReader{scanner: scanner}
}

// next returns the next event, or nil for a comment or an empty block.
func (r *sseReader) next() (*Event, error) {
	if r.pending != nil {
		event := r.pending
		r.pending = nil
		r.atEOF = true
		return event, nil
	}
	if r.atEOF {
		return nil, io.EOF
	}
	var (
		event   Event
		data    strings.Builder
		hasData bool
	)
	for r.scanner.Scan() {
		line := r.scanner.Text()
		switch {
		case line == "":
			if hasData || event.Type != "" {
				return parseEvent(&event, data.String())
			}
			continue
		case strings.HasPrefix(line, ":"):
			continue // comment, including the ping
		case strings.HasPrefix(line, "id:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			id, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, &Error{Code: "stream", Message: "sandbox: event id is not an integer: " + value}
			}
			event.ID = id
		case strings.HasPrefix(line, "event:"):
			event.Type = EventType(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			hasData = true
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	// EOF without a trailing blank line still ends an event that was read.
	if hasData || event.Type != "" {
		event, parseErr := parseEvent(&event, data.String())
		if parseErr != nil {
			return nil, parseErr
		}
		// The next call reports EOF.
		r.pending = event
		return event, nil
	}
	r.atEOF = true
	return nil, io.EOF
}

// parseEvent fills the typed payload of one event.
func parseEvent(event *Event, data string) (*Event, error) {
	switch event.Type {
	case EventStdout, EventStderr:
		var payload struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil, &Error{Code: "stream", Message: "sandbox: could not parse an output event", Cause: err}
		}
		event.Data = payload.Data
	case EventTruncated:
		var payload struct {
			LimitBytes int `json:"limit_bytes"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil, &Error{Code: "stream", Message: "sandbox: could not parse a truncated event", Cause: err}
		}
		event.LimitBytes = payload.LimitBytes
	case EventComplete:
		ex, err := parseExecution([]byte(data))
		if err != nil {
			return nil, err
		}
		event.Execution = ex
	}
	return event, nil
}

// parseExecution decodes an Execution payload.
func parseExecution(data []byte) (*Execution, error) {
	var ex Execution
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, &Error{Code: "stream", Message: "sandbox: could not parse the complete event", Cause: err}
	}
	return &ex, nil
}

// ---------------------------------------------------------------- conveniences

// Exec runs a command to completion and returns it. The returned error is nil
// only when the termination reason is exited and the exit code is 0; any other
// outcome is an *Error carrying the Execution with its output.
func (c *Client) Exec(ctx context.Context, sandboxID string, command []string) (*Execution, error) {
	return c.ExecStream(ctx, sandboxID, command, nil)
}

// ExecStream is StartExecution followed by StreamExecution, with onEvent
// receiving output as it arrives.
func (c *Client) ExecStream(ctx context.Context, sandboxID string, command []string, onEvent func(Event) error) (*Execution, error) {
	started, err := c.StartExecution(ctx, sandboxID, ExecuteRequest{Command: command})
	if err != nil {
		return executionOf(err), err
	}
	ex, err := c.StreamExecution(ctx, sandboxID, started.ID, onEvent)
	if err != nil {
		if ex != nil {
			return ex, &Error{
				Code:      string(ReasonCanceled),
				Message:   "sandbox: execution canceled",
				Execution: ex,
				Cause:     err,
			}
		}
		return executionOf(err), err
	}
	if err := outcomeError(ex); err != nil {
		return ex, err
	}
	return ex, nil
}

// executionOf returns the Execution attached to an error, when there is one.
func executionOf(err error) *Execution {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.Execution
	}
	return nil
}
