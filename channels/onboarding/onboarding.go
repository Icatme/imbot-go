// Package onboarding defines the user-visible challenge and terminal outcomes
// shared by channel registration flows. Hosts render URL locally as a QR code;
// registration never sends the authorization link to a third-party QR service.
package onboarding

import (
	"errors"
	"time"
)

// Challenge is a short-lived authorization prompt. It contains no private
// polling key, but its URL may include a platform task identifier. ExpiresAt is
// the server expiry or a documented local wait limit when no TTL is provided.
// Treat URL and UserCode as sensitive and display only to the user
// initiating this registration, not to a shared log or analytics service.
type Challenge struct {
	URL          string        `json:"verification_url"`
	UserCode     string        `json:"user_code,omitempty"`
	ExpiresAt    time.Time     `json:"expires_at"`
	PollInterval time.Duration `json:"poll_interval"`
}

var (
	ErrExpired         = errors.New("onboarding: authorization expired")
	ErrDenied          = errors.New("onboarding: authorization denied")
	ErrInvalidResponse = errors.New("onboarding: invalid registration response")
	ErrConcurrentWait  = errors.New("onboarding: registration is already being polled")
)
