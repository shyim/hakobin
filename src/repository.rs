use std::io::{self, Write};
use std::path::{Path, PathBuf};

use anyhow::{Context, bail};
use chrono::Utc;
use comfy_table::{Table, presets::UTF8_FULL};
use inquire::{Select, Text};
use serde::{Deserialize, Serialize};

use crate::apt::{
    ReleaseFile, ReleaseFileEntry, RepositoryPath, calculate_hashes, compress_gzip,
    generate_packages_content, parse_packages,
};
use crate::cdn::CdnInvalidator;
use crate::cli::DebInitArgs;
use crate::config::Config;
use crate::deb::DebPackage;
use crate::openpgp::{KeyPair, SigningKeys};
use crate::storage::{S3Store, Store};

const DEB_PREFIX: &str = "deb";
const METADATA_PATH: &str = "apt-repo.json";

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RepoMetadata {
    pub origin: String,
    pub label: String,
    pub description: String,
    pub distributions: Vec<String>,
    pub components: Vec<String>,
    pub architectures: Vec<String>,
}

#[derive(Clone)]
pub struct RepositoryManager<S = S3Store> {
    cfg: Config,
    store: S,
}

pub struct InitRequest {
    pub metadata: RepoMetadata,
    pub key_name: String,
    pub key_email: String,
    pub key_expiration_years: u32,
}

pub struct UploadRequest {
    pub deb_files: Vec<PathBuf>,
    pub distribution: String,
    pub component: String,
    pub force: bool,
    pub signing_keys: SigningKeys,
}

pub struct RemoveRequest {
    pub package: String,
    pub version: String,
    pub architecture: String,
    pub distribution: String,
    pub component: String,
    pub force: bool,
    pub signing_keys: SigningKeys,
}

impl InitRequest {
    pub fn from_args(args: DebInitArgs, cfg: &Config) -> anyhow::Result<Self> {
        cfg.require_s3()?;
        let origin = value_or_prompt(args.origin, "Repository Origin", "My Organization")?;
        let label = value_or_prompt(args.label, "Repository Label", "My APT Repository")?;
        let description = value_or_prompt(
            args.description,
            "Repository Description",
            "APT repository for my packages",
        )?;
        let distributions = list_or_prompt(args.distributions, "Distributions", "stable")?;
        let components = list_or_prompt(args.components, "Components", "main")?;
        let architectures = list_or_prompt(args.architectures, "Architectures", "amd64,all")?;
        let key_name = value_or_prompt(args.key_name, "Key Name", "Repository Signing Key")?;
        let key_email = value_or_prompt(args.key_email, "Key Email", "admin@example.com")?;

        Ok(Self {
            metadata: RepoMetadata {
                origin,
                label,
                description,
                distributions,
                components,
                architectures,
            },
            key_name,
            key_email,
            key_expiration_years: args.key_expiration_years,
        })
    }
}

impl RepositoryManager<S3Store> {
    pub async fn connect(cfg: Config) -> anyhow::Result<Self> {
        let store = S3Store::connect(&cfg).await?;
        Ok(Self { cfg, store })
    }
}

