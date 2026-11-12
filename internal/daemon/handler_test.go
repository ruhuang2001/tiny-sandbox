package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
)

// handlerHarness is a real HTTP server over the in-memory backend.
type handlerHarness struct {
	t   *testing.T
	srv *httptest.Server
	be  *membackend.Backend
}

func newHandlerHarness(t *testing.T, opts ...daemon.Option) *handlerHarness {
	t.Helper()
	be := membackend.New()
	store := daemon.New(be, opts...)
	srv := httptest.NewServer(daemon.NewHandler(store, "test-version"))
	t.Cleanup(srv.Close)
	return &handlerHarness{t: t, srv: srv, be: be}
}

// reply is one response, already read and closed so no test leaks a body.
type reply struct {
	Status int
	Body   []byte
	Header http.Header
}

// raw sends a request and returns the response fully read. body may be nil.
func (h *handlerHarness) raw(method, path string, body []byte, header map[string]string) *reply {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.srv.URL+path, r)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	data, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	if closeErr != nil {
		h.t.Fatalf("%s %s: close body: %v", method, path, closeErr)
	}
	return &reply{Status: resp.StatusCode, Body: data, Header: resp.Header}
}

// do is raw for callers that only need the status and the decoded body.
func (h *handlerHarness) do(method, path string, body []byte, header map[string]string) *reply {
	return h.raw(method, path, body, header)
}

// jsonBody decodes a response body into v.
func (h *handlerHarness) jsonBody(resp *reply, v any) {
	h.t.Helper()
	if err := json.Unmarshal(resp.Body, v); err != nil {
		h.t.Fatalf("decode body: %v", err)
	}
}

// errorCode returns the contract code of an error response.
func (h *handlerHarness) errorCode(resp *reply) string {
	h.t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	h.jsonBody(resp, &body)
	return body.Code
}

func (h *handlerHarness) create() daemon.Sandbox {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/v1/sandboxes", []byte(`{"image":"alpine:3.20"}`), nil)
	if resp.Status != http.StatusCreated {
		h.t.Fatalf("create status = %d, want 201", resp.Status)
	}
	var sb daemon.Sandbox
	h.jsonBody(resp, &sb)
	return sb
}

