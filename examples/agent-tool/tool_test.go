package agenttool_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agenttool "github.com/ruhuang2001/tiny-sandbox/examples/agent-tool"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// harness wires the real HTTP handler over a backend the test can steer, so
// the example is tested through the same path an agent would use.
type harness struct {
	be     *membackend.Backend
	store  *daemon.Sandboxes
	client *sandbox.Client
}

func newHarness(t *testing.T, opts ...daemon.Option) *harness {
	t.Helper()
	be := membackend.New()
	store := daemon.New(be, opts...)
	srv := httptest.NewServer(daemon.NewHandler(store, "test"))
	t.Cleanup(srv.Close)

	client, err := sandbox.NewClient(sandbox.Config{Domain: srv.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return &harness{be: be, store: store, client: client}
}

func TestToolDefinitionIsUsableByAnAgent(t *testing.T) {
	def := agenttool.ToolDefinition()
	if def.Name != agenttool.ToolName {
		t.Errorf("name = %q", def.Name)
	}
	if def.Description == "" {
		t.Error("a tool without a description cannot be chosen by a model")
	}
	schema := def.InputSchema
	if schema["type"] != "object" {
		t.Errorf("schema type = %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || props["code"] == nil {
		t.Fatalf("schema properties = %v, want a code field", schema["properties"])
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "code" {
		t.Errorf("required = %v, want [code]", schema["required"])
	}

	// The definition must survive JSON, because that is how it reaches a model.
	if _, err := json.Marshal(def); err != nil {
		t.Fatalf("definition does not marshal: %v", err)
	}
}

func TestRunSuccess(t *testing.T) {
	h := newHarness(t)
	h.be.Programs["python3"] = func(_ context.Context, _ []string) (string, string, int) {
		return "1024\n", "", 0
	}

	result := agenttool.New(h.client).Run(context.Background(), agenttool.Input{Code: "print(2**10)"})
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Outcome != "exited" || result.Stdout != "1024\n" {
		t.Errorf("result = %+v", result)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("exit code = %v", result.ExitCode)
	}
}

func TestRunNonZeroExitKeepsOutput(t *testing.T) {
	h := newHarness(t)
	h.be.Programs["python3"] = func(_ context.Context, _ []string) (string, string, int) {
		return "partial\n", "Traceback: boom\n", 1
	}

	result := agenttool.New(h.client).Run(context.Background(), agenttool.Input{Code: "raise"})
	if !result.IsError || result.Outcome != "nonzero_exit" {
		t.Fatalf("result = %+v, want nonzero_exit", result)
	}
	// The model needs the traceback to decide what to do next.
	if result.Stderr != "Traceback: boom\n" || result.Stdout != "partial\n" {
		t.Errorf("output was lost: %+v", result)
	}
	if result.ExitCode == nil || *result.ExitCode != 1 {
		t.Errorf("exit code = %v", result.ExitCode)
	}
}

func TestRun137WithoutAnOOMDeltaIsNotOOM(t *testing.T) {
	h := newHarness(t)
	h.be.Programs["python3"] = func(context.Context, []string) (string, string, int) { return "", "", 137 }

	result := agenttool.New(h.client).Run(context.Background(), agenttool.Input{Code: "x = bytearray(1<<30)"})
	// Exit 137 alone never means oom: without a cgroup delta and with the
	// container alive, it is an ordinary exit. This is the contract's rule,
	// and an agent that conflated the two would give the wrong advice.
	if result.Outcome != "nonzero_exit" {
		t.Fatalf("outcome = %q, want nonzero_exit without an OOM delta", result.Outcome)
	}
}

func TestRunOOM(t *testing.T) {
	h := newHarness(t)
	// The container's oom_kill counter moves during the execution, as the
	// kernel would, and the process exits 137.
	h.be.Programs["python3"] = func(_ context.Context, _ []string) (string, string, int) {
		h.be.SetOOMKills(h.onlyContainer(t), 1)
		return "", "", 137
	}

	result := agenttool.New(h.client).Run(context.Background(), agenttool.Input{Code: "x = bytearray(1<<30)"})
	if result.Outcome != "oom" {
		t.Fatalf("outcome = %q, want oom", result.Outcome)
	}
	if result.Hint == "" {
		t.Error("an OOM should tell the agent how to recover")
	}
}

func TestRunTimeoutIsDistinct(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := newHarness(t, daemon.WithClock(clock))
	h.be.Streamers["python3"] = func(ctx context.Context, _ func([]byte, []byte) error) int {
		<-ctx.Done()
		return 137
	}

	tool := agenttool.New(h.client)
	done := make(chan agenttool.Result, 1)
	go func() {
		done <- tool.Run(context.Background(), agenttool.Input{Code: "while True: pass", TimeoutSeconds: 5})
	}()

	// Let the execution start, then expire the sandbox.
	time.Sleep(50 * time.Millisecond)
	clock.Advance(6 * time.Second)
	h.store.Reap(context.Background())

	select {
	case result := <-done:
		if result.Outcome != "timeout" {
			t.Fatalf("result = %+v, want timeout", result)
		}
		if result.Hint == "" {
			t.Error("a timeout should tell the agent how to recover")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tool did not return after the sandbox expired")
	}
}

func TestRunInvalidInput(t *testing.T) {
	h := newHarness(t)
	result := agenttool.New(h.client).Run(context.Background(), agenttool.Input{})
	if !result.IsError || result.Outcome != "invalid_input" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunRemovesItsSandbox(t *testing.T) {
	h := newHarness(t)
	h.be.Programs["python3"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }

	agenttool.New(h.client).Run(context.Background(), agenttool.Input{Code: "print(1)"})

	list, err := h.client.ListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, sb := range list {
		if sb.State != sandbox.StateKilled {
			t.Errorf("sandbox %s is %s after the tool returned, want killed", sb.ID, sb.State)
		}
	}
}

func TestResultIsMachineReadable(t *testing.T) {
	code := 1
	result := agenttool.Result{
		IsError: true, Outcome: "nonzero_exit", ExitCode: &code,
		Stdout: "out", Stderr: "err", Message: "failed",
	}
	data, err := agenttool.ResultJSON(result)
	if err != nil {
		t.Fatalf("result json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	for _, key := range []string{"is_error", "outcome", "exit_code", "stdout", "stderr"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("result JSON lacks %q: %s", key, data)
		}
	}
}

// onlyContainer returns the single container the harness created, so a
// program can move that container's OOM counter.
func (h *harness) onlyContainer(t *testing.T) string {
	t.Helper()
	ids := h.be.IDs()
	if len(ids) != 1 {
		t.Fatalf("%d containers, want 1", len(ids))
	}
	return ids[0]
}

// fakeClock is the daemon's injectable clock, advanced by the test from one
// goroutine while the engine reads it from another, so it needs a lock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
