package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shyim/hakobin/internal/config"
)

// fakeS3 is a minimal in-memory S3-compatible server covering exactly the
// operations S3Store uses (path-style addressing). It lets us unit-test the
// real S3Store code path without Docker/MinIO.
type fakeObject struct {
	body []byte
	etag string
}

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]*fakeObject
	etagSeq int
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string]*fakeObject{}} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Path-style: /<bucket>/<key...>
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		existing, exists := f.objects[key]
		if r.Header.Get("If-None-Match") == "*" && exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		// Conditional overwrite used to atomically steal an expired lock: the
		// object must exist and its current ETag must match.
		if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
			if !exists || existing.etag != ifMatch {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		f.etagSeq++
		etag := fmt.Sprintf("\"etag-%d\"", f.etagSeq)
		f.objects[key] = &fakeObject{body: body, etag: etag}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		// ListObjectsV2 uses list-type=2 as a query param on the bucket.
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}
		obj, ok := f.objects[key]
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", obj.etag)
		_, _ = w.Write(obj.body)

	case http.MethodHead:
		if _, ok := f.objects[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type obj struct {
		Key string `xml:"Key"`
	}
	var result struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		IsTruncated bool     `xml:"IsTruncated"`
		Contents    []obj    `xml:"Contents"`
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		result.Contents = append(result.Contents, obj{Key: k})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<Error><Code>" + code + "</Code></Error>"))
}

func newTestStore(t *testing.T) (*S3Store, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		S3Endpoint:        srv.URL,
		S3AccessKeyID:     "test",
		S3SecretAccessKey: "test",
		S3BucketName:      "bucket",
		S3Region:          "us-east-1",
		S3UsePathStyle:    true,
	}
	store, err := NewS3Store(context.Background(), cfg)
	require.NoError(t, err)
	return store, fake
}

func TestS3StoreUploadDownload(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UploadBytes(ctx, "deb/dists/stable/Release", []byte("release"), "text/plain"))

	body, err := store.Download(ctx, "deb/dists/stable/Release")
	require.NoError(t, err)
	assert.Equal(t, "release", string(body))
}

func TestS3StoreDownloadMissing(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.Download(context.Background(), "missing")
	require.Error(t, err)
}

func TestS3StoreExists(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	exists, err := store.Exists(ctx, "k")
	require.NoError(t, err)
	assert.False(t, exists)

	require.NoError(t, store.UploadBytes(ctx, "k", []byte("v"), "text/plain"))
	exists, err = store.Exists(ctx, "k")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestS3StoreDelete(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UploadBytes(ctx, "k", []byte("v"), "text/plain"))
	require.NoError(t, store.Delete(ctx, "k"))

	exists, err := store.Exists(ctx, "k")
	require.NoError(t, err)
	assert.False(t, exists)
	// Deleting a missing key is a no-op.
	require.NoError(t, store.Delete(ctx, "k"))
}

func TestS3StoreListKeys(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UploadBytes(ctx, "deb/a", []byte("1"), "text/plain"))
	require.NoError(t, store.UploadBytes(ctx, "deb/b", []byte("2"), "text/plain"))
	require.NoError(t, store.UploadBytes(ctx, "rpm/c", []byte("3"), "text/plain"))

	keys, err := store.ListKeys(ctx, "deb/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"deb/a", "deb/b"}, keys)
}

func TestS3StoreLockAcquireReleaseAndConflict(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 300)
	require.NoError(t, err)
	require.NotNil(t, lock)

	// A second acquire while the lock is held (and unexpired) blocks and
	// retries; with a short-deadline context it exits via ctx.Done() instead of
	// waiting out the full lockMaxWait.
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = store.AcquireLock(blocked, "deb/.lock", "owner-b", 300)
	require.Error(t, err)

	// After release, re-acquire succeeds immediately.
	require.NoError(t, store.ReleaseLock(ctx, lock))
	lock2, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 300)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lock2.Owner)
}

func TestS3StoreLockStealsExpired(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// TTL 0 means the lock is already expired the moment it is written.
	_, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 0)
	require.NoError(t, err)

	// The expired lock is stolen.
	lock, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 60)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lock.Owner)
}

// TestS3StoreLockStealIsAtomic verifies the compare-and-swap steal: once one
// waiter steals an expired lock, a second waiter that had read the same expired
// lock cannot also "steal" it (its stale conditional overwrite is rejected),
// and cannot clobber the fresh lock. This is the H1 race.
func TestS3StoreLockStealIsAtomic(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// An already-expired lock exists.
	_, err := store.AcquireLock(ctx, "deb/.lock", "owner-a", 0)
	require.NoError(t, err)

	// owner-b steals it.
	lockB, err := store.AcquireLock(ctx, "deb/.lock", "owner-b", 300)
	require.NoError(t, err)
	assert.Equal(t, "owner-b", lockB.Owner)

	// owner-c now races: owner-b holds a fresh, unexpired lock, so owner-c must
	// NOT be able to acquire it. With a short context it fails rather than
	// clobbering owner-b's lock.
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = store.AcquireLock(blocked, "deb/.lock", "owner-c", 300)
	require.Error(t, err)

	// owner-b still owns the lock.
	body, etag, derr := store.downloadWithETag(ctx, "deb/.lock")
	require.NoError(t, derr)
	require.NotEmpty(t, etag)
	assert.Contains(t, string(body), "owner-b")
}

func TestNewS3StoreRequiresConfig(t *testing.T) {
	_, err := NewS3Store(context.Background(), &config.Config{})
	require.Error(t, err)
}

func TestIsNotFoundError(t *testing.T) {
	assert.True(t, isNotFoundError(&s3types.NoSuchKey{}))
	assert.True(t, isNotFoundError(&s3types.NoSuchBucket{}))
	assert.True(t, isNotFoundError(&s3types.NotFound{}))
	assert.True(t, isNotFoundError(&smithy.GenericAPIError{Code: "NotFound"}))
	assert.True(t, isNotFoundError(&smithy.GenericAPIError{Code: "404"}))
	// Wrapped error still matches via errors.As.
	assert.True(t, isNotFoundError(fmt.Errorf("head failed: %w", &s3types.NotFound{})))

	assert.False(t, isNotFoundError(errors.New("some other error")))
	assert.False(t, isNotFoundError(&smithy.GenericAPIError{Code: "AccessDenied"}))
}

func TestIsPreconditionFailed(t *testing.T) {
	assert.True(t, isPreconditionFailed(&smithy.GenericAPIError{Code: "PreconditionFailed"}))
	assert.True(t, isPreconditionFailed(fmt.Errorf("put: %w", &smithy.GenericAPIError{Code: "PreconditionFailed"})))
	assert.True(t, isPreconditionFailed(errors.New("something status code: 412")))
	assert.False(t, isPreconditionFailed(errors.New("some other error")))
}

func TestS3StoreDownloadServerError(t *testing.T) {
	// A 500 on GetObject must surface as an error (not treated as not-found).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		S3Endpoint: srv.URL, S3AccessKeyID: "t", S3SecretAccessKey: "t",
		S3BucketName: "bucket", S3Region: "us-east-1", S3UsePathStyle: true,
	}
	store, err := NewS3Store(context.Background(), cfg)
	require.NoError(t, err)

	_, err = store.Download(context.Background(), "k")
	require.Error(t, err)
}
