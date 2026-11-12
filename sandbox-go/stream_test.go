package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is a controllable contract server. Handlers are per test; the
// server counts every request so retry boundaries are assertable.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	handler  http.HandlerFunc
}

type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Handlers decode the body themselves, so put it back after
		// recording it.
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(), body: body,
		})
		handler := f.handler
		f.mu.Unlock()
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// setHandler installs the behaviour for following requests.
func (f *fakeServer) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

// count returns how many requests matched method and path.
func (f *fakeServer) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

// client returns an SDK client pointed at the fake with no retry jitter delay.
func (f *fakeServer) client(t *testing.T, maxRetries int) *Client {
	t.Helper()
	t.Setenv("SANDBOX_DOMAIN", "")
	t.Setenv("SANDBOX_API_KEY", "")
	c, err := NewClient(Config{Domain: f.srv.URL, MaxRetries: maxRetries, HTTPClient: fastClient()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// fastClient bounds every request so a broken handler fails a test quickly
// instead of hanging it.
func fastClient() *http.Client {
	return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// ---------------------------------------------------------------- streaming

// sseFrame renders one SSE event. The payload must be a single line: a raw
// newline ends the data field, so JSON with "\n" in it has to be escaped the
// way a real server does.
func sseFrame(id uint64, kind EventType, payload string) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", id, kind, strings.ReplaceAll(payload, "\n", `\n`))
}

func completePayload(id string) string {
	code := 0
	reason := ReasonExited
	ex := Execution{ID: id, SandboxID: "sbx_1", State: "completed", ExitCode: &code, TerminationReason: &reason}
	data, _ := json.Marshal(ex)
	return string(data)
}

func TestStreamReconnectsFromLastEventID(t *testing.T) {
	f := newFakeServer(t)
	attempt := atomic.Int32{}

	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		last := r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		switch attempt.Add(1) {
		case 1:
			// Deliver events 1 and 2, then drop the connection before
			// complete, which is what a proxy timeout looks like.
			_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"one\n"}`))
			_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"two\n"}`))
			f.t.Logf("first attempt: Last-Event-ID=%q, dropping", last)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		default:
			// Replay from 1 anyway: the SDK must drop what it already saw.
			_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"one\n"}`))
			_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"two\n"}`))
			_, _ = io.WriteString(w, sseFrame(3, EventStdout, `{"data":"three\n"}`))
			_, _ = io.WriteString(w, sseFrame(4, EventComplete, completePayload("exe_1")))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		}
	})

	c := f.client(t, 3)
	var got []string
	var ids []uint64
	ex, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", func(ev Event) error {
		if ev.Type == EventStdout {
			got = append(got, ev.Data)
		}
		ids = append(ids, ev.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if ex.ID != "exe_1" {
		t.Errorf("execution = %+v", ex)
	}
	if strings.Join(got, "") != "one\ntwo\nthree\n" {
		t.Errorf("delivered output = %q", got)
	}
	// Every ID once, in order: no duplicates from the replay.
	want := []uint64{1, 2, 3, 4}
	if len(ids) != len(want) {
		t.Fatalf("delivered ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("delivered ids = %v, want %v", ids, want)
		}
	}
	if n := f.count(http.MethodGet, "/v1/sandboxes/sbx_1/executions/exe_1/events"); n != 2 {
		t.Errorf("%d stream attempts, want 2", n)
	}
}

func TestStreamDropsEventsAtOrBelowTheCursor(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A server that replays out of order, and repeats an ID it already
		// sent: the stream must deliver each ID at most once, in order.
		_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"two\n"}`))
		_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"stale\n"}`))
		_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"again\n"}`))
		_, _ = io.WriteString(w, sseFrame(3, EventComplete, completePayload("exe_1")))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	})

	c := f.client(t, 0)
	var ids []uint64
	var out strings.Builder
	_, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", func(ev Event) error {
		ids = append(ids, ev.ID)
		if ev.Type == EventStdout {
			out.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if out.String() != "two\n" {
		t.Errorf("delivered output = %q, want only the first event at or above the cursor", out.String())
	}
	want := []uint64{2, 3}
	if len(ids) != len(want) {
		t.Fatalf("delivered ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("delivered ids = %v, want %v", ids, want)
		}
	}
}

func TestStreamCallbackErrorStopsWithoutCancel(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"x"}`))
		_, _ = io.WriteString(w, sseFrame(2, EventComplete, completePayload("exe_1")))
	})

	c := f.client(t, 3)
	boom := errors.New("renderer failed")
	_, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", func(Event) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the callback error", err)
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 0 {
		t.Errorf("cancel was sent %d times; a callback error must not cancel", n)
	}
}

