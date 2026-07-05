package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	S3Endpoint        string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3BucketName      string
	S3Region          string
	S3UsePathStyle    bool
	PublicURL         string
}

func FromEnv() *Config {
	region := env("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	// Accept the usual truthy spellings (true/TRUE/yes/1/on) rather than only
	// the exact strings "true"/"1".
	usePathStyle := parseBool(env("S3_USE_PATH_STYLE"))

	return &Config{
		S3Endpoint:        env("S3_ENDPOINT"),
		S3AccessKeyID:     env("AWS_ACCESS_KEY_ID"),
		S3SecretAccessKey: env("AWS_SECRET_ACCESS_KEY"),
		S3BucketName:      env("S3_BUCKET_NAME"),
		S3Region:          region,
		S3UsePathStyle:    usePathStyle,
		PublicURL:         env("HAKOBIN_PUBLIC_URL"),
	}
}

func (c *Config) RequireS3() error {
	if _, err := c.Bucket(); err != nil {
		return err
	}
	if _, err := c.AccessKey(); err != nil {
		return err
	}
	if _, err := c.SecretKey(); err != nil {
		return err
	}
	if err := c.validatePublicURL(); err != nil {
		return err
	}
	return nil
}

// validatePublicURL rejects a malformed HAKOBIN_PUBLIC_URL early, before it is
// baked into setup.sh source lines or CDN purge URLs.
func (c *Config) validatePublicURL() error {
	if c.PublicURL == "" {
		return nil
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return fmt.Errorf("invalid HAKOBIN_PUBLIC_URL %q: %w", c.PublicURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid HAKOBIN_PUBLIC_URL %q: must be an http or https URL", c.PublicURL)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid HAKOBIN_PUBLIC_URL %q: missing host", c.PublicURL)
	}
	return nil
}

func (c *Config) Bucket() (string, error) {
	b := strings.TrimSpace(c.S3BucketName)
	if b == "" {
		return "", fmt.Errorf("S3_BUCKET_NAME environment variable is required")
	}
	return b, nil
}

func (c *Config) AccessKey() (string, error) {
	k := strings.TrimSpace(c.S3AccessKeyID)
	if k == "" {
		return "", fmt.Errorf("AWS_ACCESS_KEY_ID environment variable is required")
	}
	return k, nil
}

func (c *Config) SecretKey() (string, error) {
	s := strings.TrimSpace(c.S3SecretAccessKey)
	if s == "" {
		return "", fmt.Errorf("AWS_SECRET_ACCESS_KEY environment variable is required")
	}
	return s, nil
}

func (c *Config) RepositoryBaseURL() (string, error) {
	if c.PublicURL != "" {
		return strings.TrimSuffix(c.PublicURL, "/"), nil
	}

	bucket, err := c.Bucket()
	if err != nil {
		return "", err
	}

	if c.S3Endpoint != "" {
		endpoint := strings.TrimSuffix(c.S3Endpoint, "/")
		if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
			return fmt.Sprintf("%s/%s", endpoint, bucket), nil
		}
		return fmt.Sprintf("https://%s/%s", endpoint, bucket), nil
	}

	// Path-style access addresses the bucket as a path under the regional S3
	// host rather than as a virtual host, so the generated URLs must match.
	if c.S3UsePathStyle {
		if c.S3Region != "us-east-1" {
			return fmt.Sprintf("https://s3.%s.amazonaws.com/%s", c.S3Region, bucket), nil
		}
		return fmt.Sprintf("https://s3.amazonaws.com/%s", bucket), nil
	}

	if c.S3Region != "us-east-1" {
		return fmt.Sprintf("https://%s.s3.%s.amazonaws.com", bucket, c.S3Region), nil
	}
	return fmt.Sprintf("https://%s.s3.amazonaws.com", bucket), nil
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// parseBool accepts the common truthy spellings; anything else (including empty)
// is false.
func parseBool(v string) bool {
	b, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(v)))
	if err != nil {
		// ParseBool doesn't accept "yes"/"on"; handle those explicitly.
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "yes", "on":
			return true
		}
		return false
	}
	return b
}
