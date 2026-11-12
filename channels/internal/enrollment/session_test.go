package enrollment

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/wechatbot-go/channels/onboarding"
)

func challenge() onboarding.Challenge {
	return onboarding.Challenge{URL: "https://example.com/authorize", PollInterval: 2 * time.Second, ExpiresAt: time.Now().Add(10 * time.Second)}
}

func TestPollingPacingAndRetainedSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start, calls := time.Now(), 0
		s := NewSession(challenge(), func(context.Context) (string, bool, error) {
			calls++
			if elapsed := time.Since(start); elapsed != time.Duration(calls)*2*time.Second {
				t.Fatalf("poll %d at %v", calls, elapsed)
			}
			return "credentials", calls == 2, nil
		})
		for attempt := range 2 {
			if attempt == 1 {
				// A host can retry saving already accepted credentials even
				// after the original QR has expired, without authorizing again.
				time.Sleep(10 * time.Second)
			}
			if v, err := s.Wait(context.Background()); err != nil || v != "credentials" {
				t.Fatal(v, err)
			}
		}
		if calls != 2 {
			t.Fatalf("successful registration polled again: %d", calls)
		}
	})
}

func TestSlowDownChangesNextPollWithoutExtendingExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start, calls := time.Now(), 0
		var s *Session[string]
		s = NewSession(challenge(), func(context.Context) (string, bool, error) {
			calls++
			if calls == 1 {
				return "", false, s.IncreasePollInterval(5 * time.Second)
			}
			if time.Since(start) != 9*time.Second {
				t.Fatalf("slow_down ignored: %v", time.Since(start))
			}
			return "", false, nil
		})
		if _, err := s.Wait(context.Background()); !errors.Is(err, onboarding.ErrExpired) {
			t.Fatal(err)
		}
		if calls != 2 || time.Since(start) != 10*time.Second {
			t.Fatalf("unexpected poll count or deadline: %d %v", calls, time.Since(start))
		}
	})
}

func TestNoPollAtExpiryBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		s := NewSession(challenge(), func(context.Context) (string, bool, error) { calls++; return "", false, nil })
		if _, err := s.Wait(context.Background()); !errors.Is(err, onboarding.ErrExpired) || calls != 4 {
			t.Fatalf("issued poll at expiry boundary: calls=%d err=%v", calls, err)
		}
	})
}

func TestCancelAndConcurrentWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		s := NewSession(challenge(), func(context.Context) (string, bool, error) { calls++; return "ok", true, nil })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := s.Wait(ctx); done <- err }()
		synctest.Wait()
		if _, err := s.Wait(context.Background()); !errors.Is(err, onboarding.ErrConcurrentWait) {
			t.Fatalf("concurrent polling accepted: %v", err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("cancellation issued a poll: calls=%d err=%v", calls, err)
		}
		if v, err := s.Wait(context.Background()); err != nil || v != "ok" || calls != 1 {
			t.Fatal("explicit resume failed", v, calls, err)
		}
	})
}

func TestExpiryCancelsInFlightPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		s := NewSession(challenge(), func(ctx context.Context) (string, bool, error) {
			calls++
			<-ctx.Done()
			return "partial", false, ctx.Err()
		})
		for range 2 {
			if v, err := s.Wait(context.Background()); !errors.Is(err, onboarding.ErrExpired) || v != "" {
				t.Fatal(v, err)
			}
		}
		if calls != 1 || time.Since(start) != 10*time.Second {
			t.Fatalf("poll not bounded by expiry: %d %v", calls, time.Since(start))
		}
	})
}

func TestCallerDeadlineRemainsCallerError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s := NewSession(challenge(), func(context.Context) (string, bool, error) { t.Fatal("unexpected poll"); return "", false, nil })
		if _, err := s.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
}

func TestNoImplicitRetryAndTerminalOutcomes(t *testing.T) {
	for _, failure := range []error{errors.New("network"), onboarding.ErrDenied, onboarding.ErrInvalidResponse} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				s := NewSession(challenge(), func(context.Context) (string, bool, error) { calls++; return "partial", false, failure })
				if v, err := s.Wait(context.Background()); !errors.Is(err, failure) || v != "" || calls != 1 {
					t.Fatal(v, calls, err)
				}
				_, _ = s.Wait(context.Background())
				want := 1
				if failure.Error() == "network" {
					want = 2
				}
				if calls != want {
					t.Fatalf("poll calls=%d want=%d", calls, want)
				}
			})
		})
	}
}
