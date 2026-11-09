package daemon

import (
	"context"
	"io"
)

// Signal is a signal the daemon sends to a process group. Only the two the
// contract mentions exist: SIGTERM starts a cancellation, SIGKILL finishes it.
type Signal int

// The two signals the daemon sends: SIGTERM starts a cancellation, SIGKILL
// finishes it.
const (
	SignalTerm Signal = iota + 1
	SignalKill
)

func (s Signal) String() string {
	switch s {
	case SignalTerm:
		return "SIGTERM"
	case SignalKill:
		return "SIGKILL"
	default:
		return "signal"
	}
}

// Liveness is the container-level liveness a Backend reports. The daemon asks
// for it when it must distinguish a process exit from the container going
// away, and keeps checking it while a sandbox runs so an external stop is
// noticed.
//
// The answer is about one exact container, never a name prefix: an external
// container that shares a prefix must not be mistaken for ours.
type Liveness int

const (
	// LivenessRunning means the container exists and is running.
	LivenessRunning Liveness = iota
	// LivenessPaused means the container exists and is paused.
	LivenessPaused
	// LivenessStopped means the container exists but is not running, for
	// example because it exited or was killed externally.
	LivenessStopped
	// LivenessMissing means the runtime has no record of the container.
	LivenessMissing
)

// Alive reports whether the container could still run processes.
func (l Liveness) Alive() bool {
	return l == LivenessRunning || l == LivenessPaused
}

// String makes failures readable.
func (l Liveness) String() string {
	switch l {
	case LivenessRunning:
		return "running"
	case LivenessPaused:
		return "paused"
	case LivenessStopped:
		return "stopped"
	case LivenessMissing:
		return "missing"
	default:
		return "unknown"
	}
}

// ContainerSpec describes the container a sandbox needs.
type ContainerSpec struct {
	Image    string
	MemoryMB int
	// PidsLimit bounds tasks in the container. A backend must apply it: the
	// value is the only thing standing between a fork bomb and the host's PID
	// table.
	PidsLimit int
	// NanoCPUs bounds CPU time in units of 1e-9 CPUs. Zero means no limit.
	NanoCPUs int64
	// Network is the container's network mode. Empty means the backend's
	// default, which is a bridge with outbound access.
	Network NetworkMode
	// Name is a stable container name derived from the sandbox ID. A backend
	// may use it as a hint but must key every operation on the returned ID.
	Name string
	// Labels are attached to the container. The server labels its containers
	// with tiny-sandbox.owner=<listen address> so startup cleanup can find
	// leftovers from a previous process.
	Labels map[string]string
}

// Process is one execution's process, started by a Backend.
//
// Wait blocks until the process tree has exited and returns the exit code, or
// -1 when the runtime could not report one. Stdout and Stderr return the
// readers that carry the process output; they reach EOF once Wait returns.
// Signal delivers sig to the whole process group: a cancellation must not
// leave grandchildren running.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (exitCode int, err error)
	Signal(sig Signal) error
}

// Backend is everything the daemon needs from the container runtime. The
// in-memory implementation lives in internal/daemon/membackend and is used
// only by tests; the Docker implementation lives in internal/dockerbackend.
//
// Implementations must be safe for concurrent use. Errors wrap ErrNotFound
// when the addressed object does not exist.
type Backend interface {
	// Create starts a container and returns its ID. It returns only once the
	// container is running.
	Create(ctx context.Context, spec ContainerSpec) (containerID string, err error)
	// Remove force-removes a container and everything still running in it.
	// Removing a container that is already gone reports ErrNotFound.
	Remove(ctx context.Context, containerID string) error
	// Pause freezes every process in the container.
	Pause(ctx context.Context, containerID string) error
	// Unpause resumes a paused container.
	Unpause(ctx context.Context, containerID string) error
	// Exec starts command inside the container, without a shell, and returns
	// its process. The process runs in its own process group.
	Exec(ctx context.Context, containerID string, command []string) (Process, error)
	// Liveness reports whether the container exists and is running.
	Liveness(ctx context.Context, containerID string) (Liveness, error)
	// OOMKillCount returns the container cgroup's cumulative oom_kill counter.
	// It only ever increases, so a delta across one execution attributes an
	// OOM kill to that execution.
	OOMKillCount(ctx context.Context, containerID string) (int64, error)
	// ReadFile returns a file outside any execution, as the server sees it.
	ReadFile(ctx context.Context, containerID, path string) ([]byte, error)
	// WriteFile writes a file, creating parent directories.
	WriteFile(ctx context.Context, containerID, path string, data []byte) error
}
