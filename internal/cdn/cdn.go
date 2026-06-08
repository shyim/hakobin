package cdn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/google/uuid"

	"hakobin/internal/config"
)

type CdnInvalidator struct {
	Type           string // "cloudfront" or "cloudflare"
	DistributionID string // for cloudfront
	ZoneID         string // for cloudflare
	ApiToken       string // for cloudflare
}

func FromEnv() (*CdnInvalidator, error) {
	kind := strings.TrimSpace(os.Getenv("HAKOBIN_CDN_PURGE_TYPE"))
	if kind == "" {
		return nil, nil
	}

	switch strings.ToLower(kind) {
	case "cloudfront":
		distID := strings.TrimSpace(os.Getenv("CLOUDFRONT_DISTRIBUTION_ID"))
		if distID == "" {
			return nil, fmt.Errorf("CLOUDFRONT_DISTRIBUTION_ID environment variable is required")
		}
		return &CdnInvalidator{
			Type:           "cloudfront",
			DistributionID: distID,
		}, nil
	case "cloudflare":
		zoneID := strings.TrimSpace(os.Getenv("CLOUDFLARE_ZONE_ID"))
		if zoneID == "" {
			return nil, fmt.Errorf("CLOUDFLARE_ZONE_ID environment variable is required")
		}
		apiToken := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
		if apiToken == "" {
			return nil, fmt.Errorf("CLOUDFLARE_API_TOKEN environment variable is required")
		}
		return &CdnInvalidator{
			Type:     "cloudflare",
			ZoneID:   zoneID,
			ApiToken: apiToken,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported CDN type: %s", kind)
	}
}

func (c *CdnInvalidator) Invalidate(ctx context.Context, cfg *config.Config, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	switch c.Type {
	case "cloudfront":
		return c.invalidateCloudFront(ctx, cfg, paths)
	case "cloudflare":
		return c.invalidateCloudflare(ctx, cfg, paths)
	default:
		return fmt.Errorf("unsupported CDN type: %s", c.Type)
	}
}

func (c *CdnInvalidator) invalidateCloudFront(ctx context.Context, cfg *config.Config, paths []string) error {
	// Reuse the same S3 credentials/region the rest of the tool is configured
	// with, rather than silently falling back to the default AWS chain (which
	// breaks when S3 credentials are supplied only via env for a non-default
	// profile or a MinIO-style setup).
	opts := []func(*cfconfig.LoadOptions) error{}
	if cfg != nil {
		if cfg.S3Region != "" {
			opts = append(opts, cfconfig.WithRegion(cfg.S3Region))
		}
		if cfg.S3AccessKeyID != "" && cfg.S3SecretAccessKey != "" {
			opts = append(opts, cfconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(cfg.S3AccessKeyID, cfg.S3SecretAccessKey, ""),
			))
		}
	}

	awsCfg, err := cfconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to load CloudFront config: %w", err)
	}

	client := cloudfront.NewFromConfig(awsCfg)

	var items []string
	for _, p := range paths {
		items = append(items, normalizePath(p))
	}

	// A unique caller reference per invalidation: CloudFront treats a repeated
	// caller reference idempotently, so a second-resolution timestamp could make
	// distinct invalidations in the same second collide and be dropped.
	callerRef := fmt.Sprintf("hakobin-%s", uuid.NewString())

	input := &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(c.DistributionID),
		InvalidationBatch: &cftypes.InvalidationBatch{
			CallerReference: aws.String(callerRef),
			Paths: &cftypes.Paths{
				Quantity: aws.Int32(int32(len(items))),
				Items:    items,
			},
		},
	}

	result, err := client.CreateInvalidation(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to create CloudFront invalidation: %w", err)
	}

	if result.Invalidation != nil && result.Invalidation.Id != nil {
		fmt.Printf("CloudFront invalidation created: %s\n", *result.Invalidation.Id)
	}
	fmt.Printf("Invalidating %d paths\n", len(items))
	return nil
}

func (c *CdnInvalidator) invalidateCloudflare(ctx context.Context, cfg *config.Config, paths []string) error {
	baseURL, err := cfg.RepositoryBaseURL()
	if err != nil {
		return err
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	var urls []string
	for _, p := range paths {
		urls = append(urls, fmt.Sprintf("%s%s", baseURL, normalizePath(p)))
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Cloudflare API allows purging in batches of up to 30 files
	for i := 0; i < len(urls); i += 30 {
		end := i + 30
		if end > len(urls) {
			end = len(urls)
		}
		batch := urls[i:end]

		reqBody, err := json.Marshal(map[string]interface{}{
			"files": batch,
		})
		if err != nil {
			return fmt.Errorf("failed to marshal Cloudflare payload: %w", err)
		}

		reqURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", c.ZoneID)
		req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(reqBody))
		if err != nil {
			return fmt.Errorf("failed to create Cloudflare request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+c.ApiToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to send Cloudflare purge request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := ioReadAll(resp.Body)
			return fmt.Errorf("cloudflare purge failed with status %d: %s", resp.StatusCode, string(body))
		}
	}

	fmt.Printf("Cloudflare cache purged for %d URLs\n", len(urls))
	return nil
}

func normalizePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

func ioReadAll(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	return buf.Bytes(), err
}
