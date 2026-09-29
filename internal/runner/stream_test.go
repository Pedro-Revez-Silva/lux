package runner

import (
	"context"
	"testing"
	"time"
)

// A tunnel's output waits for credit: a window's worth, then only as luxd
// grants more. Without a window, nothing waits.
func TestStreamCredit(t *testing.T) {
	if newCredit(0) != nil {
		t.Fatal("no window, yet flow control")
	}
	var none *credit
	if err := none.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := newCredit(2)
	ctx := context.Background()
	for range 2 {
		if err := c.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.take(short); err == nil {
		t.Fatal("took past the window")
	}
	done := make(chan error)
	go func() { done <- c.take(ctx) }()
	time.Sleep(20 * time.Millisecond)
	c.grant(1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a grant did not wake the taker")
	}
}