#[allow(private_bounds)]
impl<S> RepositoryManager<S>
where
    S: Store,
{
    #[cfg(test)]
    pub(crate) fn with_store(cfg: Config, store: S) -> Self {
        Self { cfg, store }
    }

    pub async fn init(&self, request: InitRequest) -> anyhow::Result<()> {
        if self.store.exists(&self.key(METADATA_PATH)).await? {
            println!("Repository already initialized. Use --force to reinitialize.");
            return Ok(());
        }

        println!(
            "Initializing APT repository in S3 bucket: {}",
            self.cfg.bucket()?
        );
        let key_pair = KeyPair::generate(
            &request.key_name,
            &request.key_email,
            "APT Repository Signing Key",
            request.key_expiration_years,
        )?;

        let key_path = std::env::current_dir()?.join("signing-key.gpg");
        key_pair.save_private_key(&key_path)?;
        println!("GPG key generated successfully!");
        println!("   Key ID: {}", key_pair.key_id);
        if let Some(expiration) = key_pair.expiration {
            println!("   Expires: {}", expiration.format("%Y-%m-%d"));
        } else {
            println!("   Expires: Never");
        }
        println!("   Private key saved to: {}", key_path.display());

        self.save_metadata(&request.metadata).await?;
        let signing_keys = SigningKeys::from_active(key_pair.clone());
        self.create_initial_indexes(&request.metadata, &signing_keys)
            .await?;
        self.upload_public_keys(&signing_keys).await?;
        self.create_setup_script(&request.metadata, &key_pair)
            .await?;

        println!("\nRepository initialized successfully!");
        println!("\nSummary:");
        println!(
            "   Repository configuration: s3://{}/{}",
            self.cfg.bucket()?,
            self.key(METADATA_PATH)
        );
        println!("   Public keys uploaded:");
        println!(
            "     - Binary format: s3://{}/{} (for setup script)",
            self.cfg.bucket()?,
            self.key("pubkey.gpg")
        );
        println!(
            "     - Armored format: s3://{}/{} (for manual import)",
            self.cfg.bucket()?,
            self.key("pubkey.asc")
        );
        println!(
            "   Setup script: s3://{}/{}",
            self.cfg.bucket()?,
            self.key("setup.sh")
        );

        println!("\nQuick Start:");
        println!("   Users can setup your repository:");
        println!(
            "     curl -fsSL {}/setup.sh | sudo bash",
            self.deb_base_url()?
        );
        println!("\nUpload packages:");
        println!(
            "     hakobin deb upload <package.deb> --distribution {} --component {} --architecture {}",
            request.metadata.distributions[0],
            request.metadata.components[0],
            request.metadata.architectures[0]
        );
        println!("\nAdvanced Usage:");
        println!("   Custom key location:");
        println!(
            "     hakobin deb --signing-key {} upload <package.deb>",
            key_path.display()
        );
        println!("\n   CI/CD with environment variable:");
        println!(
            "     export GPG_PRIVATE_KEY=\"$(cat {})\"",
            key_path.display()
        );
        println!("     hakobin deb upload <package.deb>");
        Ok(())
    }

    pub async fn upload(&self, request: UploadRequest) -> anyhow::Result<()> {
        for file in &request.deb_files {
            if !file.exists() {
                bail!("file not found: {}", file.display());
            }
        }

        let mut metadata = self.load_metadata().await.ok();
        let mut uploaded = 0usize;
        let mut skipped = 0usize;
        let mut failed = 0usize;
        let mut first_error: Option<anyhow::Error> = None;

        println!(
            "Uploading {} package(s) to S3 bucket: {}",
            request.deb_files.len(),
            self.cfg.bucket()?
        );
        println!("Distribution: {}", request.distribution);
        println!("Component: {}", request.component);

        for (index, file) in request.deb_files.iter().enumerate() {
            println!(
                "\n[{}/{}] Processing: {}",
                index + 1,
                request.deb_files.len(),
                file.display()
            );
            match self
                .upload_one(file, &request, metadata.as_mut())
                .await
                .with_context(|| format!("failed to upload {}", file.display()))
            {
                Ok(UploadOutcome::Uploaded) => {
                    uploaded += 1;
                    println!("Uploaded successfully");
                }
                Ok(UploadOutcome::Skipped) => {
                    skipped += 1;
                    println!("Skipped (already exists)");
                }
                Err(err) => {
                    failed += 1;
                    println!("Failed: {err:#}");
                    if first_error.is_none() {
                        first_error = Some(err);
                    }
                }
            }
        }

        println!("\n==================================================");
        println!("Upload Summary:");
        println!("Uploaded: {uploaded}");
        if skipped > 0 {
            println!("Skipped: {skipped}");
        }
        if failed > 0 {
            println!("Failed: {failed}");
            if failed > 0 && request.deb_files.len() > 1 {
                if let Some(err) = first_error {
                    println!("\nFirst error encountered: {err:#}");
                }
                println!("Use --force flag to overwrite existing packages.");
            }
            bail!("{failed} package(s) failed to upload");
        }

        println!("All packages processed successfully!");
        Ok(())
    }

    pub async fn list_packages(
        &self,
        distribution: Option<String>,
        component: Option<String>,
        package_name: Option<String>,
    ) -> anyhow::Result<()> {
        let metadata = self.load_metadata().await?;
        let mut rows = Vec::<PackageRow>::new();

        for dist in &metadata.distributions {
            if distribution.as_ref().is_some_and(|filter| filter != dist) {
                continue;
            }
            for comp in &metadata.components {
                if component.as_ref().is_some_and(|filter| filter != comp) {
                    continue;
                }
                for arch in metadata
                    .architectures
                    .iter()
                    .filter(|arch| arch.as_str() != "all")
                {
                    let repo = RepositoryPath {
                        distribution: dist.clone(),
                        component: comp.clone(),
                        architecture: arch.clone(),
                    };
                    let Ok(data) = self.store.download(&self.key(repo.packages_path())).await
                    else {
                        continue;
                    };
                    let Ok(packages) = parse_packages(&String::from_utf8_lossy(&data)) else {
                        continue;
                    };
                    for package in packages {
                        if package_name.as_ref().is_some_and(|filter| {
                            !package
                                .package
                                .to_lowercase()
                                .contains(&filter.to_lowercase())
                        }) {
                            continue;
                        }
                        rows.push(PackageRow {
                            name: package.package,
                            version: package.version,
                            architecture: package.architecture,
                            component: comp.clone(),
                            distribution: dist.clone(),
                            description: package
                                .description
                                .unwrap_or_default()
                                .lines()
                                .next()
                                .unwrap_or_default()
                                .to_string(),
                        });
                    }
                }
            }
        }

        rows.sort_by(|a, b| {
            a.name
                .cmp(&b.name)
                .then(a.version.cmp(&b.version))
                .then(a.architecture.cmp(&b.architecture))
        });
        rows.dedup_by(|a, b| {
            a.name == b.name
                && a.version == b.version
                && a.architecture == b.architecture
                && a.component == b.component
                && a.distribution == b.distribution
        });

        show_active_filters(
            distribution.as_deref(),
            component.as_deref(),
            package_name.as_deref(),
        );

        if rows.is_empty() {
            println!("No packages found matching the criteria.");
            return Ok(());
        }

        let mut table = Table::new();
        table.load_preset(UTF8_FULL).set_header([
            "Package",
            "Version",
            "Architecture",
            "Component",
            "Distribution",
            "Description",
        ]);
        for row in &rows {
            table.add_row(vec![
                row.name.clone(),
                row.version.clone(),
                row.architecture.clone(),
                row.component.clone(),
                row.distribution.clone(),
                truncate(&row.description, 60),
            ]);
        }
        println!("{table}");
        println!("\nTotal packages: {}", rows.len());
        Ok(())
    }

    pub async fn remove(&self, request: RemoveRequest) -> anyhow::Result<()> {
        let metadata = self.load_metadata().await?;
        let repo = RepositoryPath {
            distribution: request.distribution.clone(),
            component: request.component.clone(),
            architecture: request.architecture.clone(),
        };
        let filename = format!(
            "{}_{}_{}.deb",
            request.package, request.version, request.architecture
        );
        let pool_path = format!("{}/{}", repo.pool_path(&request.package), filename);

        if !self.store.exists(&self.key(&pool_path)).await? {
            bail!(
                "package {} version {} architecture {} not found in {}/{}",
                request.package,
                request.version,
                request.architecture,
                request.distribution,
                request.component
            );
        }

        println!("Package: {}", request.package);
        println!("Version: {}", request.version);
        println!("Architecture: {}", request.architecture);
        println!("Distribution: {}", request.distribution);
        println!("Component: {}", request.component);
        println!();
        println!("Found package at: {pool_path}");

        if !request.force {
            print!("Are you sure you want to remove this package? (y/N): ");
            io::stdout().flush()?;
            let mut response = String::new();
            io::stdin().read_line(&mut response)?;
            let response = response.trim().to_lowercase();
            if response != "y" && response != "yes" {
                println!("Removal cancelled.");
                return Ok(());
            }
        }

        self.store.delete(&self.key(&pool_path)).await?;
        let packages_path = repo.packages_path();
        if self.store.exists(&self.key(&packages_path)).await? {
            let packages_data = self.store.download(&self.key(&packages_path)).await?;
            let mut packages = parse_packages(&String::from_utf8_lossy(&packages_data))?;
            packages.retain(|pkg| {
                pkg.package != request.package
                    || pkg.version != request.version
                    || pkg.architecture != request.architecture
            });

            let updated = generate_packages_content(&packages);
            self.store
                .upload_bytes(&self.key(&packages_path), updated.as_bytes(), "text/plain")
                .await?;
            self.store
                .upload_bytes(
                    &self.key(repo.packages_gz_path()),
                    compress_gzip(updated.as_bytes())?,
                    "application/gzip",
                )
                .await?;
        }
        self.update_release(&request.distribution, &metadata, &request.signing_keys)
            .await?;

        println!("Package removed successfully!");
        Ok(())
    }

    pub async fn rotate_key(&self, signing_keys: SigningKeys) -> anyhow::Result<()> {
        let metadata = self.load_metadata().await?;
        let Some(active_key) = signing_keys.active() else {
            bail!("rotate-key requires an active private signing key");
        };

        println!(
            "Rotating APT repository signing keys in S3 bucket: {}",
            self.cfg.bucket()?
        );
        self.upload_public_keys(&signing_keys).await?;
        self.create_setup_script(&metadata, active_key).await?;

        for distribution in &metadata.distributions {
            println!("Re-signing APT distribution: {distribution}");
            self.update_release(distribution, &metadata, &signing_keys)
                .await?;
        }

        println!("APT signing keys rotated successfully.");
        Ok(())
    }

    async fn upload_one(
        &self,
        file: &Path,
        request: &UploadRequest,
        mut metadata: Option<&mut RepoMetadata>,
    ) -> anyhow::Result<UploadOutcome> {
        let package = DebPackage::from_path(file)?;
        let architecture = package.architecture()?.to_string();
        let package_name = package.package()?.to_string();
        let version = package.version()?.to_string();
        let filename = package.generated_filename()?;

        println!("  Package: {package_name}");
        println!("  Version: {version}");
        println!("  Architecture: {architecture}");
        println!("  Generated filename: {filename}");

        if let Some(metadata) = metadata.as_deref_mut() {
            self.ensure_architecture(metadata, &architecture).await?;
        }

        let repo = RepositoryPath {
            distribution: request.distribution.clone(),
            component: request.component.clone(),
            architecture: architecture.clone(),
        };
        let pool_path = format!("{}/{}", repo.pool_path(&package_name), filename);

        if self
            .package_exists(&repo, &package_name, &version, &architecture)
            .await?
        {
            if request.force {
                println!("  Force uploading (overwriting existing package)");
            } else {
                return Ok(UploadOutcome::Skipped);
            }
        }

        self.store
            .upload_bytes(
                &self.key(&pool_path),
                package.raw.clone(),
                "application/vnd.debian.binary-package",
            )
            .await?;

        let packages_path = repo.packages_path();
        let mut existing = if self.store.exists(&self.key(&packages_path)).await? {
            String::from_utf8(self.store.download(&self.key(&packages_path)).await?)?
        } else {
            String::new()
        };
        if !existing.is_empty() && !existing.ends_with("\n\n") {
            if existing.ends_with('\n') {
                existing.push('\n');
            } else {
                existing.push_str("\n\n");
            }
        }
        existing.push_str(&package.package_entry(&pool_path));
        if !existing.ends_with("\n\n") {
            existing.push('\n');
        }

        self.store
            .upload_bytes(&self.key(&packages_path), existing.as_bytes(), "text/plain")
            .await?;
        self.store
            .upload_bytes(
                &self.key(repo.packages_gz_path()),
                compress_gzip(existing.as_bytes())?,
                "application/gzip",
            )
            .await?;

        let fallback_metadata;
        let release_metadata = if let Some(metadata) = metadata.as_deref() {
            metadata
        } else {
            fallback_metadata = RepoMetadata {
                origin: String::new(),
                label: String::new(),
                description: String::new(),
                distributions: vec![request.distribution.clone()],
                components: vec![request.component.clone()],
                architectures: vec![architecture],
            };
            &fallback_metadata
        };
        self.update_release(
            &request.distribution,
            release_metadata,
            &request.signing_keys,
        )
        .await?;

        Ok(UploadOutcome::Uploaded)
    }

    async fn create_initial_indexes(
        &self,
        metadata: &RepoMetadata,
        signing_keys: &SigningKeys,
    ) -> anyhow::Result<()> {
        for dist in &metadata.distributions {
            for comp in &metadata.components {
                for arch in metadata
                    .architectures
                    .iter()
                    .filter(|arch| arch.as_str() != "all")
                {
                    let repo = RepositoryPath {
                        distribution: dist.clone(),
                        component: comp.clone(),
                        architecture: arch.clone(),
                    };
                    self.store
                        .upload_bytes(&self.key(repo.packages_path()), Vec::new(), "text/plain")
                        .await?;
                    self.store
                        .upload_bytes(
                            &self.key(repo.packages_gz_path()),
                            compress_gzip(&[])?,
                            "application/gzip",
                        )
                        .await?;
                }
            }
            self.update_release(dist, metadata, signing_keys).await?;
        }
        Ok(())
    }

    async fn ensure_architecture(
        &self,
        metadata: &mut RepoMetadata,
        architecture: &str,
    ) -> anyhow::Result<()> {
        if architecture == "all"
            || metadata
                .architectures
                .iter()
                .any(|arch| arch == architecture)
        {
            return Ok(());
        }

        metadata.architectures.push(architecture.to_string());
        for dist in &metadata.distributions {
            for comp in &metadata.components {
                let repo = RepositoryPath {
                    distribution: dist.clone(),
                    component: comp.clone(),
                    architecture: architecture.to_string(),
                };
                self.store
                    .upload_bytes(&self.key(repo.packages_path()), Vec::new(), "text/plain")
                    .await?;
                self.store
                    .upload_bytes(
                        &self.key(repo.packages_gz_path()),
                        compress_gzip(&[])?,
                        "application/gzip",
                    )
                    .await?;
            }
        }
        self.save_metadata(metadata).await
    }

    async fn update_release(
        &self,
        distribution: &str,
        metadata: &RepoMetadata,
        signing_keys: &SigningKeys,
    ) -> anyhow::Result<()> {
        let architectures = metadata
            .architectures
            .iter()
            .filter(|arch| arch.as_str() != "all")
            .cloned()
            .collect::<Vec<_>>();
        let mut files = Vec::new();

        for comp in &metadata.components {
            for arch in &architectures {
                let repo = RepositoryPath {
                    distribution: distribution.to_string(),
                    component: comp.clone(),
                    architecture: arch.clone(),
                };

                for (s3_path, release_path) in [
                    (
                        repo.packages_path(),
                        format!("{}/binary-{}/Packages", comp, arch),
                    ),
                    (
                        repo.packages_gz_path(),
                        format!("{}/binary-{}/Packages.gz", comp, arch),
                    ),
                ] {
                    if let Ok(data) = self.store.download(&self.key(&s3_path)).await {
                        let (md5, sha1, sha256) = calculate_hashes(&data);
                        files.push(ReleaseFileEntry {
                            filename: release_path,
                            size: data.len(),
                            md5,
                            sha1,
                            sha256,
                        });
                    }
                }
            }
        }

        let release = ReleaseFile {
            origin: metadata.origin.clone(),
            label: metadata.label.clone(),
            description: metadata.description.clone(),
            distribution: distribution.to_string(),
            components: metadata.components.clone(),
            architectures,
            date: Utc::now(),
            files,
        };
        let release_data = release.generate();
        let release_path = format!("dists/{distribution}/Release");
        self.store
            .upload_bytes(&self.key(&release_path), release_data.clone(), "text/plain")
            .await?;

        if let Some(key_pair) = signing_keys.active() {
            self.store
                .upload_bytes(
                    &self.key(format!("{release_path}.gpg")),
                    key_pair.sign_detached(&release_data)?.into_bytes(),
                    "application/pgp-signature",
                )
                .await?;
            self.upload_public_keys(signing_keys).await?;
        }

        if let Some(invalidator) = CdnInvalidator::from_env()? {
            let mut paths = vec![
                self.key(&release_path),
                self.key(format!("dists/{distribution}/Release.gpg")),
            ];
            for comp in &metadata.components {
                for arch in metadata
                    .architectures
                    .iter()
                    .filter(|arch| arch.as_str() != "all")
                {
                    paths.push(self.key(format!(
                        "dists/{distribution}/{comp}/binary-{arch}/Packages"
                    )));
                    paths.push(self.key(format!(
                        "dists/{distribution}/{comp}/binary-{arch}/Packages.gz"
                    )));
                }
            }
            if self
                .store
                .exists(&self.key("setup.sh"))
                .await
                .unwrap_or(false)
            {
                paths.push(self.key("setup.sh"));
            }
            if signing_keys.active().is_some() {
                paths.push(self.key("pubkey.asc"));
                paths.push(self.key("pubkey.gpg"));
            }
            if let Err(err) = invalidator.invalidate(&self.cfg, &paths).await {
                println!("Warning: CDN invalidation failed: {err:#}");
            }
        }

        Ok(())
    }

    async fn upload_public_keys(&self, signing_keys: &SigningKeys) -> anyhow::Result<()> {
        if signing_keys.active().is_none() {
            return Ok(());
        }
        self.store
            .upload_bytes(
                &self.key("pubkey.asc"),
                signing_keys.public_key_armored()?,
                "application/pgp-keys",
            )
            .await?;
        self.store
            .upload_bytes(
                &self.key("pubkey.gpg"),
                signing_keys.public_key_binary()?,
                "application/pgp-keys",
            )
            .await?;
        Ok(())
    }

    async fn create_setup_script(
        &self,
        metadata: &RepoMetadata,
        key_pair: &KeyPair,
    ) -> anyhow::Result<()> {
        let base_url = self.deb_base_url()?;
        let repo_id = repository_identifier(metadata, self.cfg.bucket()?);
        let mut script = format!(
            r#"#!/bin/bash
set -e

# APT Repository Setup Script
# Generated automatically by Hakobin Package

echo "Setting up APT repository: {label}"
echo "Origin: {origin}"
echo "Description: {description}"

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "This script must be run as root (use sudo)"
    exit 1
fi

# Create keyring directory if it doesn't exist
mkdir -p /etc/apt/keyrings

# Download and install GPG key (binary format, no gpg required)
echo "Installing repository GPG key..."
curl -fsSL {base_url}/pubkey.gpg -o /etc/apt/keyrings/{repo_id}.gpg
chmod 644 /etc/apt/keyrings/{repo_id}.gpg

# Add repository to sources list
echo "Adding repository to APT sources..."
cat > /etc/apt/sources.list.d/{repo_id}.list << 'EOF'
# {label} - {origin}
# Repository setup: {base_url}
"#,
            label = metadata.label,
            origin = metadata.origin,
            description = metadata.description
        );

        for dist in &metadata.distributions {
            for comp in &metadata.components {
                script.push_str(&format!(
                    "deb [signed-by=/etc/apt/keyrings/{repo_id}.gpg] {base_url} {dist} {comp}\n"
                ));
            }
        }

        script.push_str(&format!(
            r#"EOF

# Update package lists
echo "Updating package lists..."
apt update

echo ""
echo "Repository setup completed successfully!"
echo ""
echo "You can now install packages from this repository using:"
echo "  apt install <package-name>"
echo ""
echo "Available distributions: {distributions}"
echo "Available components: {components}"
echo "Supported architectures: {architectures}"
echo ""
echo "Repository GPG Key ID: {key_id}"
"#,
            distributions = metadata.distributions.join(", "),
            components = metadata.components.join(", "),
            architectures = metadata.architectures.join(", "),
            key_id = key_pair.key_id,
        ));

        if let Some(expiration) = key_pair.expiration {
            script.push_str(&format!(
                "echo \"GPG Key Expires: {}\"\n",
                expiration.format("%Y-%m-%d")
            ));
        } else {
            script.push_str("echo \"GPG Key Expires: Never\"\n");
        }
        script.push_str("echo \"\"\n");

        self.store
            .upload_bytes(&self.key("setup.sh"), script.as_bytes(), "text/plain")
            .await
    }

    async fn package_exists(
        &self,
        repo: &RepositoryPath,
        package: &str,
        version: &str,
        architecture: &str,
    ) -> anyhow::Result<bool> {
        if !self.store.exists(&self.key(repo.packages_path())).await? {
            return Ok(false);
        }
        let data = self.store.download(&self.key(repo.packages_path())).await?;
        let packages = parse_packages(&String::from_utf8_lossy(&data))?;
        Ok(packages.iter().any(|pkg| {
            pkg.package == package && pkg.version == version && pkg.architecture == architecture
        }))
    }

    async fn load_metadata(&self) -> anyhow::Result<RepoMetadata> {
        let data = self.store.download(&self.key(METADATA_PATH)).await?;
        serde_json::from_slice(&data).context("failed to parse repository metadata")
    }

    async fn save_metadata(&self, metadata: &RepoMetadata) -> anyhow::Result<()> {
        let data = serde_json::to_vec_pretty(metadata)?;
        self.store
            .upload_bytes(&self.key(METADATA_PATH), data, "application/json")
            .await
    }

    fn key(&self, path: impl AsRef<str>) -> String {
        format!("{DEB_PREFIX}/{}", path.as_ref().trim_start_matches('/'))
    }

    fn deb_base_url(&self) -> anyhow::Result<String> {
        Ok(format!(
            "{}/{}",
            self.cfg.repository_base_url()?.trim_end_matches('/'),
            DEB_PREFIX
        ))
    }
}

