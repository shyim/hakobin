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
