package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// collect drains a stream into a slice.
func collect(t *testing.T, st *daemon.Stream) []daemon.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []daemon.Event
	for {
		events, done, err := st.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		out = append(out, events...)
		if done {
			return out
		}
	}
}

// TestEventStream covers the event contract: dense IDs from 1, chunk size,
// exactly one truncated event, complete always last, and replay that
// reproduces the retained output.
func TestEventStream(t *testing.T) {
	ctx := context.Background()

	t.Run("ids are dense and complete is last", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["echo"] = memEcho
		sb := h.create(daemon.CreateRequest{})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "a", "b"}})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		events := collect(t, st)

		if len(events) != 2 {
			t.Fatalf("got %d events, want 2: %+v", len(events), events)
		}
		for i, ev := range events {
			if ev.ID != uint64(i+1) {
				t.Fatalf("event %d has ID %d, want %d", i, ev.ID, i+1)
			}
		}
		if events[len(events)-1].Type != daemon.EventComplete {
			t.Fatalf("last event = %s, want complete", events[len(events)-1].Type)
		}
		complete := events[len(events)-1].Execution
		if complete == nil || complete.Stdout != "a\nb\n" {
			t.Fatalf("complete payload = %+v", complete)
		}
	})

	t.Run("replay from zero reproduces the retained output", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		payload := strings.Repeat("0123456789", 5000) // 50 KB
		h.be.Programs["flood"] = memFlood
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command: []string{"flood", "50000"},
		})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if done.Stdout != payload {
			t.Fatalf("retained %d bytes, want %d", len(done.Stdout), len(payload))
		}

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		var replayed strings.Builder
		for _, ev := range collect(t, st) {
			if ev.Type == daemon.EventStdout {
				replayed.Write(ev.Data)
			}
		}
		if replayed.String() != done.Stdout {
			t.Fatalf("replayed %d bytes, retained %d; they must be identical",
				replayed.Len(), len(done.Stdout))
		}
	})

	t.Run("chunks are at most 32 KiB", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Programs["flood"] = memFlood
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command: []string{"flood", "100000"},
		})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		events := collect(t, st)
		var chunks int
		for _, ev := range events {
			if ev.Type == daemon.EventStdout {
				chunks++
				if len(ev.Data) > daemon.MaxEventDataBytes {
					t.Fatalf("stdout chunk is %d bytes, limit is %d", len(ev.Data), daemon.MaxEventDataBytes)
				}
			}
		}
		if want := (100000 + daemon.MaxEventDataBytes - 1) / daemon.MaxEventDataBytes; chunks != want {
			t.Fatalf("got %d chunks, want %d", chunks, want)
		}
	})

	t.Run("last-event-id resumes without duplication", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Programs["flood"] = memFlood
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"flood", "70000"}})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)

		all, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		every := collect(t, all)
		all.Close()

		for _, resumeAfter := range []uint64{1, 2, 3} {
			st, err := h.s.Events(sb.ID, ex.ID, resumeAfter)
			wantNoError(t, err)
			rest := collect(t, st)
			st.Close()

			if len(rest) != len(every)-int(resumeAfter) {
				t.Fatalf("resume after %d returned %d events, want %d",
					resumeAfter, len(rest), len(every)-int(resumeAfter))
			}
			if rest[0].ID != resumeAfter+1 {
				t.Fatalf("resume after %d started at %d", resumeAfter, rest[0].ID)
			}
		}
	})

	t.Run("next never re-delivers an event", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Programs["flood"] = memFlood
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"flood", "10"}})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		seen := map[uint64]bool{}
		for i := 0; i < 4; i++ {
			events, _, err := st.Next(ctx)
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			for _, ev := range events {
				if seen[ev.ID] {
					t.Fatalf("event %d delivered twice", ev.ID)
				}
				seen[ev.ID] = true
			}
		}
	})
}

