package daemon

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Clock is the daemon's only source of wall time. Tests inject a clock they
// advance by hand, so timeout behaviour is deterministic.
type Clock interface {
	Now() time.Time
}

// realClock is the production clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// SleepFunc waits for d or until ctx is done, reporting whether the sleep ran
// to completion. Classification uses it for the ambiguous-137 re-sampling
// window, so tests do not spend wall-clock time.
type SleepFunc func(ctx context.Context, d time.Duration) bool

// Option configures Sandboxes.
type Option func(*Sandboxes)

// WithClock replaces the clock.
func WithClock(c Clock) Option { return func(s *Sandboxes) { s.clock = c } }

// WithCleanupInterval sets how long the reaper waits between checks on a
// sandbox that has nothing due. It is capped at one second, as api.md requires.
func WithCleanupInterval(d time.Duration) Option {
	return func(s *Sandboxes) { s.cleanupInterval = d }
}

// WithClassifierSleep replaces the sleep classification uses while re-sampling
// container liveness after an ambiguous exit.
func WithClassifierSleep(f SleepFunc) Option { return func(s *Sandboxes) { s.sleep = f } }

// WithOwnerLabel sets the value of the tiny-sandbox.owner label on every
// container, so a restart removes its own leftovers.
func WithOwnerLabel(owner string) Option { return func(s *Sandboxes) { s.owner = owner } }

// WithCancelGrace sets how long a cancel waits between SIGTERM and SIGKILL.
func WithCancelGrace(d time.Duration) Option { return func(s *Sandboxes) { s.cancelGrace = d } }

// WithCancelDeadline sets how long a cancel waits for confirmation before it
// removes the container.
func WithCancelDeadline(d time.Duration) Option { return func(s *Sandboxes) { s.cancelDeadline = d } }

// OwnerLabelKey is the label that ties a container to the server instance
// that created it.
const OwnerLabelKey = "tiny-sandbox.owner"

// Ambiguity re-sampling: after an exit code of 137 with no recorded intent and
// no OOM delta, container liveness is sampled until it settles or this window
// runs out, and the last observation decides. A confirmed stop ends the wait
// early, so the usual external kill costs one or two intervals; the window is
// the bound for the case CI showed, where the daemon keeps reporting "running"
// long after the kill. The 137 path is the only one that pays this cost, and it
// fits the five-second eviction budget in api.md.
const (
	ambiguousWindow   = 2 * time.Second
	ambiguousInterval = 50 * time.Millisecond
	// ambiguousMinSamples is how many answers are collected before a
	// confirmed stop may end the wait early.
	ambiguousMinSamples = 3
	ambiguousSamples    = int(ambiguousWindow / ambiguousInterval)
)

// Sandboxes is the daemon's in-memory store of sandboxes and their
// executions, plus every lifecycle transition in docs/api.md. It is the
// domain object the HTTP handlers call; it knows nothing about HTTP.
//
// State is per process and lost on restart, as the contract says. Each sandbox
// has its own mutex, so a blocking cancel on one sandbox never delays requests
// on another, and the reaper skips a sandbox whose lock is held instead of
// waiting for it.
type Sandboxes struct {
	backend Backend
	clock   Clock
	sleep   SleepFunc

	cleanupInterval time.Duration
	owner           string
	cancelGrace     time.Duration
	cancelDeadline  time.Duration

	mu    sync.Mutex
	byID  map[string]*sandbox
	order []*sandbox // creation order, for newest-first listings

	// retryMu guards retryIDs: containers the runtime would not remove. It is
	// a separate lock so a retry pass never waits on the store lock, and the
	// reaper never waits on a removal it is itself scheduling.
	retryMu  sync.Mutex
	retryIDs map[string]struct{}
}

// maxRemovalRetries bounds the removal retry set. A runtime that cannot remove
// this many containers has a problem the retry loop cannot solve; startup
// cleanup still catches whatever is dropped.
const maxRemovalRetries = 256

