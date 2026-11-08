package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackoffIsCappedAndJittered(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		d := backoff(attempt, 0)
		if d > maxBackoff {
			t.Fatalf("attempt %d: backoff %v exceeds the %v cap", attempt, d, maxBackoff)
		}
		if d < 0 {
			t.Fatalf("attempt %d: negative backoff %v", attempt, d)
		}
	}
	// Attempt 0 is 200ms and attempt 3 already hits the 2s cap.
	if d := backoff(0, 0); d > baseBackoff {
		t.Errorf("attempt 0 backoff = %v, want at most %v", d, baseBackoff)
	}
	if d := backoff(10, 0); d > maxBackoff {
		t.Errorf("attempt 10 backoff = %v, want at most %v", d, maxBackoff)
	}
}

func TestBackoffHonoursRetryAfter(t *testing.T) {
	if got := backoff(5, 7*time.Second); got != 7*time.Second {
		t.Fatalf("backoff with Retry-After = %v, want 7s", got)
	}
}

func TestRetryBoundaries(t *testing.T) {
	cases := []struct {
		status int
		retry  bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusConflict, false},
		{http.StatusRequestEntityTooLarge, false},
	}
	for _, tc := range cases {
		if got := retryableStatus(tc.status); got != tc.retry {
			t.Errorf("retryableStatus(%d) = %v, want %v", tc.status, got, tc.retry)
		}
	}
}

func TestGetRetriesThenSucceeds(t *testing.T) {
	f := newFakeServer(t)
	var calls atomic.Int32
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "try later")
			return
		}
		writeJSON(w, http.StatusOK, Sandbox{ID: "sbx_1", State: StateRunning})
	})

	c := f.client(t, 3)
	sb, err := c.GetSandbox(context.Background(), "sbx_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sb.ID != "sbx_1" {
		t.Errorf("sandbox = %+v", sb)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
}

func TestGetStopsAtRetryLimit(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "down")
	})

	c := f.client(t, 2)
	_, err := c.GetSandbox(context.Background(), "sbx_1")
	if err == nil {
		t.Fatal("get succeeded, want the exhausted retries")
	}
	var sdkErr *Error
	if !errors.As(err, &sdkErr) || !sdkErr.Retryable() {
		t.Fatalf("err = %v, want a retryable *Error", err)
	}
	// One original attempt plus two retries.
	if n := f.count(http.MethodGet, "/v1/sandboxes/sbx_1"); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
}

func TestNonRetryableErrorIsAttemptedOnce(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not_found", "no such sandbox")
	})

	c := f.client(t, 5)
	_, err := c.GetSandbox(context.Background(), "sbx_1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if n := f.count(http.MethodGet, "/v1/sandboxes/sbx_1"); n != 1 {
		t.Errorf("%d attempts, want 1: a 404 is not retried", n)
	}
}

// A create must never be retried: a second attempt could start a second
// container.
func TestCreateIsAttemptedExactlyOnce(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "down")
	})

	c := f.client(t, 5)
	_, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{})
	if err == nil {
		t.Fatal("create succeeded, want the failure")
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes"); n != 1 {
		t.Errorf("%d create attempts, want exactly 1", n)
	}
}

// A start without a key could run a command twice, so the SDK supplies a key
// and retries only because of it.
func TestStartCarriesIdempotencyKey(t *testing.T) {
	f := newFakeServer(t)
	var calls atomic.Int32
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "down")
			return
		}
		writeJSON(w, http.StatusCreated, Execution{ID: "exe_1", State: "running"})
	})

	c := f.client(t, 2)
	if _, err := c.StartExecution(context.Background(), "sbx_1", ExecuteRequest{Command: []string{"true"}}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions"); n != 2 {
		t.Fatalf("%d attempts, want 2", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, r := range f.requests {
		if r.method == http.MethodPost && r.path == "/v1/sandboxes/sbx_1/executions" {
			var req ExecuteRequest
			if err := jsonUnmarshal(r.body, &req); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			keys = append(keys, req.IdempotencyKey)
		}
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("idempotency keys = %q, want one non-empty key reused", keys)
	}
}

func TestContextDoneStopsRetrying(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "down")
	})

	c := f.client(t, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.GetSandbox(ctx, "sbx_1")
	if err == nil {
		t.Fatal("get succeeded, want the context failure")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; retries must stop when the context ends", elapsed)
	}
}

