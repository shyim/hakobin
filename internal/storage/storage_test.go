package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStoreLockIsExclusive(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	now := int64(1000)
	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 300, now)
	require.NoError(t, err)
	require.NotNil(t, lock)

	// A second acquire while the lock is held (and unexpired) must fail.
	_, err = store.AcquireLock(ctx, "deb/.lock", "owner-b", 300, now+1)
	require.Error(t, err)

	// After release it can be acquired again.
	require.NoError(t, store.ReleaseLock(ctx, lock))
	lock2, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 300, now+2)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lock2.Owner)
}

func TestMemoryStoreLockStealsAfterExpiry(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	// Acquire with a short TTL.
	_, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 60, 1000)
	require.NoError(t, err)

	// A later acquire past the TTL steals the expired lock.
	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 60, 1000+61)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lock.Owner)
}

func TestReleaseLockDoesNotRemoveStolenLock(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	original, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 60, 1000)
	require.NoError(t, err)

	// owner-b steals after expiry.
	stolen, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 60, 1000+61)
	require.NoError(t, err)

	// owner-a releasing must not remove owner-b's lock.
	require.NoError(t, store.ReleaseLock(ctx, original))

	body, ok := store.Body("deb/.lock")
	require.True(t, ok, "owner-b's lock must survive owner-a's release")
	assert.Contains(t, string(body), "owner-b")

	require.NoError(t, store.ReleaseLock(ctx, stolen))
	_, ok = store.Body("deb/.lock")
	assert.False(t, ok)
}
