package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type observingLockStore struct {
	*MemoryStore
	renewed chan struct{}
	once    sync.Once
	err     error
}

func (s *observingLockStore) RenewLock(ctx context.Context, lock *Lock, ttlSeconds int64) error {
	s.once.Do(func() { close(s.renewed) })
	if s.err != nil {
		return s.err
	}
	return s.MemoryStore.RenewLock(ctx, lock, ttlSeconds)
}

func TestCacheControlFor(t *testing.T) {
	// Package URLs can be overwritten, so caches must revalidate them before
	// reuse instead of treating the first response as immutable for a year.
	assert.Equal(t, "public, no-cache",
		cacheControlFor("deb/pool/n/nginx/nginx_1_amd64.deb", "application/vnd.debian.binary-package"))
	assert.Equal(t, "public, no-cache",
		cacheControlFor("rpm/stable/x86_64/Packages/demo.rpm", "application/x-rpm"))
	// Metadata is revalidated quickly.
	assert.Equal(t, "public, max-age=60, must-revalidate",
		cacheControlFor("deb/dists/stable/Release", "text/plain"))
	assert.Equal(t, "public, max-age=60, must-revalidate",
		cacheControlFor("rpm/stable/x86_64/repodata/repomd.xml", "application/xml"))
}

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

	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 300)
	require.NoError(t, err)
	require.NotNil(t, lock)

	// A second acquire while the lock is held (and unexpired) must fail.
	_, err = store.AcquireLock(ctx, "deb/.lock", "owner-b", 300)
	require.Error(t, err)

	// After release it can be acquired again.
	require.NoError(t, store.ReleaseLock(ctx, lock))
	lock2, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 300)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lock2.Owner)
}

func TestMemoryStoreLockStealsAfterExpiry(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	// Acquire with a zero TTL so the lock is already expired.
	_, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 0)
	require.NoError(t, err)

	// A later acquire steals the expired lock.
	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 60)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lock.Owner)
}

func TestMemoryStoreRenewLockExtendsLeaseAndChecksOwner(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 0)
	require.NoError(t, err)

	require.NoError(t, store.RenewLock(ctx, lock, 60))
	_, err = store.AcquireLock(ctx, "deb/.lock", "owner-b", 60)
	require.Error(t, err, "a renewed lock must not be stealable")

	require.Error(t, store.RenewLock(ctx, &Lock{Key: lock.Key, Owner: "owner-b"}, 60))
}

func TestWithLockRenewsAndCancelsOnLostLease(t *testing.T) {
	t.Run("renews", func(t *testing.T) {
		store := &observingLockStore{MemoryStore: NewMemoryStore(), renewed: make(chan struct{})}
		err := WithLock(context.Background(), store, "deb/.lock", "owner-a", 3, func(ctx context.Context) error {
			select {
			case <-store.renewed:
				return nil
			case <-time.After(2 * time.Second):
				return errors.New("lock was not renewed")
			}
		})
		require.NoError(t, err)
		_, exists := store.Body("deb/.lock")
		assert.False(t, exists, "the lock should be released after the operation")
	})

	t.Run("cancels operation when renewal fails", func(t *testing.T) {
		store := &observingLockStore{
			MemoryStore: NewMemoryStore(), renewed: make(chan struct{}), err: errors.New("lease lost"),
		}
		err := WithLock(context.Background(), store, "deb/.lock", "owner-a", 3, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		require.ErrorContains(t, err, "repository lock renewal failed")
	})
}

func TestReleaseLockDoesNotRemoveStolenLock(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	original, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 0)
	require.NoError(t, err)

	// owner-b steals after expiry.
	stolen, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 60)
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
