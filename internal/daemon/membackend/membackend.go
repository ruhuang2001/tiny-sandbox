// Package membackend is an in-memory implementation of daemon.Backend, used
// only by tests. It is deliberately imported by no binary: the daemon tests,
// the HTTP handler tests and the contract tests share it so all three exercise
// the same semantics without Docker.
//
// It models one container per sandbox, a single running process per container,
// a monotonically increasing oom_kill counter, and a file map. Tests drive it
// through methods such as Kill (simulate an external stop), SetOOMKills
// (simulate the kernel OOM killer), and Program (decide what a command does).
package membackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// Program is what a command does when it runs. It writes output and returns an
// exit code; ctx is done when the process is signalled or the container goes
// away. Returning promptly after ctx is done keeps cancellation tests fast.
type Program func(ctx context.Context, args []string) (stdout, stderr string, exitCode int)

// Streamer is a Program that writes while it runs, so a test can observe
// output events before the execution completes. write returns an error once
// the process is gone.
type Streamer func(ctx context.Context, write func(stdout, stderr []byte) error) (exitCode int)

// Backend is the in-memory daemon.Backend.
type Backend struct {
	mu         sync.Mutex
	containers map[string]*container
	nextID     int

	// Programs maps the first argument of a command to its behaviour. A
	// command not in the map uses Streamers, then Default, which exits 0 with
	// no output.
	Programs  map[string]Program
	Streamers map[string]Streamer
	Default   Program
	// CreateErr, if set, is returned by the next Create. Tests use it to
	// exercise the create-failure path.
	CreateErr error
	// ExecErr, if set, is returned by the next Exec.
	ExecErr error

	// createBlock, when non-nil, makes Create wait for it. Tests use it to
	// observe the pending state, which is otherwise only concurrent.
	createBlock chan struct{}
}

type container struct {
	id     string
	spec   daemon.ContainerSpec
	state  daemon.Liveness
	oom    int64
	files  map[string][]byte
	proc   *process
}

type process struct {
	container string
	command   []string
	outW      *io.PipeWriter
	errW      *io.PipeWriter
	outR      *io.PipeReader
	errR      *io.PipeReader

	mu         sync.Mutex
	signals    []daemon.Signal
	ignoreTerm bool
	ignoreKill bool
	cancel     context.CancelFunc
	dead       bool
	exitCode   int
	waitErr    error

	closeOnce sync.Once
	done      chan struct{}
}

var _ daemon.Backend = (*Backend)(nil)

// New returns an empty backend.
func New() *Backend {
	return &Backend{
		containers: make(map[string]*container),
		Programs:   make(map[string]Program),
		Streamers:  make(map[string]Streamer),
		Default: func(context.Context, []string) (string, string, int) {
			return "", "", 0
		},
	}
}

// ---------------------------------------------------------------- containers

// Create records a container. It fails with CreateErr when a test set one.
func (b *Backend) Create(_ context.Context, spec daemon.ContainerSpec) (string, error) {
	b.mu.Lock()
	block := b.createBlock
	b.mu.Unlock()
	if block != nil {
		<-block
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.CreateErr != nil {
		return "", b.CreateErr
	}
	b.nextID++
	c := &container{
		id:    fmt.Sprintf("ctr_%d", b.nextID),
		spec:  spec,
		state: daemon.LivenessRunning,
		files: make(map[string][]byte),
	}
	b.containers[c.id] = c
	return c.id, nil
}

// Remove drops the container and ends its process.
func (b *Backend) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	c := b.containers[id]
	if c != nil {
		c.state = daemon.LivenessMissing
	}
	b.mu.Unlock()
	if c == nil {
		return fmt.Errorf("membackend: container %s: %w", id, daemon.ErrNotFound)
	}
	c.stopProcess()
	return nil
}

// Pause freezes the container.
func (b *Backend) Pause(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return err
	}
	if c.state != daemon.LivenessRunning {
		return fmt.Errorf("membackend: container %s is %s", id, c.state)
	}
	c.state = daemon.LivenessPaused
	return nil
}

