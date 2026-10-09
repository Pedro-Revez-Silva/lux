package runner

import (
	"context"
	"testing"
	"time"
)

// A report's frame write survives its caller's cancellation (the
// websocket would otherwise close mid-frame) but still ends at the
// caller's deadline.
func TestWithoutCancelKeepDeadline(t *testing.T) {
	t.Run("cancel does not end the write", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		wctx, cancel := withoutCancelKeepDeadline(parent)
		defer cancel()
		cancelParent()
		select {
		case <-wctx.Done():
			t.Fatalf("write ctx ended with its caller: %v", wctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
		if _, ok := wctx.Deadline(); ok {
			t.Fatal("deadline on a write whose caller had none")
		}
	})

	t.Run("deadline still bounds the write", func(t *testing.T) {
		parent, cancelParent := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancelParent()
		want, _ := parent.Deadline()
		wctx, cancel := withoutCancelKeepDeadline(parent)
		defer cancel()
		if d, ok := wctx.Deadline(); !ok || !d.Equal(want) {
			t.Fatalf("write deadline = %v, %v; want %v", d, ok, want)
		}
		select {
		case <-wctx.Done():
			if wctx.Err() != context.DeadlineExceeded {
				t.Fatalf("write ctx err = %v, want DeadlineExceeded", wctx.Err())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("write ctx outlived its caller's deadline")
		}
	})

	t.Run("cancel before deadline does not end the write", func(t *testing.T) {
		parent, cancelParent := context.WithTimeout(context.Background(), time.Hour)
		wctx, cancel := withoutCancelKeepDeadline(parent)
		defer cancel()
		cancelParent()
		if err := wctx.Err(); err != nil {
			t.Fatalf("write ctx ended with its caller: %v", err)
		}
	})
}
