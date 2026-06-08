use anyhow::Context;
use aws_config::{BehaviorVersion, Region};
use aws_credential_types::Credentials;
use aws_credential_types::provider::SharedCredentialsProvider;
use aws_sdk_s3::Client;
use aws_sdk_s3::primitives::ByteStream;

use crate::config::Config;

#[derive(Clone)]
pub struct S3Store {
    client: Client,
    bucket: String,
}

pub(crate) trait Store: Clone + Send + Sync + 'static {
    async fn upload_bytes(
        &self,
        key: &str,
        body: impl Into<Vec<u8>> + Send,
        content_type: &str,
    ) -> anyhow::Result<()>;

    async fn download(&self, key: &str) -> anyhow::Result<Vec<u8>>;

    async fn exists(&self, key: &str) -> anyhow::Result<bool>;

    async fn delete(&self, key: &str) -> anyhow::Result<()>;

    async fn list_keys(&self, prefix: &str) -> anyhow::Result<Vec<String>>;
}

impl S3Store {
    pub async fn connect(cfg: &Config) -> anyhow::Result<Self> {
        cfg.require_s3()?;

        let credentials = Credentials::new(
            cfg.access_key()?.to_string(),
            cfg.secret_key()?.to_string(),
            None,
            None,
            "hakobin",
        );

        let mut loader = aws_config::defaults(BehaviorVersion::latest())
            .region(Region::new(cfg.s3_region.clone()))
            .credentials_provider(SharedCredentialsProvider::new(credentials));

        if let Some(endpoint) = &cfg.s3_endpoint {
            loader = loader.endpoint_url(endpoint);
        }

        let shared = loader.load().await;
        let conf = aws_sdk_s3::config::Builder::from(&shared)
            .force_path_style(cfg.s3_use_path_style)
            .build();

        Ok(Self {
            client: Client::from_conf(conf),
            bucket: cfg.bucket()?.to_string(),
        })
    }
}

impl Store for S3Store {
    async fn upload_bytes(
        &self,
        key: &str,
        body: impl Into<Vec<u8>> + Send,
        content_type: &str,
    ) -> anyhow::Result<()> {
        self.client
            .put_object()
            .bucket(&self.bucket)
            .key(key)
            .content_type(content_type)
            .body(ByteStream::from(body.into()))
            .send()
            .await
            .with_context(|| format!("failed to upload s3://{}/{}", self.bucket, key))?;
        Ok(())
    }

    async fn download(&self, key: &str) -> anyhow::Result<Vec<u8>> {
        let output = self
            .client
            .get_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
            .with_context(|| format!("failed to download s3://{}/{}", self.bucket, key))?;

        let bytes = output
            .body
            .collect()
            .await
            .with_context(|| format!("failed to read s3://{}/{}", self.bucket, key))?;
        Ok(bytes.into_bytes().to_vec())
    }

    async fn exists(&self, key: &str) -> anyhow::Result<bool> {
        match self
            .client
            .head_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
        {
            Ok(_) => Ok(true),
            Err(err) if is_not_found(&err) => Ok(false),
            Err(err) => Err(err).with_context(|| {
                format!("failed to check existence of s3://{}/{}", self.bucket, key)
            }),
        }
    }

    async fn delete(&self, key: &str) -> anyhow::Result<()> {
        self.client
            .delete_object()
            .bucket(&self.bucket)
            .key(key)
            .send()
            .await
            .with_context(|| format!("failed to delete s3://{}/{}", self.bucket, key))?;
        Ok(())
    }

    async fn list_keys(&self, prefix: &str) -> anyhow::Result<Vec<String>> {
        let mut keys = Vec::new();
        let mut continuation_token = None;

        loop {
            let output = self
                .client
                .list_objects_v2()
                .bucket(&self.bucket)
                .prefix(prefix)
                .set_continuation_token(continuation_token)
                .send()
                .await
                .with_context(|| format!("failed to list s3://{}/{}", self.bucket, prefix))?;

            for object in output.contents() {
                if let Some(key) = object.key() {
                    keys.push(key.to_string());
                }
            }

            if output.is_truncated().unwrap_or(false) {
                continuation_token = output.next_continuation_token().map(ToOwned::to_owned);
            } else {
                break;
            }
        }

        Ok(keys)
    }
}

