package config

import (
	"fmt"
	"os"
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
	pathStyle := env("S3_USE_PATH_STYLE")
	usePathStyle := pathStyle == "true" || pathStyle == "1"

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

	if c.S3Region != "us-east-1" {
		return fmt.Sprintf("https://%s.s3.%s.amazonaws.com", bucket, c.S3Region), nil
	}
	return fmt.Sprintf("https://%s.s3.amazonaws.com", bucket), nil
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