// New returns Sandboxes backed by backend.
func New(backend Backend, opts ...Option) *Sandboxes {
	s := &Sandboxes{
		backend:         backend,
		clock:           realClock{},
		cleanupInterval: time.Second,
		cancelGrace:     2 * time.Second,
		cancelDeadline:  10 * time.Second,
		sleep:           sleepContext,
		byID:            make(map[string]*sandbox),
		retryIDs:        make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.cleanupInterval <= 0 || s.cleanupInterval > time.Second {
		s.cleanupInterval = time.Second
	}
	if s.cancelGrace <= 0 {
		s.cancelGrace = 2 * time.Second
	}
	if s.cancelDeadline <= s.cancelGrace {
		s.cancelDeadline = 10 * time.Second
	}
	return s
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// now is the store's single source of time, always in UTC.
func (s *Sandboxes) now() time.Time { return s.clock.Now().UTC() }

// sandbox is one sandbox and everything that belongs to it.
type sandbox struct {
	mu sync.Mutex

	id        string
	container string
	opts      sandboxOptions
	createdAt time.Time

	state      State
	killReason *KillReason
	// expiresAt is non-nil only while a lifetime deadline is armed: for a
	// kill-action sandbox in any live state, and for a pause-action sandbox
	// only while running.
	expiresAt *time.Time
	// lastActivity is the latest of: an authenticated request addressing the
	// sandbox, the end of an execution, and the close of an event stream.
	lastActivity time.Time

	executions  []*execution // in start order; newest last
	running     *execution
	idempotency map[string]*execution

	streams int
}

// pruneExecutionsLocked enforces MaxExecutionsPerSandbox. Callers hold sb.mu.
//
// Dropping a completed execution also drops its idempotency key, so a replay
// of a key that is older than the retained history starts a new execution
// instead of returning the old record. That is the deliberate trade: keeping
// every key forever makes the map grow without bound, and the alternative —
// keeping the record but discarding its output — still costs a record per
// command and still leaves the memory question open. The contract documents the
// window. The running execution is never dropped.
func (sb *sandbox) pruneExecutionsLocked() {
	if len(sb.executions) <= MaxExecutionsPerSandbox {
		return
	}
	drop := 0
	for drop < len(sb.executions) && len(sb.executions)-drop > MaxExecutionsPerSandbox {
		ex := sb.executions[drop]
		if ex == sb.running || !ex.completed() {
			// Stop at the first execution that must stay: dropping a later
			// one would leave a hole in the history and in event replay.
			return
		}
		if ex.key != "" && sb.idempotency[ex.key] == ex {
			delete(sb.idempotency, ex.key)
		}
		drop++
	}
	sb.executions = append([]*execution(nil), sb.executions[drop:]...)
}

// ---------------------------------------------------------------- creation

// Create starts a container and returns the sandbox once it is running.
func (s *Sandboxes) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	opts, err := req.resolve()
	if err != nil {
		return Sandbox{}, err
	}

	now := s.now()
	id := newID("sbx_")
	sb := &sandbox{
		id:           id,
		opts:         opts,
		createdAt:    now,
		state:        StatePending,
		lastActivity: now,
		idempotency:  make(map[string]*execution),
	}

	s.mu.Lock()
	s.byID[id] = sb
	s.order = append(s.order, sb)
	s.mu.Unlock()

	spec := ContainerSpec{
		Image:    opts.image,
		MemoryMB: opts.memoryMB,
		Name:     "tiny-sandbox-" + id,
		Labels:   map[string]string{OwnerLabelKey: s.owner},
	}
	containerID, err := s.backend.Create(ctx, spec)
	if err != nil {
		// The container may exist despite the error; removing it is the only
		// way to be sure. A failed remove is left for startup cleanup by
		// label.
		_ = s.backend.Remove(ctx, containerID)
		s.forget(sb)
		return Sandbox{}, backendError(err)
	}

	sb.mu.Lock()
	sb.container = containerID
	if sb.state != StatePending {
		// The sandbox was killed while the container was being created; the
		// transition table allows delete from pending, so the container that
		// appeared afterwards must not survive.
		sb.mu.Unlock()
		_ = s.backend.Remove(context.WithoutCancel(ctx), containerID)
		return sb.snapshot(), nil
	}
	sb.state = StateRunning
	sb.armLifetimeLocked(s.now())
	sb.mu.Unlock()
	return sb.snapshot(), nil
}

// forget removes a sandbox that never started.
func (s *Sandboxes) forget(sb *sandbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, sb.id)
	for i, other := range s.order {
		if other == sb {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

// ---------------------------------------------------------------- reads

// Get returns one sandbox. Reads never change state.
func (s *Sandboxes) Get(id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.snapshot(), nil
}

// List returns every sandbox, newest first. Killed sandboxes are included
// until the process restarts.
func (s *Sandboxes) List() []Sandbox {
	s.mu.Lock()
	order := append([]*sandbox(nil), s.order...)
	s.mu.Unlock()

	out := make([]Sandbox, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		order[i].mu.Lock()
		out = append(out, order[i].snapshot())
		order[i].mu.Unlock()
	}
	return out
}

// Touch records an authenticated request that addressed the sandbox. The idle
// timeout measures from the latest such request, the last execution end, and
// the last stream close.
func (s *Sandboxes) Touch(id string) {
	sb, err := s.lookup(id)
	if err != nil {
		return
	}
	sb.mu.Lock()
	sb.lastActivity = s.now()
	sb.mu.Unlock()
}

// OpenStream records an open event stream for the idle timeout and returns the
// function that closes it. An open stream is activity, and a stream that stays
// open keeps the sandbox from being idle.
func (s *Sandboxes) OpenStream(id string) (closeStream func(), err error) {
	sb, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	sb.streams++
	sb.lastActivity = s.now()
	sb.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			sb.mu.Lock()
			sb.streams--
			sb.lastActivity = s.now()
			sb.mu.Unlock()
		})
	}, nil
}

