package daemon_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// TestFailedCreateReleasesItsSlotOnce pins the admission accounting. A create
// whose container fails after its pending record was deleted must free one live
// slot, not two: an over-release would let the store admit more sandboxes than
// the bound allows, one lost slot at a time.
func TestFailedCreateReleasesItsSlotOnce(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newHarness(t)
		h.s = daemon.New(h.be, daemon.WithClock(h.clock), daemon.WithMaxSandboxes(1))
		h.be.CreateErr = errBoom
		release := h.be.HoldCreate()

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = h.s.Create(context.Background(), daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		}()

		for len(h.s.List()) == 0 {
			runtime.Gosched()
		}
		if _, err := h.s.Delete(context.Background(), h.s.List()[0].ID); err != nil {
			t.Fatalf("delete pending: %v", err)
		}
		release()
		<-done
		h.be.CreateErr = nil

		// The slot must be free exactly once: a second create succeeds, a third
		// is refused, so the counter is neither stuck nor below zero.
		if _, err := h.s.Create(context.Background(), daemon.CreateRequest{TimeoutSeconds: ptr(0)}); err != nil {
			t.Fatalf("iteration %d: slot was not released: %v", i, err)
		}
		if _, err := h.s.Create(context.Background(), daemon.CreateRequest{TimeoutSeconds: ptr(0)}); err == nil {
			t.Fatalf("iteration %d: bound of 1 admitted a second live sandbox", i)
		}
	}
}