func TestErrorMatchesSentinels(t *testing.T) {
	cases := []struct {
		err    *Error
		target error
	}{
		{&Error{StatusCode: http.StatusNotFound}, ErrNotFound},
		{&Error{StatusCode: http.StatusUnauthorized}, ErrUnauthorized},
		{&Error{Code: "busy"}, ErrBusy},
		{&Error{Code: "invalid_state"}, ErrInvalidState},
		{&Error{Code: "cancel_unconfirmed"}, ErrCancelUnconfirmed},
		{&Error{Code: "canceled"}, ErrCanceled},
		{&Error{Code: "timeout"}, ErrTimeout},
		{&Error{Code: "oom"}, ErrOOM},
		{&Error{Code: "evicted"}, ErrEvicted},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, tc.target) {
			t.Errorf("%+v does not match %v", tc.err, tc.target)
		}
	}

	code := 3
	nonzero := &Error{Code: "nonzero_exit", Execution: &Execution{ExitCode: &code}}
	if !errors.Is(nonzero, ErrNonZeroExit) {
		t.Error("a nonzero execution does not match ErrNonZeroExit")
	}
	zero := 0
	ok := &Error{Code: "exited", Execution: &Execution{ExitCode: &zero}}
	if errors.Is(ok, ErrNonZeroExit) {
		t.Error("a zero exit matched ErrNonZeroExit")
	}
}

func TestOutcomeErrorKeepsOutput(t *testing.T) {
	code := 2
	reason := ReasonOOM
	ex := &Execution{ID: "exe_1", ExitCode: &code, TerminationReason: &reason, Stdout: "out", Stderr: "err"}
	err := outcomeError(ex)
	if !errors.Is(err, ErrOOM) {
		t.Fatalf("err = %v, want ErrOOM", err)
	}
	var sdkErr *Error
	if !errors.As(err, &sdkErr) || sdkErr.Execution != ex {
		t.Fatalf("error lost the execution: %+v", sdkErr)
	}
	if sdkErr.Execution.Stdout != "out" || sdkErr.Execution.Stderr != "err" {
		t.Error("error lost the output")
	}

	zero := 0
	exited := ReasonExited
	if err := outcomeError(&Execution{ExitCode: &zero, TerminationReason: &exited}); err != nil {
		t.Errorf("a clean exit returned %v", err)
	}
}

func TestFilesRoundTrip(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			if got := r.URL.Query().Get("path"); got != "/work/out.txt" {
				t.Errorf("path = %q", got)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("content-type = %q", ct)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			_, _ = w.Write([]byte("stored"))
		default:
			http.NotFound(w, r)
		}
	})

	c := f.client(t, 3)
	if err := c.WriteFile(context.Background(), "sbx_1", "/work/out.txt", []byte("data")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := c.ReadFile(context.Background(), "sbx_1", "/work/out.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "stored" {
		t.Errorf("read %q, want stored", data)
	}
}

func TestClientDefaults(t *testing.T) {
	t.Setenv("SANDBOX_DOMAIN", "localhost:9999")
	t.Setenv("SANDBOX_API_KEY", "env-key")
	c, err := NewClient(Config{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if c.domain != "http://localhost:9999" {
		t.Errorf("domain = %q, want the scheme added", c.domain)
	}
	if c.apiKey != "env-key" {
		t.Errorf("api key = %q", c.apiKey)
	}
	if c.maxRetries != 3 {
		t.Errorf("maxRetries = %d, want the default 3", c.maxRetries)
	}

	disabled, err := NewClient(Config{Domain: "http://x", MaxRetries: -1})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if disabled.maxRetries != 0 {
		t.Errorf("maxRetries = %d, want 0 when negative", disabled.maxRetries)
	}

	if _, err := NewClient(Config{Domain: "http://"}); err == nil {
		t.Error("a domain without a host was accepted")
	}
}

func TestTimeRoundTrips(t *testing.T) {
	var sb Sandbox
	err := jsonUnmarshal([]byte(`{"created_at":"2026-01-02T03:04:05Z","expires_at":null}`), &sb)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sb.CreatedAt.UTC().Format(time.RFC3339) != "2026-01-02T03:04:05Z" {
		t.Errorf("created_at = %v", sb.CreatedAt)
	}
	if sb.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", sb.ExpiresAt)
	}
}

// jsonUnmarshal decodes test fixture bytes.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