enum UploadOutcome {
    Uploaded,
    Skipped,
}

struct PackageRow {
    name: String,
    version: String,
    architecture: String,
    component: String,
    distribution: String,
    description: String,
}

fn value_or_prompt(value: Option<String>, prompt: &str, default: &str) -> anyhow::Result<String> {
    match value {
        Some(value) if !value.trim().is_empty() => Ok(value.trim().to_string()),
        _ => Ok(Text::new(prompt).with_default(default).prompt()?),
    }
}

fn list_or_prompt(
    value: Option<Vec<String>>,
    prompt: &str,
    default: &str,
) -> anyhow::Result<Vec<String>> {
    let raw = match value {
        Some(values) if !values.is_empty() => values.join(","),
        _ => Text::new(prompt).with_default(default).prompt()?,
    };
    let values = raw
        .split(',')
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(ToOwned::to_owned)
        .collect::<Vec<_>>();
    if values.is_empty() {
        bail!("{prompt} must contain at least one value");
    }
    Ok(values)
}

fn repository_identifier(metadata: &RepoMetadata, bucket: &str) -> String {
    let source = [&metadata.origin, &metadata.label]
        .into_iter()
        .find(|value| !value.trim().is_empty())
        .map(String::as_str)
        .unwrap_or(bucket);

    let mut out = String::new();
    let mut last_dash = false;
    for ch in source.chars().flat_map(|c| c.to_lowercase()) {
        if ch.is_ascii_alphanumeric() {
            out.push(ch);
            last_dash = false;
        } else if !last_dash {
            out.push('-');
            last_dash = true;
        }
    }
    let mut out = out.trim_matches('-').to_string();
    if out.len() > 50 {
        out.truncate(50);
        out = out.trim_end_matches('-').to_string();
    }
    if out.len() < 2 {
        bucket.to_lowercase()
    } else {
        out
    }
}

