package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// TestTransitionTable walks every cell of the operation-by-state table in
// docs/api.md. Each cell asserts the documented result: a 409 invalid_state, a
// 200 no-op, or an actual transition.
func TestTransitionTable(t *testing.T) {
	ctx := context.Background()

	// stateFor returns a sandbox in the given state.
	stateFor := func(t *testing.T, h *harness, state daemon.State) daemon.Sandbox {
		t.Helper()
		sb := h.create(daemon.CreateRequest{})
		switch state {
		case daemon.StateRunning:
		case daemon.StatePaused:
			if _, err := h.s.Pause(ctx, sb.ID); err != nil {
				t.Fatalf("pause: %v", err)
			}
		case daemon.StateKilled:
			if _, err := h.s.Delete(ctx, sb.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
		case daemon.StatePending:
			t.Fatal("use the pending helper")
		}
		return sb
	}

	t.Run("pause", func(t *testing.T) {
		for _, tc := range []struct {
			state daemon.State
			want  daemon.State
			code  daemon.Code
		}{
			{daemon.StateRunning, daemon.StatePaused, ""},
			{daemon.StatePaused, daemon.StatePaused, ""},
			{daemon.StateKilled, "", daemon.CodeInvalidState},
		} {
			t.Run(string(tc.state), func(t *testing.T) {
				h := newHarness(t)
				sb := stateFor(t, h, tc.state)
				got, err := h.s.Pause(ctx, sb.ID)
				if tc.code != "" {
					wantCode(t, err, tc.code)
					return
				}
				wantNoError(t, err)
				if got.State != tc.want {
					t.Fatalf("state = %s, want %s", got.State, tc.want)
				}
			})
		}
	})

	t.Run("resume", func(t *testing.T) {
		for _, tc := range []struct {
			state daemon.State
			want  daemon.State
			code  daemon.Code
		}{
			{daemon.StatePaused, daemon.StateRunning, ""},
			{daemon.StateRunning, daemon.StateRunning, ""},
			{daemon.StateKilled, "", daemon.CodeInvalidState},
		} {
			t.Run(string(tc.state), func(t *testing.T) {
				h := newHarness(t)
				sb := stateFor(t, h, tc.state)
				got, err := h.s.Resume(ctx, sb.ID)
				if tc.code != "" {
					wantCode(t, err, tc.code)
					return
				}
				wantNoError(t, err)
				if got.State != tc.want {
					t.Fatalf("state = %s, want %s", got.State, tc.want)
				}
			})
		}
	})

	t.Run("delete", func(t *testing.T) {
		for _, state := range []daemon.State{
			daemon.StateRunning, daemon.StatePaused, daemon.StateKilled,
		} {
			t.Run(string(state), func(t *testing.T) {
				h := newHarness(t)
				sb := stateFor(t, h, state)
				got, err := h.s.Delete(ctx, sb.ID)
				wantNoError(t, err)
				if got.State != daemon.StateKilled {
					t.Fatalf("state = %s, want killed", got.State)
				}
				if got.KillReason == nil || *got.KillReason != daemon.KillRequested {
					t.Fatalf("kill_reason = %v, want requested", got.KillReason)
				}
				if got.ExpiresAt != nil {
					t.Fatalf("expires_at = %v, want null after kill", got.ExpiresAt)
				}
			})
		}
	})

	t.Run("start execution", func(t *testing.T) {
		for _, tc := range []struct {
			state daemon.State
			want  daemon.ExecState
			code  daemon.Code
		}{
			{daemon.StateRunning, daemon.ExecRunning, ""},
			{daemon.StatePaused, daemon.ExecRunning, ""},
			{daemon.StateKilled, "", daemon.CodeInvalidState},
		} {
			t.Run(string(tc.state), func(t *testing.T) {
				h := newHarness(t)
				sb := stateFor(t, h, tc.state)
				ex, created, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}})
				if tc.code != "" {
					wantCode(t, err, tc.code)
					return
				}
				wantNoError(t, err)
				if !created {
					t.Fatal("execution was not created")
				}
				if ex.State != tc.want {
					t.Fatalf("execution state = %s, want %s", ex.State, tc.want)
				}
				// Starting in a paused sandbox resumes it first.
				after, err := h.s.Get(sb.ID)
				wantNoError(t, err)
				if after.State != daemon.StateRunning {
					t.Fatalf("sandbox state = %s, want running", after.State)
				}
			})
		}
	})

	t.Run("files", func(t *testing.T) {
		for _, state := range []daemon.State{daemon.StateRunning, daemon.StatePaused} {
			t.Run(string(state), func(t *testing.T) {
				h := newHarness(t)
				sb := stateFor(t, h, state)
				wantNoError(t, h.s.WriteFile(ctx, sb.ID, "/tmp/a", []byte("hi")))
				data, err := h.s.ReadFile(ctx, sb.ID, "/tmp/a")
				wantNoError(t, err)
				if string(data) != "hi" {
					t.Fatalf("data = %q, want hi", data)
				}
				after, err := h.s.Get(sb.ID)
				wantNoError(t, err)
				if after.State != daemon.StateRunning {
					t.Fatalf("state = %s, want running (file access resumes)", after.State)
				}
			})
		}
		t.Run("killed", func(t *testing.T) {
			h := newHarness(t)
			sb := stateFor(t, h, daemon.StateKilled)
			wantCode(t, h.s.WriteFile(ctx, sb.ID, "/tmp/a", []byte("hi")), daemon.CodeInvalidState)
			_, err := h.s.ReadFile(ctx, sb.ID, "/tmp/a")
			wantCode(t, err, daemon.CodeInvalidState)
		})
		t.Run("relative path", func(t *testing.T) {
			h := newHarness(t)
			sb := stateFor(t, h, daemon.StateRunning)
			wantCode(t, h.s.WriteFile(ctx, sb.ID, "tmp/a", nil), daemon.CodeInvalidRequest)
			_, err := h.s.ReadFile(ctx, sb.ID, "tmp/a")
			wantCode(t, err, daemon.CodeInvalidRequest)
		})
		t.Run("missing file", func(t *testing.T) {
			h := newHarness(t)
			sb := stateFor(t, h, daemon.StateRunning)
			_, err := h.s.ReadFile(ctx, sb.ID, "/nope")
			wantCode(t, err, daemon.CodeNotFound)
		})
		t.Run("too large", func(t *testing.T) {
			h := newHarness(t)
			sb := stateFor(t, h, daemon.StateRunning)
			big := make([]byte, daemon.MaxFileBytes+1)
			wantCode(t, h.s.WriteFile(ctx, sb.ID, "/tmp/big", big), daemon.CodePayloadTooLarge)
		})
	})

	t.Run("get and list are always allowed", func(t *testing.T) {
		h := newHarness(t)
		sb := stateFor(t, h, daemon.StateKilled)
		if _, err := h.s.Get(sb.ID); err != nil {
			t.Fatalf("get killed sandbox: %v", err)
		}
		if got := h.s.List(); len(got) != 1 {
			t.Fatalf("list = %d sandboxes, want 1", len(got))
		}
	})

	t.Run("unknown sandbox", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.s.Get("sbx_missing")
		wantCode(t, err, daemon.CodeNotFound)
		_, err = h.s.Pause(ctx, "sbx_missing")
		wantCode(t, err, daemon.CodeNotFound)
		_, err = h.s.Delete(ctx, "sbx_missing")
		wantCode(t, err, daemon.CodeNotFound)
		_, _, err = h.s.StartExecution(ctx, "sbx_missing", daemon.ExecuteRequest{Command: []string{"true"}})
		wantCode(t, err, daemon.CodeNotFound)
	})
}

