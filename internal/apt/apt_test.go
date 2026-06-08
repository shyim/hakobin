package apt

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPoolPathUsesLibPrefix(t *testing.T) {
	repo := RepositoryPath{
		Distribution: "stable",
		Component:    "main",
		Architecture: "amd64",
	}
	assert.Equal(t, "pool/main/n/nginx", repo.PoolPath("nginx"))
	assert.Equal(t, "pool/main/libs/libssl", repo.PoolPath("libssl"))
}

func TestParsesAndGeneratesPackages(t *testing.T) {
	content := `Package: pkg
Version: 1.0
Architecture: amd64
Filename: pool/main/p/pkg/pkg_1.0_amd64.deb
Size: 12
MD5sum: d41d8cd98f00b204e9800998ecf8427e
SHA1: da39a3ee5e6b4b0d3255bfef95601890afd80709
SHA256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855

`
	packages, err := ParsePackages(content)
	require.NoError(t, err)
	require.Len(t, packages, 1)
	assert.Equal(t, "pkg", packages[0].Package)

	generated := GeneratePackagesContent(packages)
	assert.Contains(t, generated, "Package: pkg")
}

func TestRepositoryPathHelpers(t *testing.T) {
	repo := RepositoryPath{Distribution: "stable", Component: "main", Architecture: "amd64"}
	assert.Equal(t, "dists/stable/main/binary-amd64/Packages", repo.PackagesPath())
	assert.Equal(t, "dists/stable/main/binary-amd64/Packages.gz", repo.PackagesGzPath())
	assert.Equal(t, "dists/stable/Release", repo.ReleasePath())
	// Short names still slice safely.
	assert.Equal(t, "pool/main/a/a", repo.PoolPath("a"))
}

func TestCalculateHashesMatchesStdlib(t *testing.T) {
	data := []byte("the quick brown fox")
	md5Val, sha1Val, sha256Val := CalculateHashes(data)

	wantMD5 := md5.Sum(data)
	wantSHA1 := sha1.Sum(data)
	wantSHA256 := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(wantMD5[:]), md5Val)
	assert.Equal(t, hex.EncodeToString(wantSHA1[:]), sha1Val)
	assert.Equal(t, hex.EncodeToString(wantSHA256[:]), sha256Val)
}

func TestCompressGzipRoundTrips(t *testing.T) {
	data := []byte(strings.Repeat("hakobin ", 100))
	gz, err := CompressGzip(data)
	require.NoError(t, err)

	r, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, data, out)
}

func TestReleaseFileGenerate(t *testing.T) {
	rf := ReleaseFile{
		Origin:        "Acme",
		Label:         "Acme Repo",
		Distribution:  "stable",
		Components:    []string{"main", "contrib"},
		Architectures: []string{"amd64", "arm64"},
		Date:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Files: []ReleaseFileEntry{
			{Filename: "main/binary-amd64/Packages", Size: 42, MD5: "m", SHA1: "s1", SHA256: "s256"},
		},
	}
	out := string(rf.Generate())
	assert.Contains(t, out, "Origin: Acme")
	assert.Contains(t, out, "Suite: stable")
	assert.Contains(t, out, "Codename: stable")
	assert.Contains(t, out, "Architectures: amd64 arm64")
	assert.Contains(t, out, "Components: main contrib")
	assert.Contains(t, out, "Date: Fri, 02 Jan 2026 03:04:05 UTC")
	assert.Contains(t, out, "MD5Sum:\n")
	assert.Contains(t, out, "SHA256:\n")
	assert.Contains(t, out, "main/binary-amd64/Packages")
}

func TestReleaseFileGenerateUsesDefaults(t *testing.T) {
	rf := ReleaseFile{Distribution: "stable"}
	out := string(rf.Generate())
	assert.Contains(t, out, "Origin: APT S3 Repository")
	assert.Contains(t, out, "Label: APT S3 Repository")
	assert.Contains(t, out, "Description: APT repository hosted on S3")
	// No files → no checksum sections.
	assert.NotContains(t, out, "MD5Sum:")
}
