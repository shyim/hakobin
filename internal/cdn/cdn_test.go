package cdn

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shyim/hakobin/internal/config"
)

func TestFromEnvDisabledWhenUnset(t *testing.T) {
	t.Setenv("HAKOBIN_CDN_PURGE_TYPE", "")
	inv, err := FromEnv()
	require.NoError(t, err)
	assert.Nil(t, inv, "no CDN configured means no invalidator")
}

func TestFromEnvCloudFront(t *testing.T) {
	t.Setenv("HAKOBIN_CDN_PURGE_TYPE", "cloudfront")
	t.Setenv("CLOUDFRONT_DISTRIBUTION_ID", "")
	_, err := FromEnv()
	require.Error(t, err, "cloudfront requires a distribution id")

	t.Setenv("CLOUDFRONT_DISTRIBUTION_ID", "E123")
	inv, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, inv)
	assert.Equal(t, "cloudfront", inv.Type)
	assert.Equal(t, "E123", inv.DistributionID)
}

func TestFromEnvCloudflare(t *testing.T) {
	t.Setenv("HAKOBIN_CDN_PURGE_TYPE", "Cloudflare") // case-insensitive
	t.Setenv("CLOUDFLARE_ZONE_ID", "zone1")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	_, err := FromEnv()
	require.Error(t, err, "cloudflare requires an API token")

	t.Setenv("CLOUDFLARE_API_TOKEN", "tok")
	inv, err := FromEnv()
	require.NoError(t, err)
	require.NotNil(t, inv)
	assert.Equal(t, "cloudflare", inv.Type)
	assert.Equal(t, "zone1", inv.ZoneID)
	assert.Equal(t, "tok", inv.ApiToken)
}

func TestFromEnvUnsupported(t *testing.T) {
	t.Setenv("HAKOBIN_CDN_PURGE_TYPE", "akamai")
	_, err := FromEnv()
	require.Error(t, err)
}

func TestNormalizePath(t *testing.T) {
	assert.Equal(t, "/a/b", normalizePath("a/b"))
	assert.Equal(t, "/a/b", normalizePath("/a/b"))
}

func TestInvalidateNoPathsIsNoop(t *testing.T) {
	inv := &CdnInvalidator{Type: "cloudflare"}
	require.NoError(t, inv.Invalidate(context.Background(), &config.Config{}, nil))
}

// TestCloudflareURLAssembly verifies purge URLs are the public base joined with
// the normalized (leading-slash) path, for both relative and absolute keys.
func TestCloudflareURLAssembly(t *testing.T) {
	cfg := &config.Config{PublicURL: "https://packages.example.com/"}
	base, err := cfg.RepositoryBaseURL()
	require.NoError(t, err)
	base = strings.TrimSuffix(base, "/")

	assert.Equal(t, "https://packages.example.com/deb/pool/x", base+normalizePath("deb/pool/x"))
	assert.Equal(t, "https://packages.example.com/deb/pool/x", base+normalizePath("/deb/pool/x"))
}

func TestInvalidateUnsupportedType(t *testing.T) {
	inv := &CdnInvalidator{Type: "akamai"}
	err := inv.Invalidate(context.Background(), &config.Config{}, []string{"a"})
	require.Error(t, err)
}