// Unpause resumes the container.
func (b *Backend) Unpause(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return err
	}
	if c.state != daemon.LivenessPaused {
		return fmt.Errorf("membackend: container %s is %s", id, c.state)
	}
	c.state = daemon.LivenessRunning
	return nil
}

// Exec starts a program in the container.
func (b *Backend) Exec(ctx context.Context, id string, command []string) (daemon.Process, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ExecErr != nil {
		err := b.ExecErr
		b.ExecErr = nil
		return nil, err
	}
	c, err := b.get(id)
	if err != nil {
		return nil, err
	}
	if c.state != daemon.LivenessRunning {
		return nil, fmt.Errorf("membackend: container %s is %s", id, c.state)
	}
	if c.proc != nil && !c.proc.finished() {
		return nil, errors.New("membackend: container already has a running process")
	}
	if len(command) == 0 {
		return nil, errors.New("membackend: empty command")
	}

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	p := &process{
		container: id,
		command:   append([]string(nil), command...),
		outR:      outR,
		errR:      errR,
		outW:      outW,
		errW:      errW,
		done:      make(chan struct{}),
	}
	c.proc = p

	if streamer, ok := b.Streamers[command[0]]; ok {
		go p.runStreamer(ctx, streamer)
		return p, nil
	}
	program := b.Default
	if fn, ok := b.Programs[command[0]]; ok {
		program = fn
	}
	go p.run(ctx, program, command[1:])
	return p, nil
}

// Liveness reports the container's state.
func (b *Backend) Liveness(_ context.Context, id string) (daemon.Liveness, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil {
		return daemon.LivenessMissing, nil
	}
	return c.state, nil
}

// OOMKillCount returns the container's cumulative OOM counter.
func (b *Backend) OOMKillCount(_ context.Context, id string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil {
		return 0, fmt.Errorf("membackend: container %s: %w", id, daemon.ErrNotFound)
	}
	return c.oom, nil
}

// ReadFile returns a stored file.
func (b *Backend) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return nil, err
	}
	data, ok := c.files[path]
	if !ok {
		return nil, fmt.Errorf("membackend: %s: %w", path, daemon.ErrNotFound)
	}
	return append([]byte(nil), data...), nil
}

// WriteFile stores a file.
func (b *Backend) WriteFile(_ context.Context, id, path string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return err
	}
	c.files[path] = append([]byte(nil), data...)
	return nil
}

func (b *Backend) get(id string) (*container, error) {
	c := b.containers[id]
	if c == nil || c.state == daemon.LivenessMissing {
		return nil, fmt.Errorf("membackend: container %s: %w", id, daemon.ErrNotFound)
	}
	return c, nil
}

// ---------------------------------------------------------------- test controls

// Kill stops a container the way an external `docker kill` would: the
// container is stopped and its process gets SIGKILL-like treatment, exiting
// 137. The daemon must report this as evicted, never oom.
func (b *Backend) Kill(id string) {
	b.mu.Lock()
	c := b.containers[id]
	if c != nil {
		c.state = daemon.LivenessStopped
	}
	b.mu.Unlock()
	if c != nil {
		c.stopProcess()
	}
}

// Disappear removes the container record without stopping anything, modelling
// a container that vanished. The process sees the container gone on our side
// only through the daemon's liveness probe; the process itself ends 137.
func (b *Backend) Disappear(id string) {
	b.mu.Lock()
	c := b.containers[id]
	if c != nil {
		c.state = daemon.LivenessMissing
	}
	b.mu.Unlock()
	if c != nil {
		c.stopProcess()
	}
}

// IgnoreTerm makes the container's process ignore SIGTERM, so a cancel falls
// through to SIGKILL. It models a process that traps the signal.
func (b *Backend) IgnoreTerm(id string) {
	b.setIgnore(id, true, false)
}

// IgnoreSignals makes the container's process survive both signals, so a
// cancel cannot be confirmed. It models a process the runtime cannot stop.
func (b *Backend) IgnoreSignals(id string) {
	b.setIgnore(id, true, true)
}

