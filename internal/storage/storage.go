package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"github.com/shyim/hakobin/internal/config"
)

const (
	lockRetryInterval = 2 * time.Second
	lockMaxWait       = 5 * time.Minute
)

type Store interface {
	// UploadBytes uploads body under key. Note: the package parsing/upload path
	// currently reads whole package files into memory before upload (the S3
	// transfer manager still splits the in-memory buffer into multipart parts),
	// so peak memory scales with the largest package. This is acceptable for
	// typical .deb/.rpm sizes; very large artifacts (multi-GiB) will use
	// proportional memory. A future streaming API would remove this ceiling.
	UploadBytes(ctx context.Context, key string, body []byte, contentType string) error
	Download(ctx context.Context, key string) ([]byte, error)
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
	ListKeys(ctx context.Context, prefix string) ([]string, error)
	// AcquireLock creates an exclusive lock object at key that expires after
	// ttlSeconds. It blocks (with backoff) until the lock is acquired, an
	// existing lock expires and can be stolen, or ctx is cancelled. The
	// returned Lock must be released with its Release method. Time is evaluated
	// internally on every retry so a lock that expires mid-wait is observed.
	AcquireLock(ctx context.Context, key, owner string, ttlSeconds int64) (*Lock, error)
	// RenewLock atomically extends a lock only while the same owner still holds it.
	RenewLock(ctx context.Context, lock *Lock, ttlSeconds int64) error
	// ReleaseLock removes a lock previously acquired by this owner. It is a
	// best-effort no-op if the lock was already stolen or released. Implementations
	// must compare and delete atomically so an expired owner cannot delete a
	// successor's lock.
	ReleaseLock(ctx context.Context, lock *Lock) error
}

// Lock represents an acquired exclusive lock on a repository.
type Lock struct {
	Key   string
	Owner string
}

type lockRecord struct {
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expires_at"`
}

type S3Store struct {
	client   *s3.Client
	uploader *transfermanager.Client
	bucket   string
}

func NewS3Store(ctx context.Context, cfg *config.Config) (*S3Store, error) {
	if err := cfg.RequireS3(); err != nil {
		return nil, err
	}

	creds := credentials.NewStaticCredentialsProvider(cfg.S3AccessKeyID, cfg.S3SecretAccessKey, cfg.S3SessionToken)
	awsCfg, err := s3config.LoadDefaultConfig(ctx,
		s3config.WithRegion(cfg.S3Region),
		s3config.WithCredentialsProvider(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load S3 configuration: %w", err)
	}

	bucket, err := cfg.Bucket()
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		o.UsePathStyle = cfg.S3UsePathStyle
	})

	return &S3Store{
		client:   client,
		uploader: transfermanager.New(client),
		bucket:   bucket,
	}, nil
}

func (s *S3Store) UploadBytes(ctx context.Context, key string, body []byte, contentType string) error {
	// Use the transfer manager so large package blobs are uploaded via
	// automatic multipart (lifting the 5 GiB single-PutObject limit) rather
	// than a single PutObject call.
	_, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(cacheControlFor(key, contentType)),
	})
	if err != nil {
		return fmt.Errorf("failed to upload s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// cacheControlFor returns a Cache-Control value appropriate to the object.
// Package paths can be overwritten by --force and RPM packages are rewritten
// in place during signing-key rotation. They may be stored by shared caches,
// but must be revalidated before reuse so an unchanged URL never serves stale
// package bytes. Repository metadata changes in place on every upload and must
// also be revalidated quickly.
func cacheControlFor(key, contentType string) string {
	switch contentType {
	case "application/vnd.debian.binary-package", "application/x-rpm":
		return "public, no-cache"
	}
	// repodata metadata files are named by checksum and are effectively
	// immutable, but repomd.xml / Release / Packages are rewritten in place.
	return "public, max-age=60, must-revalidate"
}

func (s *S3Store) Download(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download s3://%s/%s: %w", s.bucket, key, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body of s3://%s/%s: %w", s.bucket, key, err)
	}
	return data, nil
}

func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check existence of s3://%s/%s: %w", s.bucket, key, err)
	}
	return true, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *S3Store) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var continuationToken *string

	for {
		resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list s3://%s/%s: %w", s.bucket, prefix, err)
		}

		for _, obj := range resp.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}

		if resp.IsTruncated != nil && *resp.IsTruncated {
			continuationToken = resp.NextContinuationToken
		} else {
			break
		}
	}

	return keys, nil
}