// ---------------------------------------------------------------- lifecycle

// Pause freezes the sandbox. It is invalid while pending or killed and a
// no-op while paused.
func (s *Sandboxes) Pause(ctx context.Context, id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()

	switch sb.state {
	case StatePaused:
		return sb.snapshot(), nil
	case StateRunning:
	default:
		return Sandbox{}, sb.invalidState()
	}

	if err := s.backend.Pause(ctx, sb.container); err != nil {
		return Sandbox{}, s.backendFailure(ctx, sb, err)
	}
	sb.state = StatePaused
	// A pause-timeout sandbox does not count while paused, so it shows no
	// deadline. A kill-timeout sandbox keeps counting, as api.md says.
	if sb.opts.timeoutAction == ActionPause {
		sb.expiresAt = nil
	}
	return sb.snapshot(), nil
}

// Resume thaws the sandbox. It is invalid while pending or killed and a no-op
// while running.
func (s *Sandboxes) Resume(ctx context.Context, id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()

	switch sb.state {
	case StateRunning:
		return sb.snapshot(), nil
	case StatePaused:
	default:
		return Sandbox{}, sb.invalidState()
	}
	if err := s.unpauseLocked(ctx, sb); err != nil {
		return Sandbox{}, err
	}
	return sb.snapshot(), nil
}

// unpauseLocked resumes the container and arms a fresh deadline when the
// lifetime timeout acts by pausing. Every transition to running starts a new
// interval; otherwise an automatic resume would expire again immediately.
func (s *Sandboxes) unpauseLocked(ctx context.Context, sb *sandbox) error {
	if err := s.backend.Unpause(ctx, sb.container); err != nil {
		return s.backendFailure(ctx, sb, err)
	}
	sb.state = StateRunning
	sb.armLifetimeLocked(s.now())
	return nil
}

// Delete kills the sandbox with reason requested and removes its container. It
// is idempotent.
func (s *Sandboxes) Delete(ctx context.Context, id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if sb.state == StateKilled {
		return sb.snapshot(), nil
	}
	s.terminateLocked(ctx, sb, KillRequested)
	return sb.snapshot(), nil
}

// killLocked moves the sandbox to killed and records why. A running execution
// inherits the outcome: an explicit delete cancels it, either timeout marks it
// timed out, and an eviction completes it as evicted. This is the only place
// the terminal state is set.
func (s *Sandboxes) killLocked(sb *sandbox, reason KillReason) {
	if sb.state == StateKilled {
		return
	}
	sb.state = StateKilled
	r := reason
	sb.killReason = &r
	sb.expiresAt = nil

	ex := sb.running
	if ex == nil {
		return
	}
	switch reason {
	case KillRequested:
		ex.setIntent(ReasonCanceled)
	case KillTimeout, KillIdleTimeout:
		ex.setIntent(ReasonTimeout)
	case KillEvicted:
		// The container is already gone; nothing to wait for. Complete the
		// execution right away so streams see the terminal event within the
		// five-second detection budget.
		sb.running = nil
		ex.finish(s.now(), -1, ExecOutcome{Reason: ReasonEvicted, ExitCode: -1, SandboxLost: true})
	}
}

