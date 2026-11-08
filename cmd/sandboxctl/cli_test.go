// Package main's tests run the CLI against the real HTTP handler.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
)

// cliHarness runs the CLI against the real HTTP handler over the in-memory
// backend, so status lines and exit codes are asserted against the actual
// contract rather than a stub.
type cliHarness struct {
	t   *testing.T
	be  *membackend.Backend
	url string
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	be := membackend.New()
	store := daemon.New(be, daemon.WithCancelGrace(10_000_000))
	srv := httptest.NewServer(daemon.NewHandler(store, "test"))
	t.Cleanup(srv.Close)
	return &cliHarness{t: t, be: be, url: srv.URL}
}

// result is one CLI invocation's observable result.
type result struct {
	code   int
	stdout string
	stderr string
}

// invoke runs the CLI with --domain pointed at the harness.
func (h *cliHarness) invoke(args ...string) result {
	h.t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"--domain", h.url}, args...), &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestVersionAndHelp(t *testing.T) {
	h := newCLIHarness(t)
	if got := h.invoke("version"); got.code != 0 || strings.TrimSpace(got.stdout) != version {
		t.Fatalf("version = %+v", got)
	}
	if got := h.invoke("help"); got.code != 0 || !strings.Contains(got.stdout, "usage:") {
		t.Fatalf("help = %+v", got)
	}
	if got := h.invoke("nonsense"); got.code != 2 {
		t.Fatalf("unknown command = %+v", got)
	}
}

func TestRunPrintsStatusLinesAndStreamsOutput(t *testing.T) {
	h := newCLIHarness(t)
	h.be.Programs["echo"] = func(_ context.Context, args []string) (string, string, int) {
		return strings.Join(args, " ") + "\n", "", 0
	}

	got := h.invoke("run", "--", "echo", "hello")
	if got.code != 0 {
		t.Fatalf("run = %+v", got)
	}
	if got.stdout != "hello\n" {
		t.Errorf("stdout = %q, want only command output", got.stdout)
	}
	if !strings.Contains(got.stderr, "sandbox: sbx_") ||
		!strings.Contains(got.stderr, "(created; remove with: sandboxctl kill sbx_") {
		t.Errorf("missing sandbox status line: %q", got.stderr)
	}
	if !strings.Contains(got.stderr, "execution: exe_") {
		t.Errorf("missing execution line: %q", got.stderr)
	}
	if !strings.Contains(got.stderr, "completed (exited), exit 0") {
		t.Errorf("missing final status line: %q", got.stderr)
	}
}

func TestRunWithExistingSandboxPrintsBareID(t *testing.T) {
	h := newCLIHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	sb, err := h.be.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = sb

	created := h.invoke("run", "--", "true")
	if created.code != 0 {
		t.Fatalf("create run = %+v", created)
	}
	id := firstSandboxID(t, created.stderr)

	got := h.invoke("run", "--sandbox", id, "--", "true")
	if got.code != 0 {
		t.Fatalf("run = %+v", got)
	}
	if !strings.Contains(got.stderr, "sandbox: "+id+"\n") {
		t.Errorf("existing sandbox line = %q", got.stderr)
	}
	if strings.Contains(got.stderr, "(created") {
		t.Errorf("an existing sandbox was reported as created: %q", got.stderr)
	}
}

// firstSandboxID pulls the id out of "sandbox: sbx_x (created; ...)".
func firstSandboxID(t *testing.T, stderr string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if id, ok := strings.CutPrefix(line, "sandbox: "); ok {
			id, _, _ = strings.Cut(id, " ")
			return id
		}
	}
	t.Fatalf("no sandbox line in %q", stderr)
	return ""
}

