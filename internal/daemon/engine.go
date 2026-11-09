package daemon

import (
	"context"
	"io"
	"log"
	"sync"
	"time"
)

// intentRecord is the termination intent the daemon recorded before it acted.
// Recorded intents win over whatever exit status follows, which is why a 137
// from a cancel is never classified as oom or exited.
type intentRecord struct {
	reason Reason
}

// execution is one execution and its live engine state.
//
// Lock order: the sandbox lock may be held while taking an execution lock, but
// never the reverse. Code holding an execution lock therefore never touches
// its sandbox; finishing an execution hands the cleanup back to the runner.
type execution struct {
	mu sync.Mutex

	id        string
	sandboxID string
	command   []string
	key       string

	startedAt time.Time
	finished  *time.Time

	state    ExecState
	exitCode *int
	reason   *Reason
	log      *eventLog

	// intent is the recorded termination intent: canceled or timeout. It is
	// set before the daemon acts and never cleared, so it wins over whatever
	// exit status follows.
	intent *intentRecord

	proc        Process
	oomBaseline int64
	done        chan struct{}
	// confirmedGone is closed once the process tree is known to be gone. A
	// cancel that cannot confirm this removes the container instead.
}

// setIntent records the first termination intent. Later intents do not
// overwrite it: the first reason the daemon acted on is the true one.
func (ex *execution) setIntent(reason Reason) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.intent == nil && ex.state == ExecRunning {
		ex.intent = &intentRecord{reason: reason}
	}
}

// finish completes the execution exactly once. It records the outcome, appends
// the terminal event, and closes done. It does not touch the sandbox.
func (ex *execution) finish(at time.Time, exitCode int, outcome ExecOutcome) bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.state == ExecCompleted {
		return false
	}

	ex.state = ExecCompleted
	f := at.UTC()
	ex.finished = &f
	if exitCode >= 0 {
		code := exitCode
		ex.exitCode = &code
	} else {
		ex.exitCode = nil
	}
	reason := outcome.Reason
	ex.reason = &reason

	stdout, stderr, truncated := ex.log.output()
	ex.log.appendComplete(Execution{
		ID:                ex.id,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		State:             ExecCompleted,
		ExitCode:          ex.exitCode,
		TerminationReason: ex.reason,
		Stdout:            string(stdout),
		Stderr:            string(stderr),
		OutputTruncated:   truncated,
		StartedAt:         ex.startedAt,
		FinishedAt:        ex.finished,
	})
	close(ex.done)
	return true
}

// snapshot returns the contract view with retained output.
func (ex *execution) snapshot() Execution {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.snapshotLocked()
}

func (ex *execution) snapshotLocked() Execution {
	stdout, stderr, truncated := ex.log.output()
	return Execution{
		ID:                ex.id,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		State:             ex.state,
		ExitCode:          cloneInt(ex.exitCode),
		TerminationReason: cloneReason2(ex.reason),
		Stdout:            string(stdout),
		Stderr:            string(stderr),
		OutputTruncated:   truncated,
		StartedAt:         ex.startedAt,
		FinishedAt:        cloneTime(ex.finished),
	}
}

func (ex *execution) completed() bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.state == ExecCompleted
}

// summary returns the contract view without copying retained output. List
// endpoints use it so a sandbox with a long history does not copy every
// execution's stdout and stderr only to discard them.
func (ex *execution) summary() ExecutionSummary {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ExecutionSummary{
		ID:                ex.id,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		State:             ex.state,
		ExitCode:          cloneInt(ex.exitCode),
		TerminationReason: cloneReason2(ex.reason),
		OutputTruncated:   ex.log.truncatedOutput(),
		StartedAt:         ex.startedAt,
		FinishedAt:        cloneTime(ex.finished),
	}
}

// ---------------------------------------------------------------- start

