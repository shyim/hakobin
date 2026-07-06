package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBoolAcceptsCommonSpellings(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "True", "1", "yes", "YES", "on"} {
		assert.True(t, parseBool(v), "expected %q to be true", v)
	}
	for _, v := range []string{"", "false", "0", "no", "off", "nope"} {
		assert.False(t, parseBool(v), "expected %q to be false", v)
	}
}

func TestValidatePublicURL(t *testing.T) {
	valid := []string{"", "https://packages.example.com", "http://localhost:9000/repo"}
	for _, u := range valid {
		c := &Config{PublicURL: u}
		assert.NoError(t, c.validatePublicURL(), "expected %q to be valid", u)
	}

	invalid := []string{"packages.example.com", "ftp://example.com", "://nohost", "https://"}
	for _, u := range invalid {
		c := &Config{PublicURL: u}
		assert.Error(t, c.validatePublicURL(), "expected %q to be invalid", u)
	}
}

func TestFromEnvReadsEnvironment(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("S3_ENDPOINT", "http://localhost:9000")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("S3_BUCKET_NAME", "my-bucket")
	t.Setenv("S3_USE_PATH_STYLE", "yes")
	t.Setenv("HAKOBIN_PUBLIC_URL", "https://packages.example.com")

	c := FromEnv()
	assert.Equal(t, "eu-west-1", c.S3Region)
	assert.Equal(t, "http://localhost:9000", c.S3Endpoint)
	assert.Equal(t, "AKIA", c.S3AccessKeyID)
	assert.Equal(t, "secret", c.S3SecretAccessKey)
	assert.Equal(t, "my-bucket", c.S3BucketName)
	assert.True(t, c.S3UsePathStyle)
	assert.Equal(t, "https://packages.example.com", c.PublicURL)
}

func TestFromEnvDefaultsRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	c := FromEnv()
	assert.Equal(t, "us-east-1", c.S3Region)
	assert.False(t, c.S3UsePathStyle)
}

func TestRequireS3(t *testing.T) {
	base := func() *Config {
		return &Config{S3BucketName: "b", S3AccessKeyID: "k", S3SecretAccessKey: "s"}
	}
	require.NoError(t, base().RequireS3())

	missingBucket := base()
	missingBucket.S3BucketName = ""
	assert.Error(t, missingBucket.RequireS3())

	missingKey := base()
	missingKey.S3AccessKeyID = ""
	assert.Error(t, missingKey.RequireS3())

	missingSecret := base()
	missingSecret.S3SecretAccessKey = ""
	assert.Error(t, missingSecret.RequireS3())

	badURL := base()
	badURL.PublicURL = "not a url"
	assert.Error(t, badURL.RequireS3())
}

func TestAccessAndSecretKeyAccessors(t *testing.T) {
	c := &Config{S3AccessKeyID: " k ", S3SecretAccessKey: " s "}
	k, err := c.AccessKey()
	require.NoError(t, err)
	assert.Equal(t, "k", k)
	s, err := c.SecretKey()
	require.NoError(t, err)
	assert.Equal(t, "s", s)

	empty := &Config{}
	_, err = empty.AccessKey()
	assert.Error(t, err)
	_, err = empty.SecretKey()
	assert.Error(t, err)
	_, err = empty.Bucket()
	assert.Error(t, err)
}

func TestRepositoryBaseURLEndpoint(t *testing.T) {
	c := &Config{S3BucketName: "b", S3Endpoint: "http://localhost:9000"}
	url, err := c.RepositoryBaseURL()
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:9000/b", url)

	// Endpoint without scheme defaults to https.
	c = &Config{S3BucketName: "b", S3Endpoint: "minio.local"}
	url, err = c.RepositoryBaseURL()
	require.NoError(t, err)
	assert.Equal(t, "https://minio.local/b", url)
}

func TestRepositoryBaseURLPathStyleForAWS(t *testing.T) {
	c := &Config{S3BucketName: "my-bucket", S3Region: "eu-central-1", S3UsePathStyle: true}
	url, err := c.RepositoryBaseURL()
	require.NoError(t, err)
	assert.Equal(t, "https://s3.eu-central-1.amazonaws.com/my-bucket", url)

	c = &Config{S3BucketName: "my-bucket", S3Region: "us-east-1", S3UsePathStyle: false}
	url, err = c.RepositoryBaseURL()
	require.NoError(t, err)
	assert.Equal(t, "https://my-bucket.s3.amazonaws.com", url)

	// PublicURL always wins.
	c = &Config{S3BucketName: "my-bucket", PublicURL: "https://cdn.example.com/", S3UsePathStyle: true}
	url, err = c.RepositoryBaseURL()
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example.com", url)
}