func TestRunExitCodesAndHints(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		want   int
		hint   string
		reason string
	}{
		{name: "exit 1 passes through", code: 1, want: 1},
		{name: "exit 42 passes through", code: 42, want: 42},
		{name: "exit 125 passes through", code: 125, want: 125},
		{name: "exit 126 collapses", code: 126, want: 1},
		{name: "exit 127 collapses", code: 127, want: 1},
		{name: "exit 137 is not a command code", code: 137, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIHarness(t)
			code := tc.code
			h.be.Programs["exit"] = func(context.Context, []string) (string, string, int) { return "", "", code }

			got := h.invoke("run", "--", "exit")
			if got.code != tc.want {
				t.Fatalf("exit code = %d, want %d (stderr %q)", got.code, tc.want, got.stderr)
			}
		})
	}
}

func TestRunOOMAndTimeoutHints(t *testing.T) {
	h := newCLIHarness(t)
	h.be.Streamers["oom"] = func(_ context.Context, _ func([]byte, []byte) error) int {
		// The engine classifies oom from the cgroup delta; drive it through
		// the backend's OOM counter instead of the exit code.
		return 137
	}
	// No hint path through membackend without a real OOM delta; covered in
	// the e2e script against Docker. Here, assert the hint table directly.
	if hintFor(&fakeOOM{}) != "increase --memory or reduce the command's memory use" {
		t.Error("oom hint is missing")
	}
	if hintFor(&fakeTimeout{}) != "increase --timeout" {
		t.Error("timeout hint is missing")
	}
	if hintFor(&fakeEvicted{}) != "the container was stopped outside sandboxd" {
		t.Error("evicted hint is missing")
	}
	if hintFor(&fakeBusy{}) != "wait for the running execution or cancel it" {
		t.Error("busy hint is missing")
	}
	if hintFor(&fakeAuth{}) != "set a valid SANDBOX_API_KEY or --api-key" {
		t.Error("auth hint is missing")
	}
	if hintFor(&fakeNotFound{}) != "check the sandbox and execution IDs" {
		t.Error("404 hint is missing")
	}
}

func TestRunCreationFlagsWithSandboxAreRejected(t *testing.T) {
	h := newCLIHarness(t)
	got := h.invoke("run", "--sandbox", "sbx_1", "--memory", "64", "--", "true")
	if got.code != 1 {
		t.Fatalf("exit = %d, want 1", got.code)
	}
	if !strings.Contains(got.stderr, "cannot be combined with --sandbox") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestRunWithoutCommand(t *testing.T) {
	h := newCLIHarness(t)
	if got := h.invoke("run"); got.code != 2 {
		t.Fatalf("exit = %d, want 2", got.code)
	}
}

func TestRunCodeShorthand(t *testing.T) {
	h := newCLIHarness(t)
	var got []string
	h.be.Programs["python3"] = func(_ context.Context, args []string) (string, string, int) {
		got = args
		return "ok\n", "", 0
	}
	res := h.invoke("run", "--", "code", "print(1)")
	if res.code != 0 {
		t.Fatalf("run = %+v", res)
	}
	if len(got) != 2 || got[0] != "-c" || got[1] != "print(1)" {
		t.Errorf("argv = %v, want [-c print(1)]", got)
	}
}

func TestRunRemoveKillsSandbox(t *testing.T) {
	h := newCLIHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }

	got := h.invoke("run", "--rm", "--", "true")
	if got.code != 0 {
		t.Fatalf("run = %+v", got)
	}
	id := firstSandboxID(t, got.stderr)
	// The sandbox should be gone from the server's eyes.
	list := h.invoke("list")
	if strings.Contains(list.stdout, id) {
		// Killed sandboxes stay listed until restart, so check the state.
		line := ""
		for _, l := range strings.Split(list.stdout, "\n") {
			if strings.HasPrefix(l, id+" ") {
				line = l
			}
		}
		if !strings.Contains(line, " killed ") {
			t.Errorf("sandbox %s survived --rm: %q", id, line)
		}
	}
}

