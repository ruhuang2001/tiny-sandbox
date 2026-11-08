package dockerbackend

import (
	"bytes"
	"io"
	"sync"
)

// bufferCap bounds one stream's outstanding output. It is well above the
// daemon's per-event chunk because the daemon's pump drains this buffer as it
// fills; the cap only exists so a consumer that never reads cannot make the
// demultiplexer grow without bound.
const bufferCap = 1 << 20

// bufferedStream is the reader a process hands the daemon. Docker's exec
// stream arrives multiplexed on one connection, so the demultiplexer writes to
// both streams from one goroutine. An io.Pipe is unbuffered: writing stdout
// would block until the daemon's stdout pump ran, and a caller reading only
// stderr would deadlock both. Buffering decouples the two readers.
type bufferedStream struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newBufferedStream() *bufferedStream {
	b := &bufferedStream{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends data, blocking while the buffer is full. It never fails; the
// demultiplexer reports the stream's errors, not the buffer's.
func (b *bufferedStream) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for len(p) > 0 {
		for b.closed {
			// The consumer closed early; drop the rest rather than blocking
			// the demultiplexer forever.
			return total, io.ErrClosedPipe
		}
		room := bufferCap - b.buf.Len()
		if room <= 0 {
			b.cond.Wait()
			continue
		}
		n := min(room, len(p))
		b.buf.Write(p[:n])
		total += n
		p = p[n:]
		b.cond.Broadcast()
	}
	return total, nil
}

// Read returns buffered bytes, blocking until data arrives or the stream
// closes. It returns io.EOF only after Close and after the buffer drains.
func (b *bufferedStream) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf.Bytes())
	b.buf.Next(n)
	b.cond.Broadcast() // wake a writer waiting for room
	return n, nil
}

// Close marks the stream finished. It does not discard buffered bytes: a
// consumer that reads after the process exits still gets the output.
func (b *bufferedStream) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
	return nil
}

var (
	_ io.Writer = (*bufferedStream)(nil)
	_ io.Reader = (*bufferedStream)(nil)
)
