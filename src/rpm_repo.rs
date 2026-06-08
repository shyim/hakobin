use std::collections::BTreeSet;
use std::io::{self, BufReader, Cursor, Write};
use std::path::{Path, PathBuf};

use anyhow::{Context, bail};
use chrono::Utc;
use comfy_table::{Table, presets::UTF8_FULL};
use flate2::Compression;
use flate2::write::GzEncoder;
use quick_xml::escape::escape;
use rpm::{Dependency, FileType, Package, PackageMetadata};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::cdn::CdnInvalidator;
use crate::config::Config;
use crate::openpgp::SigningKeys;
use crate::storage::{S3Store, Store};

const RPM_PREFIX: &str = "rpm";
const RPM_PUBLIC_KEY_NAME: &str = "RPM-GPG-KEY-hakobin.asc";

#[derive(Clone)]
pub struct RpmRepositoryManager<S = S3Store> {
    cfg: Config,
    store: S,
}

pub struct RpmInitRequest {
    pub repo: String,
    pub arch: String,
    pub signing_keys: SigningKeys,
}

pub struct RpmUploadRequest {
    pub rpm_files: Vec<PathBuf>,
    pub repo: String,
    pub arch: String,
    pub force: bool,
    pub signing_keys: SigningKeys,
}

pub struct RpmListRequest {
    pub repo: String,
    pub arch: String,
    pub package_name: Option<String>,
}