fn is_not_found<E: std::fmt::Debug + std::fmt::Display>(err: &E) -> bool {
    let msg = format!("{err} {err:?}");
    msg.contains("NotFound")
        || msg.contains("NoSuchKey")
        || msg.contains("NoSuchBucket")
        || msg.contains("status code: 404")
        || msg.contains("Not Found")
}

#[cfg(test)]
pub(crate) mod test_support {
    use std::collections::BTreeMap;
    use std::sync::{Arc, Mutex};

    use super::*;

    #[derive(Debug, Clone)]
    pub(crate) struct MemoryStore {
        objects: Arc<Mutex<BTreeMap<String, StoredObject>>>,
    }

    #[derive(Debug, Clone)]
    struct StoredObject {
        body: Vec<u8>,
        content_type: String,
    }

    impl MemoryStore {
        pub(crate) fn new() -> Self {
            Self {
                objects: Arc::new(Mutex::new(BTreeMap::new())),
            }
        }

        pub(crate) fn body(&self, key: &str) -> Option<Vec<u8>> {
            self.objects
                .lock()
                .expect("memory store lock poisoned")
                .get(key)
                .map(|object| object.body.clone())
        }

        pub(crate) fn content_type(&self, key: &str) -> Option<String> {
            self.objects
                .lock()
                .expect("memory store lock poisoned")
                .get(key)
                .map(|object| object.content_type.clone())
        }

        pub(crate) fn keys(&self) -> Vec<String> {
            self.objects
                .lock()
                .expect("memory store lock poisoned")
                .keys()
                .cloned()
                .collect()
        }
    }

    impl Store for MemoryStore {
        async fn upload_bytes(
            &self,
            key: &str,
            body: impl Into<Vec<u8>> + Send,
            content_type: &str,
        ) -> anyhow::Result<()> {
            self.objects
                .lock()
                .expect("memory store lock poisoned")
                .insert(
                    key.to_string(),
                    StoredObject {
                        body: body.into(),
                        content_type: content_type.to_string(),
                    },
                );
            Ok(())
        }

        async fn download(&self, key: &str) -> anyhow::Result<Vec<u8>> {
            self.objects
                .lock()
                .expect("memory store lock poisoned")
                .get(key)
                .map(|object| object.body.clone())
                .ok_or_else(|| anyhow::anyhow!("NoSuchKey: {key}"))
        }

        async fn exists(&self, key: &str) -> anyhow::Result<bool> {
            Ok(self
                .objects
                .lock()
                .expect("memory store lock poisoned")
                .contains_key(key))
        }

        async fn delete(&self, key: &str) -> anyhow::Result<()> {
            self.objects
                .lock()
                .expect("memory store lock poisoned")
                .remove(key);
            Ok(())
        }

        async fn list_keys(&self, prefix: &str) -> anyhow::Result<Vec<String>> {
            Ok(self
                .objects
                .lock()
                .expect("memory store lock poisoned")
                .keys()
                .filter(|key| key.starts_with(prefix))
                .cloned()
                .collect())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use aws_sdk_s3::Client;
    use aws_sdk_s3::operation::delete_object::DeleteObjectOutput;
    use aws_sdk_s3::operation::get_object::GetObjectOutput;
    use aws_sdk_s3::operation::head_object::HeadObjectOutput;
    use aws_sdk_s3::operation::list_objects_v2::ListObjectsV2Output;
    use aws_sdk_s3::operation::put_object::PutObjectOutput;
    use aws_sdk_s3::primitives::ByteStream;
    use aws_sdk_s3::types::Object;
    use aws_smithy_mocks::{
        MockResponseInterceptor, Rule, RuleMode, create_mock_http_client, mock,
    };

    fn store_with_rules(rules: &[&Rule]) -> S3Store {
        let mut interceptor = MockResponseInterceptor::new().rule_mode(RuleMode::Sequential);
        for rule in rules {
            interceptor = interceptor.with_rule(rule);
        }

        S3Store {
            client: Client::from_conf(
                aws_sdk_s3::config::Config::builder()
                    .with_test_defaults()
                    .region(aws_sdk_s3::config::Region::new("us-east-1"))
                    .http_client(create_mock_http_client())
                    .interceptor(interceptor)
                    .build(),
            ),
            bucket: "test-bucket".to_string(),
        }
    }

    #[tokio::test]
    async fn upload_bytes_sends_expected_put_object_request() {
        let put_rule = mock!(Client::put_object)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket")
                    && request.key() == Some("path/object.txt")
                    && request.content_type() == Some("text/plain")
            })
            .then_output(|| PutObjectOutput::builder().build());
        let store = store_with_rules(&[&put_rule]);