func (b *Backend) setIgnore(id string, term, kill bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.containers[id]; c != nil && c.proc != nil {
		c.proc.mu.Lock()
		c.proc.ignoreTerm = term
		c.proc.ignoreKill = kill
		c.proc.mu.Unlock()
	}
}

// SetOOMKills sets the container's oom_kill counter, as the kernel would.
func (b *Backend) SetOOMKills(id string, n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.containers[id]; c != nil {
		c.oom = n
	}
}

// Process returns a container's process, for assertions.
func (b *Backend) Process(id string) *Process {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil || c.proc == nil {
		return nil
	}
	return &Process{proc: c.proc}
}

// Process is a read-only view of a container's process.
type Process struct{ proc *process }

// Signals returns the signals the process received, in order.
func (p *Process) Signals() []daemon.Signal { return p.proc.Signals() }

// State returns a container's liveness, for assertions.
func (b *Backend) State(id string) daemon.Liveness {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.containers[id]; c != nil {
		return c.state
	}
	return daemon.LivenessMissing
}

// HoldCreate makes the next Create block until the returned function is
// called. It lets a test observe the pending state.
func (b *Backend) HoldCreate() (release func()) {
	ch := make(chan struct{})
	b.mu.Lock()
	b.createBlock = ch
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.createBlock = nil
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Signals returns the signals a process received, in order.
func (p *process) Signals() []daemon.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]daemon.Signal(nil), p.signals...)
}

// ---------------------------------------------------------------- process

// Stdout returns the process's stdout reader.
func (p *process) Stdout() io.Reader { return p.outR }

// Stderr returns the process's stderr reader.
func (p *process) Stderr() io.Reader { return p.errR }

// Wait blocks until the process has exited.
func (p *process) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode, p.waitErr
}

// Signal records the signal and acts: SIGTERM asks the program to stop by
// cancelling its context, SIGKILL ends it immediately.
func (p *process) Signal(sig daemon.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	if p.dead {
		p.mu.Unlock()
		return nil
	}
	cancel := p.cancel
	ignored := (sig == daemon.SignalTerm && p.ignoreTerm) || (sig == daemon.SignalKill && p.ignoreKill)
	p.mu.Unlock()
	if ignored {
		// A process the runtime cannot stop still binds its context: the
		// runtime removes the container, which is what ends it. Cancelling
		// here models that the program survives the signal, not that it
		// survives the removal.
		return nil
	}

	switch sig {
	case daemon.SignalTerm:
		if cancel != nil {
			cancel()
		}
	case daemon.SignalKill:
		p.finish(137, nil)
	}
	return nil
}

func (p *process) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// runStreamer executes a streaming program, writing each chunk as it arrives.
func (p *process) runStreamer(ctx context.Context, streamer Streamer) {
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()
	defer cancel()

	write := func(stdout, stderr []byte) error {
		if len(stdout) > 0 {
			if _, err := p.outW.Write(stdout); err != nil {
				return err
			}
		}
		if len(stderr) > 0 {
			if _, err := p.errW.Write(stderr); err != nil {
				return err
			}
		}
		return nil
	}
	code := streamer(ctx, write)
	if ctx.Err() != nil {
		code = 137
	}
	p.finish(code, nil)
}

// run executes the program and publishes its result.
func (p *process) run(ctx context.Context, program Program, args []string) {
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()
	defer cancel()

	stdout, stderr, code := program(ctx, args)
	if ctx.Err() != nil {
		code = 137
	}
	_, _ = io.WriteString(p.outW, stdout)
	_, _ = io.WriteString(p.errW, stderr)
	p.finish(code, nil)
}

// finish closes the pipes and the process exactly once.
func (p *process) finish(code int, waitErr error) {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.dead = true
		p.exitCode = code
		p.waitErr = waitErr
		p.mu.Unlock()
		_ = p.outW.Close()
		_ = p.errW.Close()
		close(p.done)
	})
}

// stopProcess ends a container's process, as removing or killing it would.
func (c *container) stopProcess() {
	if c.proc == nil {
		return
	}
	c.proc.finish(137, nil)
}

// ---------------------------------------------------------------- helpers