// StartExecution starts command in the sandbox.
//
// The rules come straight from api.md: the sandbox's state decides whether a
// start is allowed; a paused sandbox resumes first; one execution runs at a
// time, so an overlapping start is busy; and a reused idempotency key returns
// the existing execution instead of starting a second process.
func (s *Sandboxes) StartExecution(ctx context.Context, sandboxID string, req ExecuteRequest) (Execution, bool, error) {
	if len(req.Command) == 0 {
		return Execution{}, false, errorf(CodeInvalidRequest, "command must not be empty")
	}
	if req.IdempotencyKey != "" && len(req.IdempotencyKey) > MaxIdempotencyKey {
		return Execution{}, false, errorf(CodeInvalidRequest, "idempotency_key exceeds %d characters", MaxIdempotencyKey)
	}

	sb, err := s.lookup(sandboxID)
	if err != nil {
		return Execution{}, false, err
	}
	sb.mu.Lock()

	// State comes first: a killed or pending sandbox rejects any start, even a
	// replay of an idempotency key it accepted earlier. Idempotency answers
	// "did this request already run", not "is this sandbox usable".
	switch sb.state {
	case StateRunning:
	case StatePaused:
		if err := s.unpauseLocked(ctx, sb); err != nil {
			sb.mu.Unlock()
			return Execution{}, false, err
		}
	default:
		err := sb.invalidState()
		sb.mu.Unlock()
		return Execution{}, false, err
	}

	if key := req.IdempotencyKey; key != "" {
		if prior := sb.idempotency[key]; prior != nil {
			prior.mu.Lock()
			same := equalStrings(prior.command, req.Command)
			prior.mu.Unlock()
			if !same {
				sb.mu.Unlock()
				return Execution{}, false, errorf(CodeIdempotencyConflict,
					"idempotency key was used for a different command")
			}
			out := prior.snapshot()
			sb.mu.Unlock()
			return out, false, nil
		}
	}

	if sb.running != nil {
		runningID := sb.running.id
		sb.mu.Unlock()
		return Execution{}, false, errorf(CodeBusy, "sandbox %s is already running execution %s", sandboxID, runningID)
	}

	// The counter read happens while the request is still alive, so the
	// request's context is fine there. The process itself outlives the
	// request that started it: net/http cancels the request context when the
	// handler returns, and a command must keep running after that.
	//
	// One failed read would otherwise disable OOM attribution for the whole
	// execution (baseline -1 makes the classifier skip the delta check), so
	// the read is retried once. A second failure is rare and is reported in
	// the log, because OOM detection is the one thing a 137 cannot recover
	// from afterwards.
	baseline, oomErr := s.readOOMPaseline(ctx, sb.container)
	if oomErr != nil {
		log.Printf("daemon: oom baseline for %s unavailable, oom detection disabled for this execution: %v",
			sb.id, oomErr)
	}

	proc, err := s.backend.Exec(context.WithoutCancel(ctx), sb.container, req.Command)
	if err != nil {
		sb.mu.Unlock()
		return Execution{}, false, backendError(err)
	}

	ex := &execution{
		id:          newID("exe_"),
		sandboxID:   sandboxID,
		command:     append([]string(nil), req.Command...),
		key:         req.IdempotencyKey,
		startedAt:   s.now(),
		state:       ExecRunning,
		log:         newEventLog(),
		proc:        proc,
		oomBaseline: baseline,
		done:        make(chan struct{}),
	}
	sb.executions = append(sb.executions, ex)
	sb.running = ex
	if ex.key != "" {
		sb.idempotency[ex.key] = ex
	}
	sb.lastActivity = s.now()
	sb.pruneExecutionsLocked()
	// The contract reports the execution as running with empty output; the
	// pumps below deliver the rest through the event log.
	out := ex.snapshotLocked()
	sb.mu.Unlock()

	go s.run(sb, ex)
	return out, true, nil
}

// readOOMPaseline returns the cgroup oom_kill counter, retrying once. A
// failed read is reported to the caller, which records -1: the classifier then
// skips the delta check rather than guess.
func (s *Sandboxes) readOOMPaseline(ctx context.Context, containerID string) (int64, error) {
	baseline, err := s.backend.OOMKillCount(ctx, containerID)
	if err == nil {
		return baseline, nil
	}
	baseline, retryErr := s.backend.OOMKillCount(ctx, containerID)
	if retryErr != nil {
		return -1, retryErr
	}
	return baseline, nil
}

// run pumps output, waits for the process, classifies the exit, and finishes
// the execution. It is the only writer of the terminal state.
func (s *Sandboxes) run(sb *sandbox, ex *execution) {
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		pump(ex.log, EventStdout, ex.proc.Stdout())
	}()
	go func() {
		defer pumps.Done()
		pump(ex.log, EventStderr, ex.proc.Stderr())
	}()

	exitCode, waitErr := ex.proc.Wait()
	pumps.Wait()

	ex.mu.Lock()
	intent := ex.intent
	baseline := ex.oomBaseline
	ex.mu.Unlock()

	outcome := classify(context.WithoutCancel(context.Background()), s.backend, sb.container, baseline, exitCode, intent, s.sleep)
	if len(outcome.sampled) > 0 {
		// One line per ambiguous 137: the observed liveness sequence and how
		// long it took to settle. This is the evidence a machine we cannot
		// reproduce on leaves behind for a misclassification.
		log.Printf("daemon: execution %s exit 137: liveness %v settled after %s -> %s",
			ex.id, outcome.sampled, outcome.elapsed.Round(time.Millisecond), outcome.Reason)
	}
	if waitErr != nil && intent == nil && !outcome.SandboxLost {
		// The runtime failed to report an exit status. Keep the reason the
		// classification gave, but the exit code stays unknown.
		exitCode = -1
	}

	now := s.now()
	if ex.finish(now, exitCode, outcome) {
		sb.mu.Lock()
		if sb.running == ex {
			sb.running = nil
		}
		sb.lastActivity = now
		sb.mu.Unlock()
	}

	if outcome.SandboxLost {
		sb.mu.Lock()
		s.terminateLocked(context.WithoutCancel(context.Background()), sb, KillEvicted)
		sb.mu.Unlock()
	}
}

