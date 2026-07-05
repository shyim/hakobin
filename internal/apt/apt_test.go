package apt

import (
	"testing"

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