func TestHandlerHealth(t *testing.T) {
	h := newHandlerHarness(t)
	resp := h.do(http.MethodGet, "/healthz", nil, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	var body map[string]string
	h.jsonBody(resp, &body)
	if body["status"] != "ok" || body["version"] != "test-version" {
		t.Errorf("body = %v", body)
	}
}

func TestHandlerLifecycle(t *testing.T) {
	h := newHandlerHarness(t)
	sb := h.create()
	path := "/v1/sandboxes/" + sb.ID

	for _, step := range []struct {
		method, path string
		status       int
		state        daemon.State
	}{
		{http.MethodGet, path, 200, daemon.StateRunning},
		{http.MethodPost, path + "/pause", 200, daemon.StatePaused},
		{http.MethodPost, path + "/pause", 200, daemon.StatePaused}, // idempotent
		{http.MethodPost, path + "/resume", 200, daemon.StateRunning},
		{http.MethodPost, path + "/resume", 200, daemon.StateRunning},
		{http.MethodDelete, path, 200, daemon.StateKilled},
		{http.MethodDelete, path, 200, daemon.StateKilled}, // idempotent
	} {
		resp := h.do(step.method, step.path, nil, nil)
		if resp.Status != step.status {
			t.Fatalf("%s %s = %d, want %d", step.method, step.path, resp.Status, step.status)
		}
		var got daemon.Sandbox
		h.jsonBody(resp, &got)
		if got.State != step.state {
			t.Errorf("%s %s state = %s, want %s", step.method, step.path, got.State, step.state)
		}
	}

	// Pause and resume are invalid on a killed sandbox.
	resp := h.do(http.MethodPost, path+"/pause", nil, nil)
	if resp.Status != http.StatusConflict || h.errorCode(resp) != "invalid_state" {
		t.Errorf("pause killed = %d %s, want 409 invalid_state", resp.Status, h.errorCode(resp))
	}
}

func TestHandlerListNewestFirst(t *testing.T) {
	h := newHandlerHarness(t)
	first := h.create()
	second := h.create()

	resp := h.do(http.MethodGet, "/v1/sandboxes", nil, nil)
	var body struct {
		Sandboxes []daemon.Sandbox `json:"sandboxes"`
	}
	h.jsonBody(resp, &body)
	if len(body.Sandboxes) != 2 || body.Sandboxes[0].ID != second.ID || body.Sandboxes[1].ID != first.ID {
		t.Fatalf("list order = %v", body.Sandboxes)
	}
}

func TestHandlerAuth(t *testing.T) {
	be := membackend.New()
	store := daemon.New(be)
	srv := httptest.NewServer(daemon.NewHandler(store, "v").WithAPIKey("secret"))
	t.Cleanup(srv.Close)

	get := func(path, key string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if got := get("/healthz", ""); got != http.StatusOK {
		t.Errorf("healthz without key = %d, want 200", got)
	}
	if got := get("/v1/sandboxes", ""); got != http.StatusUnauthorized {
		t.Errorf("list without key = %d, want 401", got)
	}
	if got := get("/v1/sandboxes", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("list with wrong key = %d, want 401", got)
	}
	if got := get("/v1/sandboxes", "secret"); got != http.StatusOK {
		t.Errorf("list with key = %d, want 200", got)
	}
}

func TestHandlerExecutionFlow(t *testing.T) {
	h := newHandlerHarness(t)
	h.be.Programs["echo"] = func(_ context.Context, args []string) (string, string, int) {
		return strings.Join(args, " ") + "\n", "", 0
	}
	sb := h.create()
	base := "/v1/sandboxes/" + sb.ID + "/executions"

	resp := h.do(http.MethodPost, base, []byte(`{"command":["echo","hello"]}`), nil)
	if resp.Status != http.StatusCreated {
		t.Fatalf("start = %d, want 201", resp.Status)
	}
	var ex daemon.Execution
	h.jsonBody(resp, &ex)
	if ex.State != daemon.ExecRunning {
		t.Fatalf("state = %s", ex.State)
	}

	// The execution completes on its own.
	deadline := time.Now().Add(5 * time.Second)
	for {
		get := h.do(http.MethodGet, base+"/"+ex.ID, nil, nil)
		var got daemon.Execution
		h.jsonBody(get, &got)
		if got.State == daemon.ExecCompleted {
			if got.Stdout != "hello\n" {
				t.Errorf("stdout = %q", got.Stdout)
			}
			if got.TerminationReason == nil || *got.TerminationReason != daemon.ReasonExited {
				t.Errorf("reason = %v", got.TerminationReason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("execution did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// List responses carry no output field at all.
	listResp := h.do(http.MethodGet, base, nil, nil)
	if strings.Contains(string(listResp.Body), `"stdout"`) {
		t.Errorf("list response carries stdout: %s", listResp.Body)
	}
}

func TestHandlerBusyAndIdempotency(t *testing.T) {
	h := newHandlerHarness(t)
	release := make(chan struct{})
	h.be.Streamers["slow"] = func(_ context.Context, write func([]byte, []byte) error) int {
		<-release // runs until the test lets it finish
		_ = write([]byte("done\n"), nil)
		return 0
	}
	sb := h.create()
	base := "/v1/sandboxes/" + sb.ID + "/executions"

	first := h.do(http.MethodPost, base, []byte(`{"command":["slow"],"idempotency_key":"k1"}`), nil)
	if first.Status != http.StatusCreated {
		t.Fatalf("first start = %d", first.Status)
	}
	var ex daemon.Execution
	h.jsonBody(first, &ex)

	// A second start with a different command is busy.
	busy := h.do(http.MethodPost, base, []byte(`{"command":["other"]}`), nil)
	if busy.Status != http.StatusConflict || h.errorCode(busy) != "busy" {
		t.Fatalf("second start = %d %s, want 409 busy", busy.Status, h.errorCode(busy))
	}

	// The same key and command replays the record with 200.
	replay := h.do(http.MethodPost, base, []byte(`{"command":["slow"],"idempotency_key":"k1"}`), nil)
	if replay.Status != http.StatusOK {
		t.Fatalf("replay = %d, want 200", replay.Status)
	}
	var again daemon.Execution
	h.jsonBody(replay, &again)
	if again.ID != ex.ID {
		t.Errorf("replay id = %s, want %s", again.ID, ex.ID)
	}

	// The same key with a different command conflicts.
	conflict := h.do(http.MethodPost, base, []byte(`{"command":["other"],"idempotency_key":"k1"}`), nil)
	if conflict.Status != http.StatusConflict || h.errorCode(conflict) != "idempotency_conflict" {
		t.Fatalf("conflict = %d %s", conflict.Status, h.errorCode(conflict))
	}

	close(release)
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	id, kind, data string
}

// readSSE parses events until the stream closes or the deadline passes.
func readSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	var cur sseEvent
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if cur.id != "" || cur.kind != "" {
				events = append(events, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	if len(cur.id) > 0 || len(cur.kind) > 0 {
		events = append(events, cur)
	}
	return events
}

func TestHandlerEventStream(t *testing.T) {
	h := newHandlerHarness(t)
	h.be.Programs["emit"] = func(_ context.Context, _ []string) (string, string, int) {
		return "one\ntwo\n", "warn\n", 0
	}
	sb := h.create()
	base := "/v1/sandboxes/" + sb.ID + "/executions"

	resp := h.do(http.MethodPost, base, []byte(`{"command":["emit"]}`), nil)
	var ex daemon.Execution
	h.jsonBody(resp, &ex)

	// Let the execution finish so the whole log is replayable.
	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+base+"/"+ex.ID+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	streamResp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()
	if ct := streamResp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	events := readSSE(t, streamResp.Body)
	if len(events) < 2 {
		t.Fatalf("got %d events: %v", len(events), events)
	}
	last := events[len(events)-1]
	if last.kind != "complete" {
		t.Fatalf("last event = %s, want complete", last.kind)
	}
	var complete daemon.Execution
	if err := json.Unmarshal([]byte(last.data), &complete); err != nil {
		t.Fatalf("complete payload: %v", err)
	}
	if complete.ID != ex.ID || complete.Stdout != "one\ntwo\n" {
		t.Errorf("complete = %+v", complete)
	}
	// IDs start at 1 and increase by 1.
	for i, ev := range events {
		if ev.id != strconv.Itoa(i+1) {
			t.Fatalf("event %d has id %q", i, ev.id)
		}
	}

	// Replay after the first event: the complete event is still last, and
	// no event is repeated.
	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+base+"/"+ex.ID+"/events", nil)
	req2.Header.Set("Last-Event-ID", "1")
	replayResp, err := h.srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer func() { _ = replayResp.Body.Close() }()
	replayed := readSSE(t, replayResp.Body)
	if len(replayed) != len(events)-1 {
		t.Fatalf("replay has %d events, want %d", len(replayed), len(events)-1)
	}
	if replayed[0].id != "2" {
		t.Errorf("replay starts at id %q, want 2", replayed[0].id)
	}

	// A bad Last-Event-ID is rejected before streaming starts.
	bad := h.do(http.MethodGet, base+"/"+ex.ID+"/events", nil, map[string]string{"Last-Event-ID": "abc"})
	if bad.Status != http.StatusBadRequest || h.errorCode(bad) != "invalid_request" {
		t.Errorf("bad Last-Event-ID = %d %s", bad.Status, h.errorCode(bad))
	}
}

func TestHandlerFiles(t *testing.T) {
	h := newHandlerHarness(t)
	sb := h.create()
	base := "/v1/sandboxes/" + sb.ID + "/files?path=/work/out.txt"

	put := h.do(http.MethodPut, base, []byte("file contents"), nil)
	if put.Status != http.StatusNoContent {
		t.Fatalf("put = %d", put.Status)
	}

	get := h.do(http.MethodGet, base, nil, nil)
	if get.Status != http.StatusOK || string(get.Body) != "file contents" {
		t.Fatalf("get = %d %q", get.Status, get.Body)
	}

	missing := h.do(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files?path=/nope", nil, nil)
	if missing.Status != http.StatusNotFound || h.errorCode(missing) != "not_found" {
		t.Errorf("missing file = %d %s", missing.Status, h.errorCode(missing))
	}

	relative := h.do(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files?path=relative", nil, nil)
	if relative.Status != http.StatusBadRequest || h.errorCode(relative) != "invalid_request" {
		t.Errorf("relative path = %d %s", relative.Status, h.errorCode(relative))
	}
}

func TestHandlerRequestLimits(t *testing.T) {
	h := newHandlerHarness(t)
	sb := h.create()

	// A file above 10 MiB is rejected rather than truncated.
	big := bytes.Repeat([]byte("x"), daemon.MaxFileBytes+1)
	resp := h.do(http.MethodPut, "/v1/sandboxes/"+sb.ID+"/files?path=/big", big, nil)
	if resp.Status != http.StatusRequestEntityTooLarge || h.errorCode(resp) != "payload_too_large" {
		t.Errorf("big file = %d %s", resp.Status, h.errorCode(resp))
	}

	// A JSON body above 1 MiB is rejected.
	huge := []byte(`{"image":"` + strings.Repeat("a", daemon.MaxRequestBodyBytes) + `"}`)
	resp = h.do(http.MethodPost, "/v1/sandboxes", huge, nil)
	if resp.Status != http.StatusRequestEntityTooLarge || h.errorCode(resp) != "payload_too_large" {
		t.Errorf("big body = %d %s", resp.Status, h.errorCode(resp))
	}

	// Malformed JSON is a 400.
	resp = h.do(http.MethodPost, "/v1/sandboxes", []byte(`{`), nil)
	if resp.Status != http.StatusBadRequest || h.errorCode(resp) != "invalid_request" {
		t.Errorf("malformed = %d %s", resp.Status, h.errorCode(resp))
	}
}

func TestHandlerNotFound(t *testing.T) {
	h := newHandlerHarness(t)
	resp := h.do(http.MethodGet, "/v1/sandboxes/sbx_missing", nil, nil)
	if resp.Status != http.StatusNotFound || h.errorCode(resp) != "not_found" {
		t.Fatalf("get missing = %d %s", resp.Status, h.errorCode(resp))
	}
}

// TestHandlerShutdownEndsStreams pins graceful shutdown: http.Server.Shutdown
// waits for handlers but does not cancel their contexts, so a live event stream
// would hold the shutdown open until its deadline. The shutdown signal ends it.
func TestHandlerShutdownEndsStreams(t *testing.T) {
	be := membackend.New()
	be.Programs["block"] = func(ctx context.Context, _ []string) (string, string, int) {
		<-ctx.Done()
		return "", "", 137
	}
	// A command that never finishes on its own, so the stream stays open.
	shuttingDown := make(chan struct{})
	srv := httptest.NewServer(daemon.NewHandler(daemon.New(be), "test-version").WithShutdown(shuttingDown))
	t.Cleanup(srv.Close)

	sb := postJSON[daemon.Sandbox](t, srv, "/v1/sandboxes", `{"timeout_seconds":0}`)
	ex := postJSON[daemon.Execution](t, srv,
		"/v1/sandboxes/"+sb.ID+"/executions", `{"command":["block"]}`)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.URL+"/v1/sandboxes/"+sb.ID+"/executions/"+ex.ID+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}

	close(shuttingDown)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream survived the shutdown signal")
	}
}

// postJSON POSTs body and decodes the response into T.
func postJSON[T any](t *testing.T, srv *httptest.Server, path, body string) T {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s: request: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	return out
}