// terminateLocked kills the sandbox and removes its container. Removal is
// best effort: a container that is already gone is not an error. The removal
// is detached from the caller's context (a delete must not be cancelled
// halfway by a disconnecting client) but bounded, so a runtime that never
// answers does not pin the sandbox lock forever. A removal that fails or times
// out is scheduled for the reaper to retry: the record is already killed, and
// nothing else would revisit that container.
func (s *Sandboxes) terminateLocked(ctx context.Context, sb *sandbox, reason KillReason) {
	s.killLocked(sb, reason)
	if sb.container == "" {
		return
	}
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackendCallTimeout)
	defer cancel()
	if err := s.backend.Remove(removeCtx, sb.container); err != nil && !errors.Is(err, ErrNotFound) {
		s.scheduleRemoval(sb.container)
	}
}

// ---------------------------------------------------------------- helpers

func (s *Sandboxes) lookup(id string) (*sandbox, error) {
	s.mu.Lock()
	sb := s.byID[id]
	s.mu.Unlock()
	if sb == nil {
		return nil, errorf(CodeNotFound, "sandbox %s does not exist", id)
	}
	return sb, nil
}

// backendFailure maps a runtime failure. If the container is gone the sandbox
// is evicted rather than answered with a 502 that will never clear.
func (s *Sandboxes) backendFailure(_ context.Context, sb *sandbox, err error) error {
	if errors.Is(err, ErrNotFound) {
		s.killLocked(sb, KillEvicted)
		return errorf(CodeInvalidState, "sandbox %s was evicted", sb.id)
	}
	return backendError(err)
}

// armLifetimeLocked sets expiresAt for the current state. A kill action counts
// from creation and keeps counting while paused; a pause action counts only
// while running, and every transition to running arms a fresh interval from
// now, so an automatic resume does not expire again immediately.
func (sb *sandbox) armLifetimeLocked(now time.Time) {
	if sb.opts.timeoutSeconds == 0 || sb.state == StateKilled || sb.state == StatePending {
		sb.expiresAt = nil
		return
	}
	limit := time.Duration(sb.opts.timeoutSeconds) * time.Second
	base := sb.createdAt
	if sb.opts.timeoutAction == ActionPause {
		if sb.state != StateRunning {
			sb.expiresAt = nil
			return
		}
		base = now
	}
	deadline := base.Add(limit)
	sb.expiresAt = &deadline
}

// deadlined reports whether the lifetime deadline is due at now.
func (sb *sandbox) deadlined(now time.Time) bool {
	return sb.expiresAt != nil && !now.Before(*sb.expiresAt)
}

// idleFor returns how long the sandbox has been idle at now, or a negative
// duration when it is not idle by definition.
func (sb *sandbox) idleFor(now time.Time) time.Duration {
	if sb.state != StateRunning || sb.running != nil || sb.streams > 0 ||
		sb.opts.idleTimeoutSeconds <= 0 {
		return -1
	}
	return now.Sub(sb.lastActivity)
}

func (sb *sandbox) invalidState() error {
	return errorf(CodeInvalidState, "sandbox %s is %s", sb.id, sb.state)
}

// snapshot returns the contract view. Callers hold sb.mu.
func (sb *sandbox) snapshot() Sandbox {
	return Sandbox{
		ID:                 sb.id,
		State:              sb.state,
		Image:              sb.opts.image,
		MemoryMB:           sb.opts.memoryMB,
		TimeoutSeconds:     sb.opts.timeoutSeconds,
		TimeoutAction:      sb.opts.timeoutAction,
		IdleTimeoutSeconds: sb.opts.idleTimeoutSeconds,
		IdleTimeoutAction:  sb.opts.idleTimeoutAction,
		CreatedAt:          sb.createdAt,
		ExpiresAt:          cloneTime(sb.expiresAt),
		KillReason:         cloneReason(sb.killReason),
	}
}
