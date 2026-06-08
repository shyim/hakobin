use anyhow::Context;

#[derive(Debug, Clone)]
pub struct Config {
    pub s3_endpoint: Option<String>,
    pub s3_access_key_id: Option<String>,
    pub s3_secret_access_key: Option<String>,
    pub s3_bucket_name: Option<String>,
    pub s3_region: String,
    pub s3_use_path_style: bool,
    pub public_url: Option<String>,
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            s3_endpoint: env("S3_ENDPOINT"),
            s3_access_key_id: env("AWS_ACCESS_KEY_ID"),
            s3_secret_access_key: env("AWS_SECRET_ACCESS_KEY"),
            s3_bucket_name: env("S3_BUCKET_NAME"),
            s3_region: env("AWS_REGION").unwrap_or_else(|| "us-east-1".to_string()),
            s3_use_path_style: matches!(env("S3_USE_PATH_STYLE").as_deref(), Some("true" | "1")),
            public_url: env("HAKOBIN_PUBLIC_URL"),
        }
    }

    pub fn require_s3(&self) -> anyhow::Result<()> {
        self.bucket()?;
        self.access_key()?;
        self.secret_key()?;
        Ok(())
    }

    pub fn bucket(&self) -> anyhow::Result<&str> {
        self.s3_bucket_name
            .as_deref()
            .filter(|v| !v.is_empty())
            .context("S3_BUCKET_NAME environment variable is required")
    }

    pub fn access_key(&self) -> anyhow::Result<&str> {
        self.s3_access_key_id
            .as_deref()
            .filter(|v| !v.is_empty())
            .context("AWS_ACCESS_KEY_ID environment variable is required")
    }

    pub fn secret_key(&self) -> anyhow::Result<&str> {
        self.s3_secret_access_key
            .as_deref()
            .filter(|v| !v.is_empty())
            .context("AWS_SECRET_ACCESS_KEY environment variable is required")
    }

    pub fn repository_base_url(&self) -> anyhow::Result<String> {
        if let Some(public_url) = &self.public_url {
            return Ok(public_url.trim_end_matches('/').to_string());
        }

        let bucket = self.bucket()?;
        if let Some(endpoint) = &self.s3_endpoint {
            let endpoint = endpoint.trim_end_matches('/');
            if endpoint.starts_with("http://") || endpoint.starts_with("https://") {
                return Ok(format!("{endpoint}/{bucket}"));
            }
            return Ok(format!("https://{endpoint}/{bucket}"));
        }

        if self.s3_region != "us-east-1" {
            Ok(format!(
                "https://{bucket}.s3.{}.amazonaws.com",
                self.s3_region
            ))
        } else {
            Ok(format!("https://{bucket}.s3.amazonaws.com"))
        }
    }
}

fn env(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.trim().is_empty())
}