func (s *S3Store) AcquireLock(ctx context.Context, key, owner string, ttlSeconds int64) (*Lock, error) {
	deadline := time.Now().Add(lockMaxWait)
	for {
		// Recompute time on every iteration so a lock that expires while we are
		// waiting is observed as expired, and a freshly-created lock always
		// carries a current ExpiresAt.
		nowUnix := time.Now().Unix()
		record := lockRecord{Owner: owner, ExpiresAt: nowUnix + ttlSeconds}
		body, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}

		// Conditional create: only succeeds if no object exists at key.
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(s.bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(body),
			ContentType: aws.String("application/json"),
			IfNoneMatch: aws.String("*"),
		})
		if err == nil {
			return &Lock{Key: key, Owner: owner}, nil
		}
		if !isPreconditionFailed(err) {
			return nil, fmt.Errorf("failed to acquire lock s3://%s/%s: %w", s.bucket, key, err)
		}

		// A lock already exists. If it has expired, steal it with an atomic
		// compare-and-swap: overwrite it conditional on the ETag we just read,
		// so two waiters cannot both believe they stole the same lock. If the
		// ETag no longer matches (another waiter stole it first), we fall
		// through and retry.
		existing, etag, derr := s.downloadWithETag(ctx, key)
		if derr == nil && etag != "" {
			var cur lockRecord
			if json.Unmarshal(existing, &cur) == nil && cur.ExpiresAt <= nowUnix {
				_, putErr := s.client.PutObject(ctx, &s3.PutObjectInput{
					Bucket:      aws.String(s.bucket),
					Key:         aws.String(key),
					Body:        bytes.NewReader(body),
					ContentType: aws.String("application/json"),
					IfMatch:     aws.String(etag),
				})
				if putErr == nil {
					return &Lock{Key: key, Owner: owner}, nil
				}
				if !isPreconditionFailed(putErr) {
					return nil, fmt.Errorf("failed to steal expired lock s3://%s/%s: %w", s.bucket, key, putErr)
				}
				// Lost the race to steal; retry immediately.
				continue
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for repository lock s3://%s/%s (another operation in progress)", s.bucket, key)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
}

// downloadWithETag reads an object along with its current ETag. The ETag is
// used to steal an expired lock atomically (conditional overwrite).
func (s *S3Store) downloadWithETag(ctx context.Context, key string) ([]byte, string, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	etag := ""
	if resp.ETag != nil {
		etag = *resp.ETag
	}
	return data, etag, nil
}

func (s *S3Store) ReleaseLock(ctx context.Context, lock *Lock) error {
	if lock == nil {
		return nil
	}
	// Only delete the lock if we still own it, so we never remove a lock that
	// was stolen from us after our TTL expired.
	existing, etag, err := s.downloadWithETag(ctx, lock.Key)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return err
	}
	var cur lockRecord
	if err := json.Unmarshal(existing, &cur); err != nil {
		return fmt.Errorf("invalid repository lock %s: %w", lock.Key, err)
	}
	if cur.Owner != lock.Owner {
		return nil
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:  aws.String(s.bucket),
		Key:     aws.String(lock.Key),
		IfMatch: aws.String(etag),
	})
	if isPreconditionFailed(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to release lock s3://%s/%s: %w", s.bucket, lock.Key, err)
	}
	return nil
}

func (s *S3Store) RenewLock(ctx context.Context, lock *Lock, ttlSeconds int64) error {
	if lock == nil {
		return fmt.Errorf("cannot renew a nil repository lock")
	}
	existing, etag, err := s.downloadWithETag(ctx, lock.Key)
	if err != nil {
		return err
	}
	var cur lockRecord
	if err := json.Unmarshal(existing, &cur); err != nil {
		return fmt.Errorf("invalid repository lock %s: %w", lock.Key, err)
	}
	if cur.Owner != lock.Owner {
		return fmt.Errorf("repository lock %s is no longer owned by %s", lock.Key, lock.Owner)
	}

	cur.ExpiresAt = time.Now().Unix() + ttlSeconds
	body, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(lock.Key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
		IfMatch:     aws.String(etag),
	})
	if isPreconditionFailed(err) {
		return fmt.Errorf("repository lock %s changed while it was being renewed", lock.Key)
	}
	if err != nil {
		return fmt.Errorf("failed to renew lock s3://%s/%s: %w", s.bucket, lock.Key, err)
	}
	return nil
}

func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "PreconditionFailed" || code == "412" {
			return true
		}
	}
	return strings.Contains(err.Error(), "PreconditionFailed") ||
		strings.Contains(err.Error(), "status code: 412")
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	var nsb *s3types.NoSuchBucket
	var nf *s3types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nsb) || errors.As(err, &nf) {
		return true
	}
	// HeadObject may surface a generic APIError with a 404/NotFound code that
	// is not one of the modeled types above.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NotFound" || code == "NoSuchKey" || code == "NoSuchBucket" || code == "404" {
			return true
		}
	}
	return false
}

