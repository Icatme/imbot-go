// Package enrollment implements bounded registration IO and polling. Platform
// packages own protocol parsing; applications own credential storage and UI.
package enrollment

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Icatme/imbot-go/channels/onboarding"
)

// Session retains the polling credential inside its platform-owned closure.
// A caller may resume after cancellation or an IO error by explicitly calling
// Wait again. Successful and terminal outcomes are retained for this session.
type Session[T any] struct {
	challenge onboarding.Challenge
	poll      func(context.Context) (T, bool, error)
	mu        sync.Mutex
	waiting   bool
	interval  time.Duration
	done      bool
	value     T
	err       error
}

func NewSession[T any](challenge onboarding.Challenge, poll func(context.Context) (T, bool, error)) *Session[T] {
	return &Session[T]{challenge: challenge, poll: poll, interval: challenge.PollInterval}
}

func (s *Session[T]) Challenge() onboarding.Challenge { return s.challenge }

// IncreasePollInterval honors a platform's explicit slow_down response. It does
// not extend the challenge's lifetime or schedule another request by itself.
func (s *Session[T]) IncreasePollInterval(delta time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta <= 0 || s.interval <= 0 || delta > time.Duration(1<<63-1)-s.interval {
		return onboarding.ErrInvalidResponse
	}
	s.interval += delta
	return nil
}

func (s *Session[T]) Wait(ctx context.Context) (value T, err error) {
	if err = ctx.Err(); err != nil {
		return value, err
	}
	s.mu.Lock()
	if s.done {
		value, err = s.value, s.err
		s.mu.Unlock()
		return value, err
	}
	if s.waiting {
		s.mu.Unlock()
		return value, onboarding.ErrConcurrentWait
	}
	s.waiting = true
	s.mu.Unlock()
	complete := false
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.waiting = false
		if complete || errors.Is(err, onboarding.ErrDenied) || errors.Is(err, onboarding.ErrExpired) || errors.Is(err, onboarding.ErrInvalidResponse) {
			s.done, s.value, s.err = true, value, err
		}
	}()
	if s.poll == nil || s.challenge.PollInterval <= 0 || s.challenge.ExpiresAt.IsZero() {
		return value, onboarding.ErrInvalidResponse
	}
	if !time.Now().Before(s.challenge.ExpiresAt) {
		return value, onboarding.ErrExpired
	}
	life, cancel := context.WithDeadline(ctx, s.challenge.ExpiresAt)
	defer cancel()
	deadline, _ := life.Deadline()
	cancellation := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
			return context.DeadlineExceeded
		}
		return onboarding.ErrExpired
	}
	for {
		s.mu.Lock()
		interval := s.interval
		s.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-life.Done():
			timer.Stop()
			return value, cancellation()
		case <-timer.C:
		}
		// Deadline timer callbacks and the poll timer can wake together. Check
		// the actual deadline even if context cancellation is not delivered yet.
		if life.Err() != nil || !time.Now().Before(deadline) {
			return value, cancellation()
		}
		var done bool
		value, done, err = s.poll(life)
		if life.Err() != nil || !time.Now().Before(deadline) {
			var zero T
			return zero, cancellation()
		}
		if err != nil {
			var zero T
			return zero, err
		}
		if done {
			complete = true
			return value, nil
		}
		var zero T
		value = zero
	}
}