// pump copies r into the event log in chunks no larger than the per-event
// limit. A read can end inside a UTF-8 sequence, and an event whose payload is
// half a rune would be turned into U+FFFD by the JSON encoder on the wire, so
// the incomplete tail is carried into the next read instead of being
// delivered. At most utf8.UTFMax-1 bytes are ever pending. A read error ends
// the pump; the process wait is the authority on the outcome.
func pump(log *eventLog, kind EventType, r io.Reader) {
	if r == nil {
		return
	}
	buf := make([]byte, MaxEventDataBytes)
	carry := 0
	for {
		// Read at most MaxEventDataBytes in total, so a large write produces
		// full events rather than a full one plus a small remainder.
		n, err := r.Read(buf[carry:])
		total := carry + n

		// Hold back a partial rune; the next read completes it. Nothing is
		// held back at EOF: a truncated sequence has no completion coming,
		// and the encoder will substitute U+FFFD for it.
		tail := 0
		if err == nil && total > 0 {
			tail = incompleteRuneTail(buf[:total])
		}
		if total-tail > 0 {
			log.appendOutput(kind, buf[:total-tail])
		}
		carry = tail
		if tail > 0 {
			// Move the pending bytes to the front only after the data before
			// them has been sent: copying first would overwrite that data.
			copy(buf, buf[total-tail:total])
		}
		if err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- cancel

// CancelExecution terminates an execution's whole process group: SIGTERM,
// then SIGKILL after the grace period. It answers only once the execution is
// completed. Canceling a completed execution returns it unchanged.
//
// If the process tree cannot be confirmed gone within the cancel deadline, the
// container is removed so nothing keeps running, and the sandbox ends killed
// with reason requested.
func (s *Sandboxes) CancelExecution(ctx context.Context, sandboxID, executionID string) (Execution, error) {
	sb, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return Execution{}, err
	}
	if ex.completed() {
		return ex.snapshot(), nil
	}

	// A paused container delivers no signals, and the contract says a cancel
	// in a paused sandbox resumes it first.
	sb.mu.Lock()
	if sb.state == StatePaused {
		if err := s.backend.Unpause(ctx, sb.container); err != nil {
			sb.mu.Unlock()
			return Execution{}, s.backendFailure(ctx, sb, err)
		}
		sb.state = StateRunning
		sb.armLifetimeLocked(s.now())
	}
	sb.mu.Unlock()

	// Record the intent before acting: it explains whatever exit code follows.
	ex.setIntent(ReasonCanceled)

	// The escalation waits below must not inherit the caller's request
	// context. A client that gives up on the cancel request (a timeout, a
	// closed terminal, a dropped proxy connection) would otherwise cut both
	// waits short and move straight to removing the container, turning a
	// slow signal into sandbox destruction. The cancel grace and deadline
	// bound the action; the connection does not.
	cleanup := context.WithoutCancel(ctx)
	_ = ex.proc.Signal(SignalTerm)
	if s.waitDone(cleanup, ex.done, s.cancelGrace) {
		return ex.snapshot(), nil
	}
	_ = ex.proc.Signal(SignalKill)
	if s.waitDone(cleanup, ex.done, s.cancelDeadline-s.cancelGrace) {
		return ex.snapshot(), nil
	}

	// The process tree could not be confirmed gone. Removing the container is
	// the only way to guarantee nothing keeps running.
	sb.mu.Lock()
	s.terminateLocked(cleanup, sb, KillRequested)
	sb.mu.Unlock()
	// Removing the container ends the process, so the runner finishes the
	// execution. Give it a moment, then report what is known.
	s.waitDone(cleanup, ex.done, s.cancelGrace)
	return ex.snapshot(), nil
}

// waitDone waits for done or ctx or the timeout, reporting whether done
// closed.
func (s *Sandboxes) waitDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// ---------------------------------------------------------------- reads

// GetExecution returns one execution with retained output.
func (s *Sandboxes) GetExecution(sandboxID, executionID string) (Execution, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return Execution{}, err
	}
	return ex.snapshot(), nil
}

