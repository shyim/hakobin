# CDN Integration

Serving your APT or RPM package repositories directly from an S3 bucket is fine for low-traffic development setups, but for production use, you should put a Content Delivery Network (CDN) like **AWS CloudFront** or **Cloudflare** in front of your bucket.

---

## Why Use a CDN?

1.  **Lower Costs:** CDNs charge less for bandwidth egress than S3 buckets. Under Cloudflare, egress is completely free.
2.  **Speed & Latency:** Clients running `apt update` or `dnf install` will download packages from an edge server near them, making installs significantly faster.
3.  **High Availability:** CDNs act as a shield, caching files so that your S3 bucket doesn't get overloaded by parallel client requests.

---

## The Caching Challenge

Because package managers cache metadata to speed up operations, they expect metadata files (like `Release` or `repomd.xml`) to change when packages are uploaded or removed. If your CDN caches these files forever, client machines won't see new package updates.

To solve this, Hakobin supports **automatic cache invalidation** (purging). Whenever Hakobin updates a repository, it calculates exactly which files were modified and instructs your CDN to delete those specific files from its cache immediately.

---

## 1. AWS CloudFront Setup

If you host your repository on AWS S3 and distribute it via AWS CloudFront:

1.  Create a CloudFront distribution pointing to your S3 bucket as the origin.
2.  Ensure CloudFront is configured to redirect HTTP to HTTPS.
3.  Make note of your **CloudFront Distribution ID** (e.g. `E1A2B3C4D5E6F7`).

### Hakobin Configuration
To configure automatic CloudFront purging, export the following environment variables when running Hakobin commands:

```bash
export HAKOBIN_CDN_PURGE_TYPE=cloudfront
export CLOUDFRONT_DISTRIBUTION_ID=E1A2B3C4D5E6F7
```

> [!NOTE]
> Hakobin will automatically reuse the same `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_REGION` credentials used for S3 to authenticate and trigger the CloudFront purge.

---

## 2. Cloudflare Setup

If you host your repository on Cloudflare R2 (or any storage) and proxy it through a Cloudflare domain (e.g., `packages.example.com` with the orange cloud enabled):

1.  Find your **Zone ID** on the Cloudflare Dashboard overview page for your domain.
2.  Create an API Token with the permission: `Zone - Cache Purge - Edit` for your specific zone.

### Hakobin Configuration
Export the following environment variables:

```bash
export HAKOBIN_CDN_PURGE_TYPE=cloudflare
export CLOUDFLARE_ZONE_ID=your-cloudflare-zone-id
export CLOUDFLARE_API_TOKEN=your-cloudflare-api-token
```

---

## How Purging Works Under the Hood

When you perform an upload or remove command, Hakobin tracks every file it uploads or deletes.

For example, when uploading `nginx.deb` to a Debian stable repository, Hakobin writes/updates:
-   `deb/pool/main/n/nginx/nginx_1.2.3_amd64.deb`
-   `deb/dists/stable/main/binary-amd64/Packages`
-   `deb/dists/stable/main/binary-amd64/Packages.gz`
-   `deb/dists/stable/Release`
-   `deb/dists/stable/Release.gpg`
-   `deb/dists/stable/InRelease`

Instead of purging the entire CDN cache (which is slow and wipes out cached `.deb`/`.rpm` package files that haven't changed), Hakobin only requests the CDN to purge the exact paths that were updated (specifically the metadata files). This keeps cached package downloads active on the CDN edge while making new packages available instantly!
