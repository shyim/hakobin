package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStoreCRUD(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	// Missing key: Exists false, Download errors.
	exists, err := store.Exists(ctx, "a/b")
	require.NoError(t, err)
	assert.False(t, exists)
	_, err = store.Download(ctx, "a/b")
	require.Error(t, err)

	// Upload then read back.
	require.NoError(t, store.UploadBytes(ctx, "a/b", []byte("hello"), "text/plain"))
	exists, err = store.Exists(ctx, "a/b")
	require.NoError(t, err)
	assert.True(t, exists)

	body, err := store.Download(ctx, "a/b")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))

	ct, ok := store.ContentType("a/b")
	require.True(t, ok)
	assert.Equal(t, "text/plain", ct)
	_, ok = store.ContentType("missing")
	assert.False(t, ok)

	// Delete then confirm gone; deleting a missing key is a no-op.
	require.NoError(t, store.Delete(ctx, "a/b"))
	exists, err = store.Exists(ctx, "a/b")
	require.NoError(t, err)
	assert.False(t, exists)
	require.NoError(t, store.Delete(ctx, "a/b"))
}

func TestMemoryStoreListKeysByPrefix(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	require.NoError(t, store.UploadBytes(ctx, "deb/pool/x", []byte("1"), "text/plain"))
	require.NoError(t, store.UploadBytes(ctx, "deb/dists/y", []byte("2"), "text/plain"))
	require.NoError(t, store.UploadBytes(ctx, "rpm/z", []byte("3"), "text/plain"))

	keys, err := store.ListKeys(ctx, "deb/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"deb/pool/x", "deb/dists/y"}, keys)

	keys, err = store.ListKeys(ctx, "rpm/")
	require.NoError(t, err)
	assert.Equal(t, []string{"rpm/z"}, keys)

	assert.Len(t, store.Keys(), 3)
}

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