// TestPendingState exercises the fourth state of the transition table. Pending
// is only observable by a concurrent reader, so the backend is held inside
// Create while another goroutine inspects the sandbox.
func TestPendingState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	release := h.be.HoldCreate()

	created := make(chan daemon.Sandbox, 1)
	go func() {
		sb, err := h.s.Create(ctx, daemon.CreateRequest{})
		if err != nil {
			t.Errorf("create: %v", err)
			created <- daemon.Sandbox{}
			return
		}
		created <- sb
	}()

	var id string
	deadline := time.Now().Add(2 * time.Second)
	for id == "" && time.Now().Before(deadline) {
		if list := h.s.List(); len(list) > 0 {
			id = list[0].ID
			if list[0].State != daemon.StatePending {
				t.Fatalf("state while creating = %s, want pending", list[0].State)
			}
		}
	}
	if id == "" {
		t.Fatal("sandbox did not appear in pending state")
	}

	// Every operation the table marks invalid while pending.
	_, err := h.s.Pause(ctx, id)
	wantCode(t, err, daemon.CodeInvalidState)
	_, err = h.s.Resume(ctx, id)
	wantCode(t, err, daemon.CodeInvalidState)
	_, _, err = h.s.StartExecution(ctx, id, daemon.ExecuteRequest{Command: []string{"true"}})
	wantCode(t, err, daemon.CodeInvalidState)
	wantCode(t, h.s.WriteFile(ctx, id, "/a", nil), daemon.CodeInvalidState)
	// Reads are allowed.
	if _, err := h.s.Get(id); err != nil {
		t.Fatalf("get pending: %v", err)
	}

	// Delete from pending is allowed and is not undone by the create finishing.
	got, err := h.s.Delete(ctx, id)
	wantNoError(t, err)
	if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillRequested {
		t.Fatalf("delete from pending = %+v", got)
	}

	release()
	final := <-created
	if final.State != daemon.StateKilled {
		t.Fatalf("create returned %s, want killed", final.State)
	}
	if l := h.be.State(h.containerOf(id)); l.Alive() {
		t.Fatalf("container %s is %s, want removed", h.containerOf(id), l)
	}
}