// TestTruncation covers the two retention bounds: 1 MiB of output and 4096
// output events. Exceeding either discards later output, sends one truncated
// event, sets output_truncated, and leaves the process running.
func TestTruncation(t *testing.T) {
	ctx := context.Background()

	t.Run("byte limit", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Streamers["spew"] = func(_ context.Context, write func([]byte, []byte) error) int {
			chunk := make([]byte, 32<<10)
			for i := range chunk {
				chunk[i] = 'x'
			}
			// 33 chunks is 1 MiB plus 32 KiB: the last one crosses the bound.
			for i := 0; i < 33; i++ {
				if err := write(chunk, nil); err != nil {
					return 1
				}
			}
			return 0
		}
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"spew"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)

		if !done.OutputTruncated {
			t.Fatal("output_truncated = false after exceeding 1 MiB")
		}
		if got := len(done.Stdout); got != daemon.MaxOutputBytes {
			t.Fatalf("retained %d bytes, want exactly %d", got, daemon.MaxOutputBytes)
		}
		if *done.ExitCode != 0 {
			t.Fatalf("exit = %d, want 0: hitting the limit must not stop the process", *done.ExitCode)
		}

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		var truncated int
		var last daemon.EventType
		for _, ev := range collect(t, st) {
			if ev.Type == daemon.EventTruncated {
				truncated++
				if ev.LimitBytes != daemon.MaxOutputBytes {
					t.Fatalf("truncated limit_bytes = %d, want %d", ev.LimitBytes, daemon.MaxOutputBytes)
				}
			}
			last = ev.Type
		}
		if truncated != 1 {
			t.Fatalf("got %d truncated events, want exactly 1", truncated)
		}
		if last != daemon.EventComplete {
			t.Fatalf("last event = %s, want complete", last)
		}
	})

	t.Run("event limit", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Streamers["drip"] = func(_ context.Context, write func([]byte, []byte) error) int {
			for i := 0; i < daemon.MaxOutputEvents+100; i++ {
				if err := write([]byte("x"), nil); err != nil {
					return 1
				}
			}
			return 0
		}
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"drip"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if !done.OutputTruncated {
			t.Fatal("output_truncated = false after exceeding the event bound")
		}

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		events := collect(t, st)
		var output int
		for _, ev := range events {
			switch ev.Type {
			case daemon.EventStdout, daemon.EventStderr:
				output++
			}
		}
		if output > daemon.MaxOutputEvents {
			t.Fatalf("%d output events, limit is %d", output, daemon.MaxOutputEvents)
		}
		if events[len(events)-1].Type != daemon.EventComplete {
			t.Fatalf("last event = %s, want complete", events[len(events)-1].Type)
		}
	})

	t.Run("stderr counts against the same budget", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Streamers["spew"] = func(_ context.Context, write func([]byte, []byte) error) int {
			chunk := make([]byte, 32<<10)
			for i := 0; i < 16; i++ {
				if err := write(chunk, nil); err != nil {
					return 1
				}
			}
			for i := 0; i < 16; i++ {
				if err := write(nil, chunk); err != nil {
					return 1
				}
			}
			return 0
		}
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"spew"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if len(done.Stdout) != daemon.MaxOutputBytes/2 || len(done.Stderr) != daemon.MaxOutputBytes/2 {
			t.Fatalf("stdout=%d stderr=%d, want half the budget each", len(done.Stdout), len(done.Stderr))
		}
		if done.OutputTruncated {
			t.Fatal("output_truncated = true with exactly the budget used")
		}
	})
}

// memFlood writes n copies of a ten-character pattern to stdout.
func memFlood(_ context.Context, args []string) (string, string, int) {
	n := 0
	for _, a := range args {
		for _, ch := range a {
			n = n*10 + int(ch-'0')
		}
	}
	pattern := "0123456789"
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(pattern)
	}
	return b.String()[:n], "", 0
}