pub struct RpmRemoveRequest {
    pub package: String,
    pub epoch: String,
    pub version: String,
    pub release: String,
    pub arch: String,
    pub repo: String,
    pub repo_arch: String,
    pub force: bool,
    pub signing_keys: SigningKeys,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RpmPackage {
    name: String,
    epoch: String,
    version: String,
    release: String,
    arch: String,
    summary: String,
    description: String,
    license: String,
    vendor: String,
    group: String,
    url: String,
    source_rpm: String,
    build_time: u64,
    package_size: u64,
    installed_size: u64,
    archive_size: u64,
    checksum: String,
    location: String,
    files: Vec<RpmFile>,
    provides: Vec<RpmDependency>,
    requires: Vec<RpmDependency>,
    conflicts: Vec<RpmDependency>,
    obsoletes: Vec<RpmDependency>,
    changelogs: Vec<RpmChangelog>,
    raw: Vec<u8>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RpmFile {
    path: String,
    kind: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RpmDependency {
    name: String,
    flags: String,
    epoch: String,
    version: String,
    release: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RpmChangelog {
    author: String,
    date: u64,
    text: String,
}

struct MetadataFile {
    kind: &'static str,
    filename: String,
    data: Vec<u8>,
    open_checksum: String,
    checksum: String,
    open_size: usize,
    size: usize,
}

#[derive(Debug, Clone, Eq, PartialEq, Ord, PartialOrd)]
struct RpmRepositoryId {
    repo: String,
    arch: String,
}

impl RpmRepositoryManager<S3Store> {
    pub async fn connect(cfg: Config) -> anyhow::Result<Self> {
        let store = S3Store::connect(&cfg).await?;
        Ok(Self { cfg, store })
    }
}

#[allow(private_bounds)]
impl<S> RpmRepositoryManager<S>
where
    S: Store,
{
    #[cfg(test)]
    pub(crate) fn with_store(cfg: Config, store: S) -> Self {
        Self { cfg, store }
    }

    pub async fn init(&self, request: RpmInitRequest) -> anyhow::Result<()> {
        println!(
            "Initializing RPM repository '{}' for {} in S3 bucket: {}",
            request.repo,
            request.arch,
            self.cfg.bucket()?
        );
        self.write_metadata(&request.repo, &request.arch, &[], &request.signing_keys)
            .await?;
        println!("RPM repository initialized successfully.");
        println!(
            "Repository base URL: {}/{}",
            self.cfg.repository_base_url()?.trim_end_matches('/'),
            self.repo_prefix(&request.repo, &request.arch)
        );
        Ok(())
    }

    pub async fn upload(&self, request: RpmUploadRequest) -> anyhow::Result<()> {
        for file in &request.rpm_files {
            if !file.exists() {
                bail!("file not found: {}", file.display());
            }
        }

        println!(
            "Uploading {} RPM package(s) to S3 bucket: {}",
            request.rpm_files.len(),
            self.cfg.bucket()?
        );
        println!("Repository: {}", request.repo);
        println!("Repository architecture: {}", request.arch);

        let mut uploaded = 0usize;
        let mut skipped = 0usize;
        let mut failed = 0usize;
        let mut first_error: Option<anyhow::Error> = None;

        for (index, file) in request.rpm_files.iter().enumerate() {
            println!(
                "\n[{}/{}] Processing: {}",
                index + 1,
                request.rpm_files.len(),
                file.display()
            );
            match self.upload_one(file, &request).await {
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

        if uploaded > 0 {
            let packages = self.load_packages(&request.repo, &request.arch).await?;
            self.write_metadata(
                &request.repo,
                &request.arch,
                &packages,
                &request.signing_keys,
            )
            .await?;
        }

        println!("\n==================================================");
        println!("RPM Upload Summary:");
        println!("Uploaded: {uploaded}");
        if skipped > 0 {
            println!("Skipped: {skipped}");
        }
        if failed > 0 {
            println!("Failed: {failed}");
            if let Some(err) = first_error {
                println!("\nFirst error encountered: {err:#}");
            }
            println!("Use --force flag to overwrite existing packages.");
            bail!("{failed} RPM package(s) failed to upload");
        }

        println!("All RPM packages processed successfully!");
        Ok(())
    }

    pub async fn list(&self, request: RpmListRequest) -> anyhow::Result<()> {
        let mut packages = self.load_packages(&request.repo, &request.arch).await?;
        if let Some(filter) = &request.package_name {
            let filter = filter.to_lowercase();
            packages.retain(|package| package.name.to_lowercase().contains(&filter));
        }

        if packages.is_empty() {
            println!("No RPM packages found matching the criteria.");
            return Ok(());
        }

        packages.sort_by(|a, b| {
            a.name
                .cmp(&b.name)
                .then(a.version.cmp(&b.version))
                .then(a.release.cmp(&b.release))
                .then(a.arch.cmp(&b.arch))
        });

        let mut table = Table::new();
        table
            .load_preset(UTF8_FULL)
            .set_header(["Package", "Epoch", "Version", "Release", "Arch", "Summary"]);
        for package in &packages {
            table.add_row(vec![
                package.name.clone(),
                package.epoch.clone(),
                package.version.clone(),
                package.release.clone(),
                package.arch.clone(),
                truncate(&package.summary, 60),
            ]);
        }
        println!("{table}");
        println!("\nTotal RPM packages: {}", packages.len());
        Ok(())
    }

    pub async fn remove(&self, request: RpmRemoveRequest) -> anyhow::Result<()> {
        let packages = self
            .load_packages(&request.repo, &request.repo_arch)
            .await?;
        let Some(package) = packages.iter().find(|package| {
            package.name == request.package
                && package.epoch == request.epoch
                && package.version == request.version
                && package.release == request.release
                && package.arch == request.arch
        }) else {
            bail!(
                "package {} epoch {} version {} release {} architecture {} not found in {}/{}",
                request.package,
                request.epoch,
                request.version,
                request.release,
                request.arch,
                request.repo,
                request.repo_arch
            );
        };

        println!("Package: {}", package.name);
        println!("Epoch: {}", package.epoch);
        println!("Version: {}", package.version);
        println!("Release: {}", package.release);
        println!("Architecture: {}", package.arch);
        println!("Repository: {}", request.repo);
        println!("Repository architecture: {}", request.repo_arch);
        println!("Found package at: {}", package.location);

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

        self.store
            .delete(&self.key(&request.repo, &request.repo_arch, &package.location))
            .await?;
        let packages = self
            .load_packages(&request.repo, &request.repo_arch)
            .await?;
        self.write_metadata(
            &request.repo,
            &request.repo_arch,
            &packages,
            &request.signing_keys,
        )
        .await?;
        println!("RPM package removed successfully!");
        Ok(())
    }

    pub async fn rotate_key(&self, signing_keys: SigningKeys) -> anyhow::Result<()> {
        if signing_keys.active().is_none() {
            bail!("rotate-key requires an active private signing key");
        }

        let repositories = self.discover_repositories().await?;
        if repositories.is_empty() {
            bail!("no RPM repositories found under {RPM_PREFIX}/");
        }

        println!(
            "Rotating signing keys for {} RPM repository target(s) in S3 bucket: {}",
            repositories.len(),
            self.cfg.bucket()?
        );

        for repository in &repositories {
            println!(
                "Re-signing RPM repository: {}/{}",
                repository.repo, repository.arch
            );
            let packages = self
                .load_packages(&repository.repo, &repository.arch)
                .await?;
            let mut signed_packages = Vec::with_capacity(packages.len());

            for package in packages {
                let location = package.location.clone();
                let signed_package = package
                    .signed(&signing_keys)
                    .with_context(|| format!("failed to sign RPM package {location}"))?;
                signed_packages.push(signed_package);
            }

            for package in &signed_packages {
                self.store
                    .upload_bytes(
                        &self.key(&repository.repo, &repository.arch, &package.location),
                        package.raw.clone(),
                        "application/x-rpm",
                    )
                    .await?;
            }

            self.write_metadata(
                &repository.repo,
                &repository.arch,
                &signed_packages,
                &signing_keys,
            )
            .await?;
        }

        println!("RPM signing keys rotated successfully.");
        Ok(())
    }

    async fn upload_one(
        &self,
        file: &Path,
        request: &RpmUploadRequest,
    ) -> anyhow::Result<UploadOutcome> {
        let package = RpmPackage::from_path(file)?;
        if package.arch != request.arch && package.arch != "noarch" {
            bail!(
                "package architecture {} does not match repository architecture {}",
                package.arch,
                request.arch
            );
        }

        println!("  Package: {}", package.name);
        println!("  Epoch: {}", package.epoch);
        println!("  Version: {}", package.version);
        println!("  Release: {}", package.release);
        println!("  Architecture: {}", package.arch);
        println!("  Generated filename: {}", package.filename());

        let key = self.key(&request.repo, &request.arch, &package.location);
        if self.store.exists(&key).await? {
            if request.force {
                println!("  Force uploading (overwriting existing package)");
            } else {
                return Ok(UploadOutcome::Skipped);
            }
        }

        let package = package
            .signed(&request.signing_keys)
            .with_context(|| format!("failed to sign RPM package {}", file.display()))?;
        self.store
            .upload_bytes(&key, package.raw, "application/x-rpm")
            .await?;
        Ok(UploadOutcome::Uploaded)
    }

    async fn load_packages(&self, repo: &str, arch: &str) -> anyhow::Result<Vec<RpmPackage>> {
        let packages_prefix = self.key(repo, arch, "Packages/");
        let mut packages = Vec::new();

        for key in self.store.list_keys(&packages_prefix).await? {
            if !key.ends_with(".rpm") {
                continue;
            }
            let data = self.store.download(&key).await?;
            let location = key
                .strip_prefix(&format!("{}/", self.repo_prefix(repo, arch)))
                .unwrap_or(&key)
                .to_string();
            packages.push(RpmPackage::from_bytes(location, data)?);
        }

        packages.sort_by(|a, b| a.location.cmp(&b.location));
        Ok(packages)
    }

    async fn discover_repositories(&self) -> anyhow::Result<Vec<RpmRepositoryId>> {
        let repositories = self
            .store
            .list_keys(&format!("{RPM_PREFIX}/"))
            .await?
            .into_iter()
            .filter_map(|key| rpm_repository_from_key(&key))
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        Ok(repositories)
    }

    async fn write_metadata(
        &self,
        repo: &str,
        arch: &str,
        packages: &[RpmPackage],
        signing_keys: &SigningKeys,
    ) -> anyhow::Result<()> {
        let metadata = generate_repository_metadata(packages)?;
        let repodata_prefix = self.key(repo, arch, "repodata/");
        let public_key_path = self.key(repo, arch, RPM_PUBLIC_KEY_NAME);

        for file in [&metadata.primary, &metadata.filelists, &metadata.other] {
            self.store
                .upload_bytes(
                    &format!("{repodata_prefix}{}", file.filename),
                    file.data.clone(),
                    "application/gzip",
                )
                .await?;
        }

        let repomd_bytes = metadata.repomd.into_bytes();
        self.store
            .upload_bytes(
                &format!("{repodata_prefix}repomd.xml"),
                repomd_bytes.clone(),
                "application/xml",
            )
            .await?;

        if let Some((signature, public_key)) = sign_rpm_metadata(&repomd_bytes, signing_keys)? {
            self.store
                .upload_bytes(
                    &format!("{repodata_prefix}repomd.xml.asc"),
                    signature,
                    "application/pgp-signature",
                )
                .await?;
            self.store
                .upload_bytes(&public_key_path, public_key, "application/pgp-keys")
                .await?;
        }

        if let Some(invalidator) = CdnInvalidator::from_env()? {
            let mut paths = vec![format!("{repodata_prefix}repomd.xml")];
            paths.push(format!("{repodata_prefix}{}", metadata.primary.filename));
            paths.push(format!("{repodata_prefix}{}", metadata.filelists.filename));
            paths.push(format!("{repodata_prefix}{}", metadata.other.filename));
            if signing_keys.active().is_some() {
                paths.push(format!("{repodata_prefix}repomd.xml.asc"));
                paths.push(public_key_path);
            }
            if let Err(err) = invalidator.invalidate(&self.cfg, &paths).await {
                println!("Warning: CDN invalidation failed: {err:#}");
            }
        }

        Ok(())
    }

    fn repo_prefix(&self, repo: &str, arch: &str) -> String {
        format!("{RPM_PREFIX}/{repo}/{arch}")
    }

    fn key(&self, repo: &str, arch: &str, path: &str) -> String {
        format!(
            "{}/{}",
            self.repo_prefix(repo, arch),
            path.trim_start_matches('/')
        )
    }
}

impl RpmPackage {
    fn from_path(path: &Path) -> anyhow::Result<Self> {
        let raw =
            std::fs::read(path).with_context(|| format!("failed to read {}", path.display()))?;
        let filename = path
            .file_name()
            .and_then(|value| value.to_str())
            .context("RPM package path has no UTF-8 filename")?
            .to_string();
        Self::from_bytes(format!("Packages/{filename}"), raw)
    }

    fn from_bytes(location: String, raw: Vec<u8>) -> anyhow::Result<Self> {
        let metadata = parse_metadata(&raw)?;
        let name = metadata.get_name()?.to_string();
        let epoch = metadata.get_epoch().unwrap_or(0).to_string();
        let version = metadata.get_version()?.to_string();
        let release = metadata.get_release()?.to_string();
        let arch = metadata.get_arch()?.to_string();
        let checksum = hex::encode(Sha256::digest(&raw));
        let files = metadata
            .get_file_entries()
            .unwrap_or_default()
            .into_iter()
            .map(|file| RpmFile {
                path: file.path().display().to_string(),
                kind: match file.file_type() {
                    FileType::Dir => "dir",
                    FileType::SymbolicLink => "ghost",
                    FileType::Regular | FileType::Other => "file",
                    _ => "file",
                }
                .to_string(),
            })
            .collect::<Vec<_>>();
        let installed_size = metadata
            .get_file_entries()
            .unwrap_or_default()
            .into_iter()
            .map(|file| file.size() as u64)
            .sum::<u64>();

        Ok(Self {
            name,
            epoch,
            version,
            release,
            arch,
            summary: metadata.get_summary().unwrap_or("").to_string(),
            description: metadata.get_description().unwrap_or("").to_string(),
            license: metadata.get_license().unwrap_or("").to_string(),
            vendor: metadata.get_vendor().unwrap_or("").to_string(),
            group: metadata.get_group().unwrap_or("").to_string(),
            url: metadata.get_url().unwrap_or("").to_string(),
            source_rpm: metadata.get_source_rpm().unwrap_or("").to_string(),
            build_time: metadata.get_build_time().unwrap_or(0),
            package_size: raw.len() as u64,
            installed_size,
            archive_size: raw.len() as u64,
            checksum,
            location,
            files,
            provides: dependencies(metadata.get_provides().unwrap_or_default()),
            requires: dependencies(metadata.get_requires().unwrap_or_default()),
            conflicts: dependencies(metadata.get_conflicts().unwrap_or_default()),
            obsoletes: dependencies(metadata.get_obsoletes().unwrap_or_default()),
            changelogs: metadata
                .get_changelog_entries()
                .unwrap_or_default()
                .into_iter()
                .map(|entry| RpmChangelog {
                    author: entry.name,
                    date: entry.timestamp,
                    text: entry.description,
                })
                .collect(),
            raw,
        })
    }

    fn filename(&self) -> String {
        self.location
            .strip_prefix("Packages/")
            .unwrap_or(&self.location)
            .to_string()
    }

    fn signed(self, signing_keys: &SigningKeys) -> anyhow::Result<Self> {
        let raw = sign_rpm_package(&self.raw, signing_keys)?;
        Self::from_bytes(self.location, raw)
    }
}

fn parse_metadata(raw: &[u8]) -> anyhow::Result<PackageMetadata> {
    let mut reader = BufReader::new(Cursor::new(raw));
    Ok(PackageMetadata::parse(&mut reader)?)
}

fn dependencies(dependencies: Vec<Dependency>) -> Vec<RpmDependency> {
    dependencies
        .into_iter()
        .map(|dependency| {
            let (epoch, version, release) = split_evr(&dependency.version);
            RpmDependency {
                name: dependency.name,
                flags: dependency.flags.comparator_str().to_string(),
                epoch,
                version,
                release,
            }
        })
        .collect()
}

fn split_evr(value: &str) -> (String, String, String) {
    let (epoch, value) = value
        .split_once(':')
        .map(|(epoch, value)| (epoch.to_string(), value))
        .unwrap_or_else(|| ("0".to_string(), value));
    let (version, release) = value
        .rsplit_once('-')
        .map(|(version, release)| (version.to_string(), release.to_string()))
        .unwrap_or_else(|| (value.to_string(), String::new()));
    (epoch, version, release)
}

fn rpm_repository_from_key(key: &str) -> Option<RpmRepositoryId> {
    let path = key.strip_prefix(&format!("{RPM_PREFIX}/"))?;
    let mut parts = path.split('/');
    let repo = parts.next()?;
    let arch = parts.next()?;
    let marker = parts.next()?;

    if repo.is_empty() || arch.is_empty() {
        return None;
    }

    match marker {
        "Packages" | "repodata" => Some(RpmRepositoryId {
            repo: repo.to_string(),
            arch: arch.to_string(),
        }),
        _ => None,
    }
}

struct RepositoryMetadata {
    primary: MetadataFile,
    filelists: MetadataFile,
    other: MetadataFile,
    repomd: String,
}

fn generate_repository_metadata(packages: &[RpmPackage]) -> anyhow::Result<RepositoryMetadata> {
    let primary_xml = generate_primary_xml(packages);
    let filelists_xml = generate_filelists_xml(packages);
    let other_xml = generate_other_xml(packages);

    let primary = metadata_file("primary", primary_xml.as_bytes())?;
    let filelists = metadata_file("filelists", filelists_xml.as_bytes())?;
    let other = metadata_file("other", other_xml.as_bytes())?;
    let repomd = generate_repomd_xml([&primary, &filelists, &other]);

    Ok(RepositoryMetadata {
        primary,
        filelists,
        other,
        repomd,
    })
}

fn sign_rpm_metadata(
    repomd: &[u8],
    signing_keys: &SigningKeys,
) -> anyhow::Result<Option<(Vec<u8>, Vec<u8>)>> {
    let Some(key_pair) = signing_keys.active() else {
        return Ok(None);
    };
    Ok(Some((
        key_pair.sign_detached(repomd)?.into_bytes(),
        signing_keys.public_key_armored()?,
    )))
}

fn sign_rpm_package(raw: &[u8], signing_keys: &SigningKeys) -> anyhow::Result<Vec<u8>> {
    let Some(active_key) = signing_keys.active() else {
        return Ok(raw.to_vec());
    };

    let private_key = active_key.private_key_armored()?;
    let signer = rpm::signature::pgp::Signer::from_asc_bytes(&private_key)
        .context("failed to load RPM package signing key")?;
    let mut reader = BufReader::new(Cursor::new(raw));
    let mut package = Package::parse(&mut reader).context("failed to parse RPM package")?;

    package.sign(signer).context("failed to sign RPM package")?;

    let mut signed = Vec::new();
    package
        .write(&mut signed)
        .context("failed to serialize signed RPM package")?;
    Ok(signed)
}

fn metadata_file(kind: &'static str, xml: &[u8]) -> anyhow::Result<MetadataFile> {
    let mut encoder = GzEncoder::new(Vec::new(), Compression::default());
    encoder.write_all(xml)?;
    let data = encoder.finish()?;
    let open_checksum = hex::encode(Sha256::digest(xml));
    let checksum = hex::encode(Sha256::digest(&data));
    Ok(MetadataFile {
        kind,
        filename: format!("{checksum}-{kind}.xml.gz"),
        size: data.len(),
        open_size: xml.len(),
        data,
        open_checksum,
        checksum,
    })
}

fn generate_primary_xml(packages: &[RpmPackage]) -> String {
    let mut out = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="{}">
"#,
        packages.len()
    );

    for package in packages {
        out.push_str(r#"<package type="rpm">"#);
        tag(&mut out, "name", &package.name);
        tag(&mut out, "arch", &package.arch);
        out.push_str(&format!(
            r#"<version epoch="{}" ver="{}" rel="{}"/>"#,
            xml(&package.epoch),
            xml(&package.version),
            xml(&package.release)
        ));
        out.push_str(&format!(
            r#"<checksum type="sha256" pkgid="YES">{}</checksum>"#,
            package.checksum
        ));
        tag(&mut out, "summary", &package.summary);
        tag(&mut out, "description", &package.description);
        tag(&mut out, "packager", "");
        tag(&mut out, "url", &package.url);
        out.push_str(&format!(
            r#"<time file="{}" build="{}"/>"#,
            Utc::now().timestamp(),
            package.build_time
        ));
        out.push_str(&format!(
            r#"<size package="{}" installed="{}" archive="{}"/>"#,
            package.package_size, package.installed_size, package.archive_size
        ));
        out.push_str(&format!(r#"<location href="{}"/>"#, xml(&package.location)));
        out.push_str("<format>");
        rpm_tag(&mut out, "license", &package.license);
        rpm_tag(&mut out, "vendor", &package.vendor);
        rpm_tag(&mut out, "group", &package.group);
        rpm_tag(&mut out, "buildhost", "");
        rpm_tag(&mut out, "sourcerpm", &package.source_rpm);
        out.push_str(r#"<rpm:header-range start="0" end="0"/>"#);
        dependency_block(&mut out, "rpm:provides", &package.provides);
        dependency_block(&mut out, "rpm:requires", &package.requires);
        dependency_block(&mut out, "rpm:conflicts", &package.conflicts);
        dependency_block(&mut out, "rpm:obsoletes", &package.obsoletes);
        for file in package.files.iter().filter(|file| file.kind == "file") {
            tag(&mut out, "file", &file.path);
        }
        out.push_str("</format></package>");
    }
    out.push_str("</metadata>\n");
    out
}

fn generate_filelists_xml(packages: &[RpmPackage]) -> String {
    let mut out = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="{}">
"#,
        packages.len()
    );
    for package in packages {
        out.push_str(&format!(
            r#"<package pkgid="{}" name="{}" arch="{}">"#,
            package.checksum,
            xml(&package.name),
            xml(&package.arch)
        ));
        out.push_str(&format!(
            r#"<version epoch="{}" ver="{}" rel="{}"/>"#,
            xml(&package.epoch),
            xml(&package.version),
            xml(&package.release)
        ));
        for file in &package.files {
            if file.kind == "dir" {
                out.push_str(&format!(r#"<file type="dir">{}</file>"#, xml(&file.path)));
            } else if file.kind == "ghost" {
                out.push_str(&format!(r#"<file type="ghost">{}</file>"#, xml(&file.path)));
            } else {
                tag(&mut out, "file", &file.path);
            }
        }
        out.push_str("</package>");
    }
    out.push_str("</filelists>\n");
    out
}

fn generate_other_xml(packages: &[RpmPackage]) -> String {
    let mut out = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="{}">
"#,
        packages.len()
    );
    for package in packages {
        out.push_str(&format!(
            r#"<package pkgid="{}" name="{}" arch="{}">"#,
            package.checksum,
            xml(&package.name),
            xml(&package.arch)
        ));
        out.push_str(&format!(
            r#"<version epoch="{}" ver="{}" rel="{}"/>"#,
            xml(&package.epoch),
            xml(&package.version),
            xml(&package.release)
        ));
        for changelog in &package.changelogs {
            out.push_str(&format!(
                r#"<changelog author="{}" date="{}">{}</changelog>"#,
                xml(&changelog.author),
                changelog.date,
                xml(&changelog.text)
            ));
        }
        out.push_str("</package>");
    }
    out.push_str("</otherdata>\n");
    out
}

fn generate_repomd_xml(files: [&MetadataFile; 3]) -> String {
    let revision = Utc::now().timestamp();
    let mut out = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo">
<revision>{revision}</revision>
"#
    );
    for file in files {
        out.push_str(&format!(
            r#"<data type="{}"><checksum type="sha256">{}</checksum><open-checksum type="sha256">{}</open-checksum><location href="repodata/{}"/><timestamp>{revision}</timestamp><size>{}</size><open-size>{}</open-size></data>"#,
            file.kind,
            file.checksum,
            file.open_checksum,
            file.filename,
            file.size,
            file.open_size
        ));
    }
    out.push_str("</repomd>\n");
    out
}

fn dependency_block(out: &mut String, tag_name: &str, dependencies: &[RpmDependency]) {
    if dependencies.is_empty() {
        return;
    }
    out.push_str(&format!("<{tag_name}>"));
    for dependency in dependencies {
        out.push_str(&format!(r#"<rpm:entry name="{}""#, xml(&dependency.name)));
        if !dependency.flags.is_empty() {
            out.push_str(&format!(r#" flags="{}""#, xml(&dependency.flags)));
        }
        if !dependency.epoch.is_empty() {
            out.push_str(&format!(r#" epoch="{}""#, xml(&dependency.epoch)));
        }
        if !dependency.version.is_empty() {
            out.push_str(&format!(r#" ver="{}""#, xml(&dependency.version)));
        }
        if !dependency.release.is_empty() {
            out.push_str(&format!(r#" rel="{}""#, xml(&dependency.release)));
        }
        out.push_str("/>");
    }
    out.push_str(&format!("</{tag_name}>"));
}

fn tag(out: &mut String, tag_name: &str, value: &str) {
    out.push_str(&format!("<{tag_name}>{}</{tag_name}>", xml(value)));
}

fn rpm_tag(out: &mut String, tag_name: &str, value: &str) {
    out.push_str(&format!("<rpm:{tag_name}>{}</rpm:{tag_name}>", xml(value)));
}

fn xml(value: &str) -> String {
    escape(value).into_owned()
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

enum UploadOutcome {
    Uploaded,
    Skipped,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use crate::openpgp::KeyPair;
    use crate::storage::test_support::MemoryStore;
    use flate2::read::GzDecoder;
    use std::io::Read as _;

    #[test]
    fn split_evr_handles_epoch_and_release() {
        assert_eq!(
            split_evr("1:2.3.4-5.el9"),
            ("1".to_string(), "2.3.4".to_string(), "5.el9".to_string())
        );
        assert_eq!(
            split_evr("2.3.4"),
            ("0".to_string(), "2.3.4".to_string(), String::new())
        );
    }

    #[test]
    fn discovers_rpm_repository_from_metadata_and_package_keys() {
        assert_eq!(
            rpm_repository_from_key("rpm/stable/x86_64/repodata/repomd.xml"),
            Some(RpmRepositoryId {
                repo: "stable".into(),
                arch: "x86_64".into(),
            })
        );
        assert_eq!(
            rpm_repository_from_key("rpm/testing/aarch64/Packages/demo-1.0.0-1.aarch64.rpm"),
            Some(RpmRepositoryId {
                repo: "testing".into(),
                arch: "aarch64".into(),
            })
        );
        assert_eq!(
            rpm_repository_from_key("rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc"),
            None
        );
        assert_eq!(rpm_repository_from_key("deb/dists/stable/Release"), None);
    }

    #[test]
    fn metadata_uses_compressed_checksums_in_repomd() {
        let package = RpmPackage {
            name: "demo".into(),
            epoch: "0".into(),
            version: "1.0.0".into(),
            release: "1".into(),
            arch: "x86_64".into(),
            summary: "Demo".into(),
            description: "Demo package".into(),
            license: "MIT".into(),
            vendor: String::new(),
            group: String::new(),
            url: String::new(),
            source_rpm: String::new(),
            build_time: 0,
            package_size: 12,
            installed_size: 34,
            archive_size: 12,
            checksum: "abc123".into(),
            location: "Packages/demo-1.0.0-1.x86_64.rpm".into(),
            files: vec![RpmFile {
                path: "/usr/bin/demo".into(),
                kind: "file".into(),
            }],
            provides: Vec::new(),
            requires: Vec::new(),
            conflicts: Vec::new(),
            obsoletes: Vec::new(),
            changelogs: Vec::new(),
            raw: Vec::new(),
        };
        let metadata = generate_repository_metadata(&[package]).unwrap();
        assert!(metadata.repomd.contains("repodata/"));
        assert!(metadata.repomd.contains("primary"));
        assert!(metadata.repomd.contains("filelists"));
        assert!(metadata.repomd.contains("other"));
        assert!(metadata.primary.filename.ends_with("-primary.xml.gz"));
    }

    #[test]
    fn rpm_metadata_signature_is_ascii_armored() {
        let active = KeyPair::generate(
            "Hakobin Test",
            "hakobin@example.com",
            "RPM metadata signing",
            0,
        )
        .unwrap();
        let trusted = KeyPair::generate(
            "Hakobin Old",
            "hakobin-old@example.com",
            "RPM metadata signing",
            0,
        )
        .unwrap()
        .public_cert()
        .unwrap();
        let signing_keys = SigningKeys::new(Some(active), vec![trusted]);
        let (signature, public_key) = sign_rpm_metadata(b"<repomd/>", &signing_keys)
            .unwrap()
            .unwrap();

        let signature = String::from_utf8(signature).unwrap();
        let public_key = String::from_utf8(public_key).unwrap();
        assert!(signature.starts_with("-----BEGIN PGP SIGNATURE-----"));
        assert_eq!(
            public_key
                .matches("-----BEGIN PGP PUBLIC KEY BLOCK-----")
                .count(),
            2
        );
    }

    #[test]
    fn rpm_package_signing_leaves_bytes_unchanged_without_active_key() {
        let raw = test_rpm_package();
        let signed = sign_rpm_package(&raw, &SigningKeys::default()).unwrap();
        assert_eq!(signed, raw);
    }

    #[test]
    fn rpm_package_signing_uses_active_key() -> anyhow::Result<()> {
        let active = generated_key("Hakobin RPM Package");
        let raw = test_rpm_package();
        let signed = sign_rpm_package(&raw, &SigningKeys::from_active(active.clone()))?;

        assert_ne!(signed, raw);
        verify_rpm_signature(&signed, active.public_key.as_bytes())?;
        RpmPackage::from_bytes("Packages/demo-1.0.0-1.el9.x86_64.rpm".into(), signed)?;
        Ok(())
    }

    #[tokio::test]
    async fn rpm_workflow_upload_remove_and_rotate_key_uses_memory_store() -> anyhow::Result<()> {
        let cfg = test_config();
        let store = MemoryStore::new();
        let manager = RpmRepositoryManager::with_store(cfg, store.clone());
        let old_key = generated_key("Hakobin Old RPM");
        let old_keys = SigningKeys::from_active(old_key.clone());

        manager
            .init(RpmInitRequest {
                repo: "stable".into(),
                arch: "x86_64".into(),
                signing_keys: old_keys.clone(),
            })
            .await?;

        let package_dir = tempfile::tempdir()?;
        let package_path = package_dir.path().join("demo-1.0.0-1.el9.x86_64.rpm");
        std::fs::write(&package_path, test_rpm_package())?;

        manager
            .upload(RpmUploadRequest {
                rpm_files: vec![package_path],
                repo: "stable".into(),
                arch: "x86_64".into(),
                force: false,
                signing_keys: old_keys.clone(),
            })
            .await?;

        let package_key = "rpm/stable/x86_64/Packages/demo-1.0.0-1.el9.x86_64.rpm";
        assert!(store.body(package_key).is_some());
        assert_eq!(
            store.content_type(package_key).as_deref(),
            Some("application/x-rpm")
        );
        assert!(
            store
                .body("rpm/stable/x86_64/repodata/repomd.xml")
                .is_some()
        );
        assert!(
            store
                .body("rpm/stable/x86_64/repodata/repomd.xml.asc")
                .is_some()
        );
        let uploaded_package = store.body(package_key).unwrap();
        verify_rpm_signature(&uploaded_package, old_key.public_key.as_bytes())?;

        let new_key = generated_key("Hakobin New RPM");
        let rotated_keys = SigningKeys::new(Some(new_key.clone()), vec![old_key.public_cert()?]);
        manager.rotate_key(rotated_keys.clone()).await?;

        let rotated_package = store.body(package_key).unwrap();
        assert_ne!(rotated_package, uploaded_package);
        verify_rpm_signature(&rotated_package, new_key.public_key.as_bytes())?;
        let rotated_checksum = hex::encode(Sha256::digest(&rotated_package));
        assert!(primary_metadata_contains_checksum(
            &store,
            &rotated_checksum
        ));

        let public_keys = String::from_utf8(
            store
                .body("rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc")
                .unwrap(),
        )?;
        let signature = String::from_utf8(
            store
                .body("rpm/stable/x86_64/repodata/repomd.xml.asc")
                .unwrap(),
        )?;
        let repomd =
            String::from_utf8(store.body("rpm/stable/x86_64/repodata/repomd.xml").unwrap())?;
        assert_eq!(
            public_keys
                .matches("-----BEGIN PGP PUBLIC KEY BLOCK-----")
                .count(),
            2
        );
        assert!(signature.starts_with("-----BEGIN PGP SIGNATURE-----"));
        assert!(repomd.contains("<repomd "));
        assert!(
            store
                .keys()
                .iter()
                .any(|key| key.starts_with("rpm/stable/x86_64/repodata/"))
        );

        manager
            .remove(RpmRemoveRequest {
                package: "demo".into(),
                epoch: "0".into(),
                version: "1.0.0".into(),
                release: "1.el9".into(),
                arch: "x86_64".into(),
                repo: "stable".into(),
                repo_arch: "x86_64".into(),
                force: true,
                signing_keys: rotated_keys,
            })
            .await?;

        assert!(store.body(package_key).is_none());
        Ok(())
    }

    #[test]
    fn parses_generated_rpm_package() {
        let raw = test_rpm_package();

        let parsed =
            RpmPackage::from_bytes("Packages/demo-1.0.0-1.el9.x86_64.rpm".into(), raw).unwrap();
        assert_eq!(parsed.name, "demo");
        assert_eq!(parsed.version, "1.0.0");
        assert_eq!(parsed.release, "1.el9");
        assert_eq!(parsed.arch, "x86_64");
        assert!(parsed.files.iter().any(|file| file.path == "/usr/bin/demo"));
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

    fn test_rpm_package() -> Vec<u8> {
        let mut builder = rpm::PackageBuilder::new("demo", "1.0.0", "MIT", "x86_64", "Demo");
        builder.release("1.el9").description("Demo package");
        builder
            .with_file_contents("hello", rpm::FileOptions::new("/usr/bin/demo"))
            .unwrap();
        let package = builder.build().unwrap();
        let mut raw = Vec::new();
        package.write(&mut raw).unwrap();
        raw
    }

    fn verify_rpm_signature(raw: &[u8], public_key: &[u8]) -> anyhow::Result<()> {
        let mut reader = BufReader::new(Cursor::new(raw));
        let package = Package::parse(&mut reader)?;
        let verifier = rpm::signature::pgp::Verifier::from_asc_bytes(public_key)?;
        package.verify_signature(&verifier)?;
        Ok(())
    }

    fn primary_metadata_contains_checksum(store: &MemoryStore, checksum: &str) -> bool {
        store
            .keys()
            .into_iter()
            .filter(|key| {
                key.starts_with("rpm/stable/x86_64/repodata/") && key.ends_with("-primary.xml.gz")
            })
            .filter_map(|key| store.body(&key))
            .any(|data| {
                let mut xml = String::new();
                GzDecoder::new(&data[..]).read_to_string(&mut xml).is_ok() && xml.contains(checksum)
            })
    }
}