// TestCreateDefaults checks the defaults and validation from api.md.
func TestCreateDefaults(t *testing.T) {
	ctx := context.Background()

	t.Run("defaults", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		if sb.Image != daemon.DefaultImage {
			t.Errorf("image = %s, want %s", sb.Image, daemon.DefaultImage)
		}
		if sb.MemoryMB != daemon.DefaultMemoryMB {
			t.Errorf("memory_mb = %d, want %d", sb.MemoryMB, daemon.DefaultMemoryMB)
		}
		if sb.TimeoutSeconds != daemon.DefaultTimeoutSeconds {
			t.Errorf("timeout_seconds = %d, want %d", sb.TimeoutSeconds, daemon.DefaultTimeoutSeconds)
		}
		if sb.TimeoutAction != daemon.ActionKill || sb.IdleTimeoutAction != daemon.ActionKill {
			t.Errorf("actions = %s/%s, want kill/kill", sb.TimeoutAction, sb.IdleTimeoutAction)
		}
		if sb.IdleTimeoutSeconds != 0 {
			t.Errorf("idle_timeout_seconds = %d, want 0", sb.IdleTimeoutSeconds)
		}
		if sb.State != daemon.StateRunning {
			t.Errorf("state = %s, want running", sb.State)
		}
		wantDeadline := sb.CreatedAt.Add(300 * time.Second)
		if sb.ExpiresAt == nil || !sb.ExpiresAt.Equal(wantDeadline) {
			t.Errorf("expires_at = %v, want %v", sb.ExpiresAt, wantDeadline)
		}
		if sb.KillReason != nil {
			t.Errorf("kill_reason = %v, want null", sb.KillReason)
		}
	})

	t.Run("explicit zero disables lifetime", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		if sb.TimeoutSeconds != 0 || sb.ExpiresAt != nil {
			t.Fatalf("timeout_seconds = %d, expires_at = %v; want 0 and null", sb.TimeoutSeconds, sb.ExpiresAt)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		h := newHarness(t)
		for name, req := range map[string]daemon.CreateRequest{
			"memory too small": {MemoryMB: ptr(15)},
			"negative timeout": {TimeoutSeconds: ptr(-1)},
			"bad action":       {TimeoutAction: ptr(daemon.Action("explode"))},
			"bad idle action":  {IdleTimeoutAction: ptr(daemon.Action("explode"))},
			"negative idle":    {IdleTimeoutSeconds: ptr(-5)},
			"empty image":      {Image: ptr("")},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := h.s.Create(ctx, req)
				wantCode(t, err, daemon.CodeInvalidRequest)
			})
		}
	})

	t.Run("backend failure", func(t *testing.T) {
		h := newHarness(t)
		h.be.CreateErr = errBoom
		_, err := h.s.Create(ctx, daemon.CreateRequest{})
		wantCode(t, err, daemon.CodeBackendError)
		if n := len(h.s.List()); n != 0 {
			t.Fatalf("list = %d after failed create, want 0", n)
		}
	})

	t.Run("newest first", func(t *testing.T) {
		h := newHarness(t)
		first := h.create(daemon.CreateRequest{})
		h.clock.Advance(time.Millisecond)
		second := h.create(daemon.CreateRequest{})
		list := h.s.List()
		if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
			t.Fatalf("list order = %v, want newest first", []string{list[0].ID, list[1].ID})
		}
	})
}