        store
            .upload_bytes("path/object.txt", b"hello".to_vec(), "text/plain")
            .await
            .unwrap();

        assert_eq!(put_rule.num_calls(), 1);
    }

    #[tokio::test]
    async fn download_reads_mocked_object_body() {
        let get_rule = mock!(Client::get_object)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket") && request.key() == Some("path/object.txt")
            })
            .then_output(|| {
                GetObjectOutput::builder()
                    .body(ByteStream::from_static(b"hello"))
                    .build()
            });
        let store = store_with_rules(&[&get_rule]);

        let body = store.download("path/object.txt").await.unwrap();

        assert_eq!(body, b"hello");
        assert_eq!(get_rule.num_calls(), 1);
    }

    #[tokio::test]
    async fn exists_maps_head_success_and_not_found_error() {
        let found_rule = mock!(Client::head_object)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket") && request.key() == Some("present")
            })
            .then_output(|| HeadObjectOutput::builder().build());
        let missing_rule = mock!(Client::head_object)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket") && request.key() == Some("missing")
            })
            .sequence()
            .http_status(404, None)
            .build();
        let store = store_with_rules(&[&found_rule, &missing_rule]);

        assert!(store.exists("present").await.unwrap());
        assert!(!store.exists("missing").await.unwrap());
        assert_eq!(found_rule.num_calls(), 1);
        assert_eq!(missing_rule.num_calls(), 1);
    }

    #[tokio::test]
    async fn delete_sends_expected_delete_object_request() {
        let delete_rule = mock!(Client::delete_object)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket") && request.key() == Some("path/object.txt")
            })
            .then_output(|| DeleteObjectOutput::builder().build());
        let store = store_with_rules(&[&delete_rule]);

        store.delete("path/object.txt").await.unwrap();

        assert_eq!(delete_rule.num_calls(), 1);
    }

    #[tokio::test]
    async fn list_keys_handles_paginated_results() {
        let first_page = mock!(Client::list_objects_v2)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket")
                    && request.prefix() == Some("prefix/")
                    && request.continuation_token().is_none()
            })
            .then_output(|| {
                ListObjectsV2Output::builder()
                    .contents(Object::builder().key("prefix/a.txt").build())
                    .is_truncated(true)
                    .next_continuation_token("next-page")
                    .build()
            });
        let second_page = mock!(Client::list_objects_v2)
            .match_requests(|request| {
                request.bucket() == Some("test-bucket")
                    && request.prefix() == Some("prefix/")
                    && request.continuation_token() == Some("next-page")
            })
            .then_output(|| {
                ListObjectsV2Output::builder()
                    .contents(Object::builder().key("prefix/b.txt").build())
                    .is_truncated(false)
                    .build()
            });
        let store = store_with_rules(&[&first_page, &second_page]);

        let keys = store.list_keys("prefix/").await.unwrap();

        assert_eq!(keys, vec!["prefix/a.txt", "prefix/b.txt"]);
        assert_eq!(first_page.num_calls(), 1);
        assert_eq!(second_page.num_calls(), 1);
    }
}