func TestStreamContextCancelCancelsExecution(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			reason := ReasonCanceled
			writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "completed", TerminationReason: &reason})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"x"}`))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done() // hold the stream open until the client leaves
	})

	c := f.client(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	ex, err := c.StreamExecution(ctx, "sbx_1", "exe_1", nil)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if ex == nil || ex.ID != "exe_1" {
		t.Errorf("execution = %+v", ex)
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 1 {
		t.Errorf("cancel count = %d, want 1", n)
	}
}

func TestStreamCancelUnconfirmed(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "shutting down")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	})

	c := f.client(t, 0) // no retries, so the cancel failure is final fast
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.StreamExecution(ctx, "sbx_1", "exe_1", nil)
	if !errors.Is(err, ErrCancelUnconfirmed) {
		t.Fatalf("err = %v, want ErrCancelUnconfirmed", err)
	}
}

// ---------------------------------------------------------------- start

func TestStartRecoversAfterCancelDuringStart(t *testing.T) {
	f := newFakeServer(t)
	var pushed atomic.Bool

	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/executions"):
			var req ExecuteRequest
			_ = json.Unmarshal(readBody(r), &req)
			if req.IdempotencyKey == "" {
				t.Error("start request carries no idempotency key")
			}
			if !pushed.Swap(true) {
				// The first attempt accepts the work, then the caller's
				// context dies before the response arrives: the server holds
				// the response until the client goes away.
				<-r.Context().Done()
				return
			}
			// The recovery attempt: the server returns the existing record.
			writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "running"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			reason := ReasonCanceled
			writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "completed", TerminationReason: &reason})
		default:
			http.NotFound(w, r)
		}
	})

	c := f.client(t, 1)
	// The contract has two separate rules. Step 3: a context that is already
	// done returns without sending anything.
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, err := c.StartExecution(done, "sbx_1", ExecuteRequest{Command: []string{"sleep", "10"}}); err == nil {
		t.Fatal("an already-done context still sent a request")
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions"); n != 0 {
		t.Fatalf("%d starts sent with a done context, want 0", n)
	}

	// Step 4: the context ends while the start is in flight, so the SDK
	// repeats the start with the same key and cancels what it finds.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	ex, err := c.StartExecution(ctx, "sbx_1", ExecuteRequest{Command: []string{"sleep", "10"}})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if ex == nil || ex.ID != "exe_1" {
		t.Fatalf("execution = %+v, want exe_1", ex)
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 1 {
		t.Errorf("cancel count = %d, want 1", n)
	}
}

// readBody returns the request body as bytes, for handlers that decode twice.
func readBody(r *http.Request) []byte {
	data, _ := io.ReadAll(r.Body)
	return data
}

// TestStreamAcceptsTheLargestCompleteEvent pins the scanner limit against the
// worst case the server can send. A complete event carries the whole retained
// output (1 MiB across stdout and stderr), and JSON turns every control byte
// into six characters, so the data line can exceed 6 MiB. The earlier 2 MiB
// cap made a command that printed control bytes fail to stream even though it
// had completed successfully.
func TestStreamAcceptsTheLargestCompleteEvent(t *testing.T) {
	f := newFakeServer(t)

	// Every byte is 0x01: the worst case for JSON escaping.
	worst := strings.Repeat("\x01", 1<<20)
	code := 0
	reason := ReasonExited
	payload, err := json.Marshal(Execution{
		ID: "exe_1", SandboxID: "sbx_1", State: "completed",
		ExitCode: &code, TerminationReason: &reason,
		Stdout: worst[:512<<10], Stderr: worst[512<<10:],
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "id: 1\nevent: complete\ndata: "+string(payload)+"\n\n")
	})
	if len(payload) < 6<<20 {
		t.Fatalf("test payload is only %d bytes; it does not reach the limit it exists to exercise", len(payload))
	}

	c := f.client(t, 1)
	ex, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(ex.Stdout) != 512<<10 || len(ex.Stderr) != 512<<10 {
		t.Fatalf("stdout=%d stderr=%d, want 512 KiB each", len(ex.Stdout), len(ex.Stderr))
	}
}
