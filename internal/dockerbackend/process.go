package dockerbackend

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// process is one execution's process: a Docker exec inside the sandbox
// container.
type process struct {
	b           *Backend
	containerID string
	execID      string
	pidPath     string

	stdout *bufferedStream
	stderr *bufferedStream
	done   chan struct{} // closed when the output stream has ended

	mu       sync.Mutex
	demuxErr error
}

var _ daemon.Process = (*process)(nil)

// Stdout returns the process's stdout reader. It reaches EOF once the streams
// end, which Wait waits for.
func (p *process) Stdout() io.Reader { return p.stdout }

// Stderr returns the process's stderr reader.
func (p *process) Stderr() io.Reader { return p.stderr }

// Wait blocks until the output stream ends, then reads the exec's exit code,
// or -1 when the runtime reports none.
//
// The output stream ends when the process tree closes its stdout and stderr,
// which is what the engine pumps before it needs the exit status. The inspect
// poll only covers the moment between the stream closing and the daemon
// recording the exit code.
func (p *process) Wait() (int, error) {
	<-p.done

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inspect, err := p.b.waitInspect(ctx, p.execID)
	if err != nil {
		return -1, err
	}
	if inspect.ExitCode == nil {
		return -1, nil
	}
	return *inspect.ExitCode, nil
}

// Signal terminates the whole process group, including grandchildren. The
// exec's PID exists only inside the container, so the PID file written by the
// wrapper is read through the archive API and the group is killed from inside.
//
// A missing PID file means the process has not started yet or has already
// been reaped; the first is retried briefly, the second is a success.
func (p *process) Signal(sig daemon.Signal) error {
	name := "TERM"
	if sig == daemon.SignalKill {
		name = "KILL"
	}
	pid, err := p.pid(3 * time.Second)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// busybox kill does not accept "--", so the PID is passed as a separate
	// validated operand and the signal as the option.
	if _, err := p.b.capture(ctx, p.containerID, []string{"/bin/sh", "-c", "kill -" + name + " -" + pid}); err != nil {
		// The group may already be gone, which is the state this call wants.
		return err
	}
	return nil
}

// pid reads the wrapper's PID file, retrying while the exec is still starting.
func (p *process) pid(limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		data, err := p.b.ReadFile(context.Background(), p.containerID, p.pidPath)
		if err == nil {
			return parsePid(data)
		}
		if !isNotFound(err) || time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isNotFound(err error) bool { return errors.Is(err, daemon.ErrNotFound) }
