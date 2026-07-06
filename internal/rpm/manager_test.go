package rpm

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/rpmpack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/storage"
)

func testManager() (*RpmRepositoryManager, *storage.MemoryStore) {
	store := storage.NewMemoryStore()
	cfg := &config.Config{
		S3BucketName: "b", S3Region: "us-east-1", S3AccessKeyID: "k", S3SecretAccessKey: "s",
		PublicURL: "https://packages.example.test",
	}
	return NewRpmRepositoryManager(cfg, store), store
}

// makeRpm builds a minimal RPM with the given NVRA on disk and returns its path.
func makeRpm(t *testing.T, dir, name, version, release, arch string) string {
	t.Helper()
	pkg, err := rpmpack.NewRPM(rpmpack.RPMMetaData{
		Name: name, Version: version, Release: release, Arch: arch,
		Summary: "Summary of " + name, Description: "Desc", Licence: "MIT",
	})
	require.NoError(t, err)
	pkg.AddFile(rpmpack.RPMFile{Name: "/usr/bin/" + name, Body: []byte("x"), Mode: 0100755, Owner: "root", Group: "root"})

	var buf bytes.Buffer
	require.NoError(t, pkg.Write(&buf))

	path := filepath.Join(dir, name+"-"+version+"-"+release+"."+arch+".rpm")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0644))
	return path
}

func TestInitValidatesRepoArch(t *testing.T) {
	rm, _ := testManager()
	ctx := context.Background()

	err := rm.Init(ctx, &RpmInitRequest{Repo: "../evil", Arch: "x86_64"})
	require.Error(t, err)

	err = rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64/nested"})
	require.Error(t, err)
}

func TestInitWritesRepodata(t *testing.T) {
	rm, store := testManager()
	require.NoError(t, rm.Init(context.Background(), &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))

	_, ok := store.Body("rpm/stable/x86_64/repodata/repomd.xml")
	assert.True(t, ok, "init must write repomd.xml")
}

func TestUploadListRemoveRoundTrip(t *testing.T) {
	rm, store := testManager()
	ctx := context.Background()
	dir := t.TempDir()

	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))

	path := makeRpm(t, dir, "demo", "1.0.0", "1.el9", "x86_64")
	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{
		RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64",
	}))

	_, ok := store.Body("rpm/stable/x86_64/Packages/demo-1.0.0-1.el9.x86_64.rpm")
	assert.True(t, ok, "package blob must be stored")

	pkgs, err := rm.loadPackages(ctx, "stable", "x86_64")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	assert.Equal(t, "demo", pkgs[0].Name)

	// List should not error and find the package.
	require.NoError(t, rm.List(ctx, &RpmListRequest{Repo: "stable", Arch: "x86_64"}))

	// Remove it.
	require.NoError(t, rm.Remove(ctx, &RpmRemoveRequest{
		Package: "demo", Epoch: "0", Version: "1.0.0", Release: "1.el9",
		Arch: "x86_64", Repo: "stable", RepoArch: "x86_64", Force: true,
	}))
	_, ok = store.Body("rpm/stable/x86_64/Packages/demo-1.0.0-1.el9.x86_64.rpm")
	assert.False(t, ok, "package blob must be deleted")
}

func TestUploadSkipsExistingWithoutForce(t *testing.T) {
	rm, _ := testManager()
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))
	path := makeRpm(t, dir, "demo", "1.0.0", "1.el9", "x86_64")

	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64"}))
	// Second upload without force is a no-op (skipped), not an error.
	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64"}))
	// With force it succeeds too.
	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64", Force: true}))
}

func TestUploadRejectsArchMismatch(t *testing.T) {
	rm, _ := testManager()
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))

	// aarch64 package into an x86_64 repo must fail.
	path := makeRpm(t, dir, "demo", "1.0.0", "1.el9", "aarch64")
	err := rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64"})
	require.Error(t, err)
}

func TestUploadAllowsNoarchIntoAnyRepo(t *testing.T) {
	rm, store := testManager()
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))

	path := makeRpm(t, dir, "docs", "2.0.0", "1.el9", "noarch")
	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64"}))

	_, ok := store.Body("rpm/stable/x86_64/Packages/docs-2.0.0-1.el9.noarch.rpm")
	assert.True(t, ok)
}

