package wire

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

// TokenCache is per adapter/account. Failed refreshes are never cached.
type TokenCache struct {
	mu         sync.Mutex
	value      string
	expires    time.Time
	refreshing chan struct{}
}

func (t *TokenCache) Get(ctx context.Context, fetch func(context.Context) (string, time.Duration, error)) (string, error) {
	for {
		t.mu.Lock()
		if err := ctx.Err(); err != nil {
			t.mu.Unlock()
			return "", err
		}
		if t.value != "" && time.Now().Before(t.expires) {
			value := t.value
			t.mu.Unlock()
			return value, nil
		}
		if pending := t.refreshing; pending != nil {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending:
				continue
			}
		}
		t.refreshing = make(chan struct{})
		t.mu.Unlock()
		break
	}
	// Never hold the state lock across network IO. Other callers wait on the
	// refresh completion or their own cancellation; there is still one fetch.
	v, ttl, err := fetch(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() {
		close(t.refreshing)
		t.refreshing = nil
	}()
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if v == "" || ttl <= 0 {
		return "", errors.New("invalid token response")
	}
	margin := ttl / 10
	if margin > time.Minute {
		margin = time.Minute
	}
	t.value = v
	t.expires = time.Now().Add(ttl - margin)
	return v, nil
}

// Invalidate rejects only the token used by the failed request. A delayed
// authentication error from an older request must not discard a newer token.
func (t *TokenCache) Invalidate(rejectedToken string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rejectedToken == "" || t.value != rejectedToken {
		return
	}
	t.value = ""
	t.expires = time.Time{}
}

type item[T any] struct {
	value   T
	expires time.Time
}

// Cache is bounded and synchronized. Eviction causes an explicit expired-reply
// error at the adapter, never a fallback send to a guessed chat.
type Cache[T any] struct {
	mu    sync.Mutex
	items map[string]item[T]
	limit int
}

func NewCache[T any](limit int) *Cache[T] {
	if limit < 1 {
		limit = 1024
	}
	return &Cache[T]{items: map[string]item[T]{}, limit: limit}
}
func (c *Cache[T]) Put(k string, v T, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, x := range c.items {
		if !now.Before(x.expires) {
			delete(c.items, id)
		}
	}
	if _, ok := c.items[k]; !ok && len(c.items) >= c.limit {
		old := ""
		var when time.Time
		for id, x := range c.items {
			if old == "" || x.expires.Before(when) {
				old = id
				when = x.expires
			}
		}
		delete(c.items, old)
	}
	c.items[k] = item[T]{v, expires}
}
func (c *Cache[T]) Get(k string) (v T, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.items[k]
	if !ok {
		return v, false
	}
	if !time.Now().Before(x.expires) {
		delete(c.items, k)
		return v, false
	}
	return x.value, true
}

// Guard prevents two readers from owning the same account connection.
type Guard struct{ running atomic.Bool }

func (g *Guard) Enter() error {
	if !g.running.CompareAndSwap(false, true) {
		return gateway.ErrAlreadyRunning
	}
	return nil
}
func (g *Guard) Leave() { g.running.Store(false) }

// Retry reconnects transient transport failures. Explicit API/auth/config errors
// are terminal unless marked Retryable. Notifications contain redacted errors.
func Retry(ctx context.Context, onError func(error), session func(context.Context) error) error {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		err := session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return errors.New("channel session ended unexpectedly")
		}
		var api *gateway.APIError
		if errors.As(err, &api) && !api.Retryable {
			return err
		}
		if onError != nil {
			onError(err)
		}
		if time.Since(start) > time.Minute {
			delay = time.Second
		}
		d := delay + time.Duration(rand.Int64N(int64(delay/4+1)))
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
func Wait(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
