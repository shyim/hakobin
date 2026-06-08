use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::Context;
use aws_config::BehaviorVersion;
use aws_sdk_cloudfront::types::{InvalidationBatch, Paths};
use reqwest::StatusCode;
use serde_json::json;

use crate::config::Config;

#[derive(Debug, Clone)]
pub enum CdnInvalidator {
    CloudFront { distribution_id: String },
    Cloudflare { zone_id: String, api_token: String },
}

impl CdnInvalidator {
    pub fn from_env() -> anyhow::Result<Option<Self>> {
        let Some(kind) = std::env::var("HAKOBIN_CDN_PURGE_TYPE")
            .ok()
            .filter(|v| !v.trim().is_empty())
        else {
            return Ok(None);
        };

        match kind.to_lowercase().as_str() {
            "cloudfront" => Ok(Some(Self::CloudFront {
                distribution_id: required_env("CLOUDFRONT_DISTRIBUTION_ID")?,
            })),
            "cloudflare" => Ok(Some(Self::Cloudflare {
                zone_id: required_env("CLOUDFLARE_ZONE_ID")?,
                api_token: required_env("CLOUDFLARE_API_TOKEN")?,
            })),
            other => anyhow::bail!("unsupported CDN type: {other}"),
        }
    }

    pub async fn invalidate(&self, cfg: &Config, paths: &[String]) -> anyhow::Result<()> {
        if paths.is_empty() {
            return Ok(());
        }

        match self {
            Self::CloudFront { distribution_id } => {
                invalidate_cloudfront(distribution_id, paths).await
            }
            Self::Cloudflare { zone_id, api_token } => {
                invalidate_cloudflare(cfg, zone_id, api_token, paths).await
            }
        }
    }
}

async fn invalidate_cloudfront(distribution_id: &str, paths: &[String]) -> anyhow::Result<()> {
    let cfg = aws_config::defaults(BehaviorVersion::latest()).load().await;
    let client = aws_sdk_cloudfront::Client::new(&cfg);
    let normalized = paths.iter().map(normalize_path).collect::<Vec<_>>();
    let quantity = normalized.len() as i32;
    let caller_reference = format!(
        "hakobin-{}",
        SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs()
    );

    let batch = InvalidationBatch::builder()
        .caller_reference(caller_reference)
        .paths(
            Paths::builder()
                .quantity(quantity)
                .set_items(Some(normalized.clone()))
                .build()?,
        )
        .build()?;

    let result = client
        .create_invalidation()
        .distribution_id(distribution_id)
        .invalidation_batch(batch)
        .send()
        .await
        .context("failed to create CloudFront invalidation")?;

    if let Some(invalidation) = result.invalidation {
        println!("CloudFront invalidation created: {}", invalidation.id);
    }
    println!("Invalidating {} paths", normalized.len());
    Ok(())
}

async fn invalidate_cloudflare(
    cfg: &Config,
    zone_id: &str,
    api_token: &str,
    paths: &[String],
) -> anyhow::Result<()> {
    let base_url = cfg.repository_base_url()?;
    let urls = paths
        .iter()
        .map(|path| format!("{}{}", base_url.trim_end_matches('/'), normalize_path(path)))
        .collect::<Vec<_>>();

    let client = reqwest::Client::new();
    for batch in urls.chunks(30) {
        let response = client
            .post(format!(
                "https://api.cloudflare.com/client/v4/zones/{zone_id}/purge_cache"
            ))
            .bearer_auth(api_token)
            .json(&json!({ "files": batch }))
            .send()
            .await
            .context("failed to send Cloudflare purge request")?;

        if response.status() != StatusCode::OK {
            let status = response.status();
            let body = response.text().await.unwrap_or_default();
            anyhow::bail!("Cloudflare purge failed with {status}: {body}");
        }
    }

    println!("Cloudflare cache purged for {} URLs", urls.len());
    Ok(())
}

fn normalize_path(path: &String) -> String {
    if path.starts_with('/') {
        path.clone()
    } else {
        format!("/{path}")
    }
}

fn required_env(name: &str) -> anyhow::Result<String> {
    std::env::var(name)
        .ok()
        .filter(|v| !v.trim().is_empty())
        .with_context(|| format!("{name} environment variable is required"))
}