func TestListAndLifecycle(t *testing.T) {
	h := newCLIHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	runResult := h.invoke("run", "--", "true")
	id := firstSandboxID(t, runResult.stderr)

	list := h.invoke("list")
	if list.code != 0 || !strings.Contains(list.stdout, id+" running") {
		t.Fatalf("list = %+v", list)
	}

	for _, step := range []struct {
		command string
		state   string
	}{
		{"pause", "paused"},
		{"resume", "running"},
		{"kill", "killed"},
	} {
		got := h.invoke(step.command, id)
		if got.code != 0 {
			t.Fatalf("%s = %+v", step.command, got)
		}
		if !strings.Contains(got.stdout, id+" "+step.state) {
			t.Errorf("%s printed %q, want state %s", step.command, got.stdout, step.state)
		}
	}
}

func TestInspect(t *testing.T) {
	h := newCLIHarness(t)
	runResult := h.invoke("run", "--", "echo", "x")
	id := firstSandboxID(t, runResult.stderr)
	got := h.invoke("inspect", id)
	if got.code != 0 || !strings.Contains(got.stdout, "id: "+id) ||
		!strings.Contains(got.stdout, "state: running") {
		t.Fatalf("inspect = %+v", got)
	}
}

func TestLogs(t *testing.T) {
	h := newCLIHarness(t)
	h.be.Programs["echo"] = func(_ context.Context, args []string) (string, string, int) {
		return strings.Join(args, " ") + "\n", "warning\n", 0
	}
	runResult := h.invoke("run", "--", "echo", "logged")
	execID := ""
	for _, line := range strings.Split(runResult.stderr, "\n") {
		if id, ok := strings.CutPrefix(line, "execution: "); ok {
			execID = id
		}
	}
	if execID == "" {
		t.Fatalf("no execution line in %q", runResult.stderr)
	}
	id := firstSandboxID(t, runResult.stderr)

	got := h.invoke("logs", id, execID)
	if got.code != 0 {
		t.Fatalf("logs = %+v", got)
	}
	if got.stdout != "logged\n" {
		t.Errorf("logs stdout = %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "warning") {
		t.Errorf("logs stderr = %q", got.stderr)
	}
}

func TestAuthErrorPrintsHint(t *testing.T) {
	be := membackend.New()
	store := daemon.New(be)
	srv := httptest.NewServer(daemon.NewHandler(store, "test").WithAPIKey("secret"))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	code := run([]string{"--domain", srv.URL, "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "hint: set a valid SANDBOX_API_KEY or --api-key") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestConnectionRefusedHint(t *testing.T) {
	var out, errOut bytes.Buffer
	// Port 1 on loopback refuses connections immediately.
	code := run([]string{"--domain", "http://127.0.0.1:1", "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "hint: start sandboxd or set SANDBOX_DOMAIN") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// ---------------------------------------------------------------- fake errors

// The hint table is keyed on the SDK's errors. These stand in for outcomes the
// in-memory backend cannot produce.
type fakeOOM struct{}

func (*fakeOOM) Error() string { return "oom" }

func (e *fakeOOM) Is(target error) bool { return target.Error() == "sandbox: killed by the kernel OOM killer" }

type fakeTimeout struct{}

func (*fakeTimeout) Error() string { return "timeout" }

func (e *fakeTimeout) Is(target error) bool { return target.Error() == "sandbox: sandbox lifetime expired" }

type fakeEvicted struct{}

func (*fakeEvicted) Error() string { return "evicted" }

func (e *fakeEvicted) Is(target error) bool {
	return target.Error() == "sandbox: container was stopped externally"
}

type fakeBusy struct{}

func (*fakeBusy) Error() string { return "busy" }

func (e *fakeBusy) Is(target error) bool { return target.Error() == "sandbox: another execution is running" }

type fakeAuth struct{}

func (*fakeAuth) Error() string { return "unauthorized" }

func (e *fakeAuth) Is(target error) bool { return target.Error() == "sandbox: unauthorized" }

type fakeNotFound struct{}

func (*fakeNotFound) Error() string { return "not found" }

func (e *fakeNotFound) Is(target error) bool { return target.Error() == "sandbox: not found" }

var _ = json.Marshal