// TestUTF8SurvivesChunkBoundaries pins the rule that no output event ends
// inside a UTF-8 sequence. Output events are JSON strings encoded one event at
// a time, so a rune split across two events would arrive as two U+FFFD
// replacement characters and the bytes the program wrote would be lost.
func TestUTF8SurvivesChunkBoundaries(t *testing.T) {
	ctx := context.Background()

	t.Run("a rune straddling the per-event limit", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		// Pad to one byte before the limit, then a three-byte character: the
		// cut lands inside it.
		pad := strings.Repeat("a", daemon.MaxEventDataBytes-1)
		h.be.Streamers["spew"] = func(_ context.Context, write func([]byte, []byte) error) int {
			if err := write([]byte(pad+"中文\n"), nil); err != nil {
				return 1
			}
			return 0
		}
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"spew"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		want := pad + "中文\n"
		if done.Stdout != want {
			t.Fatalf("retained output lost bytes across the boundary: %d bytes, want %d", len(done.Stdout), len(want))
		}

		st, err := h.s.Events(sb.ID, ex.ID, 0)
		wantNoError(t, err)
		defer st.Close()
		var replayed strings.Builder
		for _, ev := range collect(t, st) {
			if ev.Type != daemon.EventStdout {
				continue
			}
			// Round-trip through the same encoding the handler uses: that is
			// where a split rune would become U+FFFD.
			payload, err := json.Marshal(map[string]string{"data": string(ev.Data)})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var back struct{ Data string }
			if err := json.Unmarshal(payload, &back); err != nil {
				t.Fatalf("decode: %v", err)
			}
			replayed.WriteString(back.Data)
		}
		if replayed.String() != want {
			t.Fatalf("SSE round trip changed the output: %q", replayed.String())
		}
	})

	t.Run("a stream that ends mid-sequence is delivered at EOF", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		h.be.Streamers["odd"] = func(_ context.Context, write func([]byte, []byte) error) int {
			// 0xe4 starts a three-byte sequence that never completes.
			if err := write([]byte{'a', 0xe4, 0xb8}, nil); err != nil {
				return 1
			}
			return 0
		}
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"odd"}})
		wantNoError(t, err)
		done := h.waitCompleted(sb.ID, ex.ID)
		if done.Stdout != "a\xe4\xb8" {
			t.Fatalf("stdout = % x, want the raw bytes at EOF", done.Stdout)
		}
	})
}

// TestEventStreamRejectsACursorBeyondTheLog pins the rule that a Last-Event-ID
// past the newest event is invalid. No such event will ever exist, so a
// running execution would otherwise make the caller wait for a replay that
// cannot arrive.
func TestEventStreamRejectsACursorBeyondTheLog(t *testing.T) {
	h := newHarness(t)
	sb := h.create(daemon.CreateRequest{})
	h.be.Programs["echo"] = memEcho
	ex, _, err := h.s.StartExecution(context.Background(), sb.ID, daemon.ExecuteRequest{Command: []string{"echo", "hi"}})
	wantNoError(t, err)
	h.waitCompleted(sb.ID, ex.ID)

	if _, err := h.s.Events(sb.ID, ex.ID, 99); err == nil {
		t.Fatal("a cursor beyond the newest event was accepted")
	} else {
		wantCode(t, err, daemon.CodeInvalidRequest)
	}

	// The newest event ID itself is valid: it asks for nothing and closes.
	newest, err := h.s.LastEventID(sb.ID, ex.ID)
	wantNoError(t, err)
	st, err := h.s.Events(sb.ID, ex.ID, newest)
	wantNoError(t, err)
	defer st.Close()
	if events, _, err := st.Next(context.Background()); err != nil || len(events) != 0 {
		t.Fatalf("resume at the newest ID returned %d events, err %v", len(events), err)
	}
}

// TestExecutionHistoryIsPruned pins MaxExecutionsPerSandbox: the oldest
// completed executions and their idempotency keys are dropped, the running one
// never is.
func TestExecutionHistoryIsPruned(t *testing.T) {
	h := newHarness(t)
	h.be.Programs["echo"] = memEcho
	sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})

	firstID := ""
	for i := 0; i < daemon.MaxExecutionsPerSandbox+5; i++ {
		key := fmt.Sprintf("key-%d", i)
		ex, _, err := h.s.StartExecution(context.Background(), sb.ID, daemon.ExecuteRequest{
			Command:        []string{"echo", "x"},
			IdempotencyKey: key,
		})
		wantNoError(t, err)
		if firstID == "" {
			firstID = ex.ID
		}
		h.waitCompleted(sb.ID, ex.ID)
	}

	list, err := h.s.ListExecutions(sb.ID)
	wantNoError(t, err)
	if len(list) != daemon.MaxExecutionsPerSandbox {
		t.Fatalf("history holds %d executions, want %d", len(list), daemon.MaxExecutionsPerSandbox)
	}
	if _, err := h.s.GetExecution(sb.ID, firstID); err == nil {
		t.Fatal("the oldest execution survived the retention limit")
	}

	// The evicted key no longer replays; a fresh start with it runs again.
	ex, created, err := h.s.StartExecution(context.Background(), sb.ID, daemon.ExecuteRequest{
		Command:        []string{"echo", "x"},
		IdempotencyKey: "key-0",
	})
	wantNoError(t, err)
	if !created {
		t.Fatal("a pruned idempotency key reported an existing execution")
	}
	h.waitCompleted(sb.ID, ex.ID)
}
