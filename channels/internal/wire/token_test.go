package wire

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenWaiterCanCancelDuringAnotherRefresh(t *testing.T) {
	var cache TokenCache
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	fetch := func(context.Context) (string, time.Duration, error) {
		calls.Add(1)
		close(started)
		<-release
		return "fresh-token", time.Hour, nil
	}
	first := make(chan error, 1)
	go func() { _, err := cache.Get(context.Background(), fetch); first <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	go func() { _, err := cache.Get(ctx, fetch); waiter <- err }()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled token waiter remained blocked behind another network request")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("waiter performed an extra refresh: %d", calls.Load())
	}
	if got, err := cache.Get(context.Background(), fetch); err != nil || got != "fresh-token" {
		t.Fatal(got, err)
	}
}
