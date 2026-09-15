// internal/auth/hasher.go
package auth

import (
	"context"
	"errors"
	"time"
)

// ErrHashQueueTimeout is returned when a caller waits longer than the configured
// queue timeout for a password-hashing slot to free up.
var ErrHashQueueTimeout = errors.New("password hashing queue timed out")

// PasswordHasher bounds concurrent Argon2id/bcrypt operations to protect this
// instance's own CPU/memory from many simultaneous hash requests (e.g. a flood
// of login attempts across many different accounts/IPs, each individually under
// its own rate limit). The bound is intentionally per-instance, not shared via
// Redis — see docs/design/specs/2026-09-15-ratelimit-hardening-design.md §9-§10.
type PasswordHasher struct {
	sem          chan struct{}
	queueTimeout time.Duration
	params       Argon2Params
}

func NewPasswordHasher(maxConcurrent int, queueTimeout time.Duration, params Argon2Params) *PasswordHasher {
	return &PasswordHasher{
		sem:          make(chan struct{}, maxConcurrent),
		queueTimeout: queueTimeout,
		params:       params,
	}
}

func (h *PasswordHasher) acquire(ctx context.Context) error {
	timer := time.NewTimer(h.queueTimeout)
	defer timer.Stop()
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrHashQueueTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *PasswordHasher) release() {
	<-h.sem
}

// Hash produces a new Argon2id hash. Always used for new/changed passwords going forward.
func (h *PasswordHasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return hashArgon2(password, h.params)
}

// Verify checks password against hash, auto-detecting bcrypt vs Argon2id.
func (h *PasswordHasher) Verify(ctx context.Context, hash, password string) (bool, error) {
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	return verifyPassword(hash, password)
}

// NeedsRehash reports whether hash is a legacy (non-Argon2id) hash that should
// be transparently upgraded on next successful login.
func (h *PasswordHasher) NeedsRehash(hash string) bool {
	return !IsArgon2Hash(hash)
}
