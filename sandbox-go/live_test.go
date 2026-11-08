//go:build live

// Live SDK tests against a running sandboxd with a Docker backend:
//
//	make build && ./bin/sandboxd -addr 127.0.0.1:18080 &
//	SANDBOX_DOMAIN=http://127.0.0.1:18080 go test -tags live ./sandbox-go/ -v
//
// They prove the SDK against the real server, not a fake: SSE replay, live
// output, cancellation, OOM classification, and files.
package sandbox_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func liveClient(t *testing.T) *sandbox.Client {
	t.Helper()
	domain := os.Getenv("SANDBOX_DOMAIN")
	if domain == "" {
		domain = "http://127.0.0.1:18080"
	}
	c, err := sandbox.NewClient(sandbox.Config{Domain: domain})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.ListSandboxes(ctx); err != nil {
		t.Skipf("sandboxd not reachable at %s: %v", domain, err)
	}
	return c
}

func liveSandbox(t *testing.T, c *sandbox.Client, image string, memoryMB int) *sandbox.Sandbox {
	t.Helper()
	sb, err := c.CreateSandbox(context.Background(), sandbox.CreateSandboxRequest{
		Image:              ptr(image),
		MemoryMB:           ptr(memoryMB),
		TimeoutSeconds:     ptr(120),
		IdleTimeoutSeconds: ptr(60),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.KillSandbox(killCtx, sb.ID); err != nil {
			t.Errorf("kill %s: %v", sb.ID, err)
		}
	})
	return sb
}

func ptr[T any](v T) *T { return &v }

func TestLiveExecStreamsOutput(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	var events []sandbox.EventType
	ex, err := c.ExecStream(context.Background(), sb.ID, []string{"sh", "-c", "echo out; echo err >&2"}, func(ev sandbox.Event) error {
		events = append(events, ev.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if ex.Stdout != "out\n" || ex.Stderr != "err\n" {
		t.Errorf("stdout=%q stderr=%q", ex.Stdout, ex.Stderr)
	}
	if ex.TerminationReason == nil || *ex.TerminationReason != sandbox.ReasonExited {
		t.Errorf("reason = %v", ex.TerminationReason)
	}
	if len(events) == 0 || events[len(events)-1] != sandbox.EventComplete {
		t.Errorf("events = %v, want complete last", events)
	}
}

func TestLiveNonZeroExit(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	ex, err := c.Exec(context.Background(), sb.ID, []string{"sh", "-c", "exit 3"})
	if !errors.Is(err, sandbox.ErrNonZeroExit) {
		t.Fatalf("err = %v, want ErrNonZeroExit", err)
	}
	if ex == nil || ex.ExitCode == nil || *ex.ExitCode != 3 {
		t.Fatalf("execution = %+v", ex)
	}
}

func TestLiveCancel(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	started, err := c.StartExecution(context.Background(), sb.ID, sandbox.ExecuteRequest{
		Command: []string{"sh", "-c", "sleep 60 & wait"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	canceled, err := c.CancelExecution(context.Background(), sb.ID, started.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.TerminationReason == nil || *canceled.TerminationReason != sandbox.ReasonCanceled {
		t.Fatalf("reason = %v, want canceled", canceled.TerminationReason)
	}
	if canceled.State != "completed" {
		t.Errorf("state = %s", canceled.State)
	}
}

func TestLiveOOM(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "python:3.12-alpine", 64)

	_, err := c.Exec(context.Background(), sb.ID, []string{"python3", "-c", "x = bytearray(256 * 1024 * 1024)"})
	if !errors.Is(err, sandbox.ErrOOM) {
		t.Fatalf("err = %v, want ErrOOM", err)
	}
	var sdkErr *sandbox.Error
	if !errors.As(err, &sdkErr) || sdkErr.Execution == nil {
		t.Fatalf("error carries no execution: %+v", err)
	}
}

func TestLiveFiles(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	if err := c.WriteFile(context.Background(), sb.ID, "/work/nested/dir/f.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := c.ReadFile(context.Background(), sb.ID, "/work/nested/dir/f.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("read %q", data)
	}
	missing, err := c.ReadFile(context.Background(), sb.ID, "/nope")
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("missing file err = %v, want ErrNotFound (%q)", err, string(missing))
	}
}

func TestLiveReplayAfterComplete(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	ex, err := c.Exec(context.Background(), sb.ID, []string{"sh", "-c", "printf 'one\\ntwo\\n'"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	// A second stream of a finished execution replays and closes.
	var replayed strings.Builder
	again, err := c.StreamExecution(context.Background(), sb.ID, ex.ID, func(ev sandbox.Event) error {
		if ev.Type == sandbox.EventStdout {
			replayed.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.String() != "one\ntwo\n" {
		t.Errorf("replayed %q", replayed.String())
	}
	if again.Stdout != ex.Stdout {
		t.Errorf("replay execution differs: %q vs %q", again.Stdout, ex.Stdout)
	}
}