// ListExecutions returns every execution of the sandbox, newest first, without
// output.
func (s *Sandboxes) ListExecutions(sandboxID string) ([]ExecutionSummary, error) {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	list := append([]*execution(nil), sb.executions...)
	sb.mu.Unlock()

	out := make([]ExecutionSummary, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		// summary() reads the fields without copying retained output, so a
		// long history costs one slice allocation, not one output copy per
		// execution.
		out = append(out, list[i].summary())
	}
	return out, nil
}

// execution looks up one execution inside a sandbox.
func (s *Sandboxes) execution(sandboxID, executionID string) (*sandbox, *execution, error) {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return nil, nil, err
	}
	sb.mu.Lock()
	var found *execution
	for _, ex := range sb.executions {
		if ex.id == executionID {
			found = ex
			break
		}
	}
	sb.mu.Unlock()
	if found == nil {
		return nil, nil, errorf(CodeNotFound, "execution %s does not exist in sandbox %s", executionID, sandboxID)
	}
	return sb, found, nil
}

// ---------------------------------------------------------------- events

// Stream delivers one execution's events. It keeps its own cursor, so calling
// Next repeatedly never re-delivers an event: that is what makes a reconnect
// with Last-Event-ID lossless. Closing a stream never affects the execution.
type Stream struct {
	log    *eventLog
	cursor uint64
	once   sync.Once
	close  func()
}

// Events returns a Stream positioned after lastEventID. A lastEventID of 0
// replays from the first event, which reproduces the retained stdout and
// stderr exactly. The stream must be closed; an open stream counts as
// activity for the idle timeout.
//
// A cursor beyond the newest event is rejected: no event will ever have the
// requested ID, and a running execution would otherwise leave the caller
// waiting for a replay that cannot arrive.
func (s *Sandboxes) Events(sandboxID, executionID string, lastEventID uint64) (*Stream, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return nil, err
	}
	if newest := ex.log.lastEventID(); lastEventID > newest {
		return nil, errorf(CodeInvalidRequest,
			"Last-Event-ID %d is beyond the newest event %d", lastEventID, newest)
	}
	closeStream, err := s.OpenStream(sandboxID)
	if err != nil {
		return nil, err
	}
	return &Stream{log: ex.log, cursor: lastEventID, close: closeStream}, nil
}

// Next returns every event after the cursor, blocking until at least one is
// available, the log completes, or ctx is done. done is true once the complete
// event has been delivered.
func (st *Stream) Next(ctx context.Context) (events []Event, done bool, err error) {
	events, done, err = st.log.wait(ctx, st.cursor)
	if len(events) > 0 {
		st.cursor = events[len(events)-1].ID
	}
	return events, done, err
}

// Close releases the stream.
func (st *Stream) Close() {
	st.once.Do(st.close)
}

// LastEventID returns the newest event ID of an execution, for diagnostics
// and for a client that wants to resume without replaying.
func (s *Sandboxes) LastEventID(sandboxID, executionID string) (uint64, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return 0, err
	}
	return ex.log.lastEventID(), nil
}

// ---------------------------------------------------------------- files

// ReadFile returns a file inside the sandbox. A paused sandbox resumes first,
// as the transition table says.
func (s *Sandboxes) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	if !isAbsPath(path) {
		return nil, errorf(CodeInvalidRequest, "path must be absolute")
	}
	sb, err := s.prepareFiles(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()

	data, err := s.backend.ReadFile(ctx, container, path)
	if err != nil {
		return nil, backendError(err)
	}
	return data, nil
}

// WriteFile writes a file inside the sandbox, creating parent directories.
func (s *Sandboxes) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	if !isAbsPath(path) {
		return errorf(CodeInvalidRequest, "path must be absolute")
	}
	if len(data) > MaxFileBytes {
		return errorf(CodePayloadTooLarge, "file exceeds %d bytes", MaxFileBytes)
	}
	sb, err := s.prepareFiles(ctx, sandboxID)
	if err != nil {
		return err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()

	if err := s.backend.WriteFile(ctx, container, path, data); err != nil {
		return backendError(err)
	}
	return nil
}

// prepareFiles applies the state rules for file access: allowed while running,
// a resume first while paused, invalid while pending or killed.
func (s *Sandboxes) prepareFiles(ctx context.Context, sandboxID string) (*sandbox, error) {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	switch sb.state {
	case StateRunning:
		return sb, nil
	case StatePaused:
		if err := s.unpauseLocked(ctx, sb); err != nil {
			return nil, err
		}
		return sb, nil
	default:
		return nil, sb.invalidState()
	}
}

func isAbsPath(path string) bool {
	return len(path) > 0 && path[0] == '/'
}

// ---------------------------------------------------------------- small helpers

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneReason2(r *Reason) *Reason {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}