// MemoryStore implements Store for testing
type MemoryStore struct {
	mu      sync.Mutex
	objects map[string]*storedObject
}

type storedObject struct {
	body        []byte
	contentType string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		objects: make(map[string]*storedObject),
	}
}

func (m *MemoryStore) UploadBytes(ctx context.Context, key string, body []byte, contentType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &storedObject{
		body:        body,
		contentType: contentType,
	}
	return nil
}

func (m *MemoryStore) Download(ctx context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, exists := m.objects[key]
	if !exists {
		return nil, fmt.Errorf("NoSuchKey: %s", key)
	}
	return obj.body, nil
}

func (m *MemoryStore) Exists(ctx context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.objects[key]
	return exists, nil
}

func (m *MemoryStore) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemoryStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (m *MemoryStore) AcquireLock(ctx context.Context, key, owner string, ttlSeconds int64) (*Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nowUnix := time.Now().Unix()
	if obj, exists := m.objects[key]; exists {
		var cur lockRecord
		if json.Unmarshal(obj.body, &cur) == nil && cur.ExpiresAt > nowUnix {
			return nil, fmt.Errorf("repository lock %s is held by another operation", key)
		}
	}

	record := lockRecord{Owner: owner, ExpiresAt: nowUnix + ttlSeconds}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	m.objects[key] = &storedObject{body: body, contentType: "application/json"}
	return &Lock{Key: key, Owner: owner}, nil
}

func (m *MemoryStore) ReleaseLock(ctx context.Context, lock *Lock) error {
	if lock == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if obj, exists := m.objects[lock.Key]; exists {
		var cur lockRecord
		if json.Unmarshal(obj.body, &cur) == nil && cur.Owner != lock.Owner {
			return nil
		}
	}
	delete(m.objects, lock.Key)
	return nil
}

func (m *MemoryStore) RenewLock(ctx context.Context, lock *Lock, ttlSeconds int64) error {
	if lock == nil {
		return fmt.Errorf("cannot renew a nil repository lock")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, exists := m.objects[lock.Key]
	if !exists {
		return fmt.Errorf("repository lock %s no longer exists", lock.Key)
	}
	var cur lockRecord
	if err := json.Unmarshal(obj.body, &cur); err != nil {
		return err
	}
	if cur.Owner != lock.Owner {
		return fmt.Errorf("repository lock %s is no longer owned by %s", lock.Key, lock.Owner)
	}
	cur.ExpiresAt = time.Now().Unix() + ttlSeconds
	body, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	m.objects[lock.Key] = &storedObject{body: body, contentType: "application/json"}
	return nil
}

// WithLock runs fn while maintaining an expiring repository lock. The operation
// context is cancelled immediately if renewal fails, preventing a writer that
// lost its lease from continuing to publish metadata.
func WithLock(ctx context.Context, store Store, key, owner string, ttlSeconds int64, fn func(context.Context) error) error {
	if ttlSeconds <= 0 {
		return fmt.Errorf("repository lock TTL must be positive")
	}
	lock, err := store.AcquireLock(ctx, key, owner, ttlSeconds)
	if err != nil {
		return err
	}

	opCtx, cancelOperation := context.WithCancel(ctx)
	interval := time.Duration(ttlSeconds) * time.Second / 3
	if interval < time.Second {
		interval = time.Second
	}
	renewErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-opCtx.Done():
				return
			case <-ticker.C:
				if err := store.RenewLock(opCtx, lock, ttlSeconds); err != nil {
					renewErr <- err
					cancelOperation()
					return
				}
			}
		}
	}()

	operationErr := fn(opCtx)
	cancelOperation()
	<-heartbeatDone

	var heartbeatErr error
	select {
	case heartbeatErr = <-renewErr:
	default:
	}

	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), 10*time.Second)
	releaseErr := store.ReleaseLock(releaseCtx, lock)
	cancelRelease()

	if heartbeatErr != nil {
		return fmt.Errorf("repository lock renewal failed: %w", heartbeatErr)
	}
	if operationErr != nil {
		return operationErr
	}
	if releaseErr != nil {
		return fmt.Errorf("failed to release repository lock: %w", releaseErr)
	}
	return nil
}

func (m *MemoryStore) Body(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, exists := m.objects[key]
	if !exists {
		return nil, false
	}
	return obj.body, true
}

func (m *MemoryStore) ContentType(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, exists := m.objects[key]
	if !exists {
		return "", false
	}
	return obj.contentType, true
}

func (m *MemoryStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		keys = append(keys, k)
	}
	return keys
}