func TestRemoveNoarchFallbackMatch(t *testing.T) {
	rm, store := testManager()
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))

	path := makeRpm(t, dir, "docs", "2.0.0", "1.el9", "noarch")
	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64"}))

	// User passes the repo arch (x86_64) rather than the package's noarch; the
	// noarch fallback must still find and remove it.
	require.NoError(t, rm.Remove(ctx, &RpmRemoveRequest{
		Package: "docs", Epoch: "0", Version: "2.0.0", Release: "1.el9",
		Arch: "x86_64", Repo: "stable", RepoArch: "x86_64", Force: true,
	}))
	_, ok := store.Body("rpm/stable/x86_64/Packages/docs-2.0.0-1.el9.noarch.rpm")
	assert.False(t, ok, "noarch package must be removed via fallback match")
}

func TestRemoveNotFound(t *testing.T) {
	rm, _ := testManager()
	ctx := context.Background()
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))

	err := rm.Remove(ctx, &RpmRemoveRequest{
		Package: "ghost", Epoch: "0", Version: "9.9.9", Release: "1",
		Arch: "x86_64", Repo: "stable", RepoArch: "x86_64", Force: true,
	})
	require.Error(t, err)
}

func TestRotateKeyReSignsDiscoveredRepos(t *testing.T) {
	rm, store := testManager()
	ctx := context.Background()
	dir := t.TempDir()

	key, err := openpgp.GenerateKeyPair("Hakobin", "hakobin@example.com", "test", 0)
	require.NoError(t, err)
	signed := openpgp.NewSigningKeys(key, nil)

	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64", SigningKeys: signed}))
	path := makeRpm(t, dir, "demo", "1.0.0", "1.el9", "x86_64")
	require.NoError(t, rm.Upload(ctx, &RpmUploadRequest{RpmFiles: []string{path}, Repo: "stable", Arch: "x86_64", SigningKeys: signed}))

	newKey, err := openpgp.GenerateKeyPair("Hakobin New", "hakobin@example.com", "test", 0)
	require.NoError(t, err)
	oldCert, err := key.PublicKeyCert()
	require.NoError(t, err)
	rotated := openpgp.NewSigningKeys(newKey, []*openpgp.PublicKeyCert{oldCert})

	require.NoError(t, rm.RotateKey(ctx, rotated))

	// The published key bundle must contain both keys after rotation.
	bundle, ok := store.Body("rpm/stable/x86_64/" + RpmPublicKeyName)
	require.True(t, ok)
	certs, err := openpgp.ParsePublicKeyCerts(bundle)
	require.NoError(t, err)
	assert.Len(t, certs, 2)
}

func TestRotateKeyRequiresActiveKey(t *testing.T) {
	rm, _ := testManager()
	err := rm.RotateKey(context.Background(), openpgp.NewSigningKeys(nil, nil))
	require.Error(t, err)
}

func TestValidateRepoArch(t *testing.T) {
	valid := [][2]string{{"stable", "x86_64"}, {"testing", "aarch64"}, {"el9", "noarch"}}
	for _, c := range valid {
		assert.NoError(t, validateRepoArch(c[0], c[1]), "%v should be valid", c)
	}
	invalid := [][2]string{{"", "x86_64"}, {"stable", ""}, {"..", "x86_64"}, {"stable", "../x"}, {"a/b", "x86_64"}, {".", "x86_64"}}
	for _, c := range invalid {
		assert.Error(t, validateRepoArch(c[0], c[1]), "%v should be invalid", c)
	}
}

func TestDiscoverRepositories(t *testing.T) {
	rm, _ := testManager()
	ctx := context.Background()
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "stable", Arch: "x86_64"}))
	require.NoError(t, rm.Init(ctx, &RpmInitRequest{Repo: "testing", Arch: "aarch64"}))

	repos, err := rm.discoverRepositories(ctx)
	require.NoError(t, err)
	assert.Len(t, repos, 2)
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "", truncate("anything", 0))
	assert.Equal(t, "", truncate("anything", -1))
	assert.Equal(t, "ab", truncate("abcdef", 2))
	assert.Equal(t, "short", truncate("short", 60))
	assert.Equal(t, "abc...", truncate("abcdefgh", 6))
}