fn show_active_filters(
    distribution: Option<&str>,
    component: Option<&str>,
    package_name: Option<&str>,
) {
    let mut filters = Vec::new();
    if let Some(distribution) = distribution.filter(|value| !value.is_empty()) {
        filters.push(format!("distribution: {distribution}"));
    }
    if let Some(component) = component.filter(|value| !value.is_empty()) {
        filters.push(format!("component: {component}"));
    }
    if let Some(package_name) = package_name.filter(|value| !value.is_empty()) {
        filters.push(format!("package name: {package_name}"));
    }

    if !filters.is_empty() {
        println!("Active filters: {}", filters.join(", "));
    }
}

fn truncate(value: &str, max: usize) -> String {
    if value.chars().count() <= max {
        value.to_string()
    } else {
        let mut truncated = value
            .chars()
            .take(max.saturating_sub(3))
            .collect::<String>();
        truncated.push_str("...");
        truncated
    }
}

#[allow(dead_code)]
fn _expiration_choices() -> Vec<u32> {
    Select::new("Key Expiration", vec![1, 2, 3, 5, 10, 20, 0])
        .with_starting_cursor(1)
        .prompt()
        .map(|v| vec![v])
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::openpgp::KeyPair;
    use crate::storage::test_support::MemoryStore;
    use flate2::Compression;
    use flate2::write::GzEncoder;
    use tar::{Builder, Header};

    #[test]
    fn repository_identifier_sanitizes() {
        let metadata = RepoMetadata {
            origin: "Acme Corp. & Co!".into(),
            label: String::new(),
            description: String::new(),
            distributions: vec!["stable".into()],
            components: vec!["main".into()],
            architectures: vec!["amd64".into()],
        };
        assert_eq!(repository_identifier(&metadata, "bucket"), "acme-corp-co");
    }

    #[tokio::test]
    async fn apt_workflow_upload_remove_and_rotate_key_uses_memory_store() -> anyhow::Result<()> {
        let cfg = test_config();
        let store = MemoryStore::new();
        let manager = RepositoryManager::with_store(cfg, store.clone());
        let metadata = RepoMetadata {
            origin: "Hakobin".into(),
            label: "Hakobin APT".into(),
            description: "Test APT repository".into(),
            distributions: vec!["stable".into()],
            components: vec!["main".into()],
            architectures: vec!["amd64".into()],
        };
        let old_key = generated_key("Hakobin Old APT");
        let old_keys = SigningKeys::from_active(old_key.clone());

        manager.save_metadata(&metadata).await?;
        manager.create_initial_indexes(&metadata, &old_keys).await?;
        manager.upload_public_keys(&old_keys).await?;
        manager.create_setup_script(&metadata, &old_key).await?;

        let package_dir = tempfile::tempdir()?;
        let package_path = package_dir.path().join("demo.deb");
        std::fs::write(
            &package_path,
            test_deb(
                b"Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nMaintainer: Test <test@example.com>\nDescription: Demo package\n",
            ),
        )?;

        manager
            .upload(UploadRequest {
                deb_files: vec![package_path],
                distribution: "stable".into(),
                component: "main".into(),
                force: false,
                signing_keys: old_keys.clone(),
            })
            .await?;

        let pool_key = "deb/pool/main/d/demo/demo_1.0.0_amd64.deb";
        let packages_key = "deb/dists/stable/main/binary-amd64/Packages";
        assert!(store.body(pool_key).is_some());
        assert_eq!(
            store.content_type(pool_key).as_deref(),
            Some("application/vnd.debian.binary-package")
        );
        let packages = String::from_utf8(store.body(packages_key).unwrap())?;
        assert!(packages.contains("Package: demo"));
        assert!(packages.contains("Filename: pool/main/d/demo/demo_1.0.0_amd64.deb"));

        manager
            .remove(RemoveRequest {
                package: "demo".into(),
                version: "1.0.0".into(),
                architecture: "amd64".into(),
                distribution: "stable".into(),
                component: "main".into(),
                force: true,
                signing_keys: old_keys.clone(),
            })
            .await?;

        assert!(store.body(pool_key).is_none());
        let packages = String::from_utf8(store.body(packages_key).unwrap())?;
        assert!(!packages.contains("Package: demo"));

        let new_key = generated_key("Hakobin New APT");
        let rotated_keys = SigningKeys::new(Some(new_key.clone()), vec![old_key.public_cert()?]);
        manager.rotate_key(rotated_keys).await?;

        let public_keys = String::from_utf8(store.body("deb/pubkey.asc").unwrap())?;
        let setup = String::from_utf8(store.body("deb/setup.sh").unwrap())?;
        assert_eq!(
            public_keys
                .matches("-----BEGIN PGP PUBLIC KEY BLOCK-----")
                .count(),
            2
        );
        assert!(setup.contains(&new_key.key_id));
        assert!(store.body("deb/dists/stable/Release").is_some());
        assert!(store.body("deb/dists/stable/Release.gpg").is_some());
        assert!(store.keys().iter().any(|key| key == "deb/pubkey.gpg"));
        Ok(())
    }

    fn test_config() -> Config {
        Config {
            s3_endpoint: None,
            s3_access_key_id: Some("test-access-key".into()),
            s3_secret_access_key: Some("test-secret-key".into()),
            s3_bucket_name: Some("test-bucket".into()),
            s3_region: "us-east-1".into(),
            s3_use_path_style: true,
            public_url: Some("https://packages.example.test".into()),
        }
    }

    fn generated_key(name: &str) -> KeyPair {
        KeyPair::generate(name, "hakobin@example.com", "test", 0).unwrap()
    }

    fn test_deb(control: &[u8]) -> Vec<u8> {
        let control_tar = gzipped_control_tar(control);
        let empty_tar = gzipped_control_tar(b"Package: ignored\n");
        let mut ar = b"!<arch>\n".to_vec();
        append_ar_member(&mut ar, "debian-binary", b"2.0\n");
        append_ar_member(&mut ar, "control.tar.gz", &control_tar);
        append_ar_member(&mut ar, "data.tar.gz", &empty_tar);
        ar
    }

    fn gzipped_control_tar(control: &[u8]) -> Vec<u8> {
        let mut tar = Vec::new();
        {
            let mut builder = Builder::new(&mut tar);
            let mut header = Header::new_gnu();
            header.set_path("./control").unwrap();
            header.set_size(control.len() as u64);
            header.set_mode(0o644);
            header.set_cksum();
            builder.append(&header, control).unwrap();
            builder.finish().unwrap();
        }

        let mut gz = GzEncoder::new(Vec::new(), Compression::default());
        gz.write_all(&tar).unwrap();
        gz.finish().unwrap()
    }

    fn append_ar_member(ar: &mut Vec<u8>, name: &str, data: &[u8]) {
        let header = format!(
            "{:<16}{:<12}{:<6}{:<6}{:<8}{:<10}`\n",
            format!("{name}/"),
            0,
            0,
            0,
            0o100644,
            data.len()
        );
        assert_eq!(header.len(), 60);
        ar.extend_from_slice(header.as_bytes());
        ar.extend_from_slice(data);
        if data.len() % 2 == 1 {
            ar.push(b'\n');
        }
    }
}
