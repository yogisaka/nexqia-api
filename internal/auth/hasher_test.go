// internal/auth/hasher_test.go
package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPasswordHasher_HashAndVerifyRoundTrip(t *testing.T) {
	hasher := NewPasswordHasher(4, time.Second, testParams)
	ctx := context.Background()

	hash, err := hasher.Hash(ctx, "s3cret!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ok, err := hasher.Verify(ctx, hash, "s3cret!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected password to verify")
	}
}

func TestPasswordHasher_NeedsRehash(t *testing.T) {
	hasher := NewPasswordHasher(4, time.Second, testParams)
	ctx := context.Background()

	argon2Hash, _ := hasher.Hash(ctx, "x")
	if hasher.NeedsRehash(argon2Hash) {
		t.Error("expected fresh argon2 hash to not need rehash")
	}

	bcryptHash := "$2a$10$abcdefghijklmnopqrstuv" // syntactically bcrypt-shaped, enough for prefix check
	if !hasher.NeedsRehash(bcryptHash) {
		t.Error("expected bcrypt-shaped hash to need rehash")
	}
}

func TestPasswordHasher_BoundsConcurrency(t *testing.T) {
	const maxConcurrent = 2
	hasher := NewPasswordHasher(maxConcurrent, 50*time.Millisecond, testParams)

	var inFlight, maxObserved int
	var mu sync.Mutex
	var wg sync.WaitGroup

	track := func() {
		mu.Lock()
		inFlight++
		if inFlight > maxObserved {
			maxObserved = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hasher.acquire(context.Background()); err != nil {
				return
			}
			defer hasher.release()
			track()
		}()
	}
	wg.Wait()

	if maxObserved > maxConcurrent {
		t.Errorf("expected at most %d concurrent hash operations, observed %d", maxConcurrent, maxObserved)
	}
}

func TestPasswordHasher_TimesOutWhenQueueFull(t *testing.T) {
	hasher := NewPasswordHasher(1, 20*time.Millisecond, testParams)
	ctx := context.Background()

	if err := hasher.acquire(ctx); err != nil {
		t.Fatalf("unexpected error acquiring first slot: %v", err)
	}
	defer hasher.release()

	err := hasher.acquire(ctx)
	if !errors.Is(err, ErrHashQueueTimeout) {
		t.Errorf("expected ErrHashQueueTimeout when queue full past timeout, got %v", err)
	}
}
