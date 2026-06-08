use std::collections::BTreeMap;
use std::io::Write;

use anyhow::{Context, bail};
use chrono::{DateTime, Utc};
use flate2::Compression;
use flate2::write::GzEncoder;
use md5::Md5;
use sha1::Sha1;
use sha2::{Digest, Sha256};

#[derive(Debug, Clone)]
pub struct RepositoryPath {
    pub distribution: String,
    pub component: String,
    pub architecture: String,
}

impl RepositoryPath {
    pub fn packages_path(&self) -> String {
        format!(
            "dists/{}/{}/binary-{}/Packages",
            self.distribution, self.component, self.architecture
        )
    }

    pub fn packages_gz_path(&self) -> String {
        format!("{}.gz", self.packages_path())
    }

    pub fn release_path(&self) -> String {
        format!("dists/{}/Release", self.distribution)
    }

    pub fn pool_path(&self, package: &str) -> String {
        let prefix = if package.starts_with("lib") && package.len() > 3 {
            &package[..4]
        } else {
            &package[..1]
        };
        format!("pool/{}/{}/{}", self.component, prefix, package)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PackageEntry {
    pub package: String,
    pub version: String,
    pub architecture: String,
    pub filename: String,
    pub size: u64,
    pub md5sum: String,
    pub sha1: String,
    pub sha256: String,
    pub maintainer: Option<String>,
    pub description: Option<String>,
    pub section: Option<String>,
    pub priority: Option<String>,
    pub installed_size: Option<u64>,
    pub depends: Option<String>,
    pub recommends: Option<String>,
    pub suggests: Option<String>,
    pub conflicts: Option<String>,
    pub breaks: Option<String>,
    pub replaces: Option<String>,
    pub provides: Option<String>,
    pub homepage: Option<String>,
    pub extra: BTreeMap<String, String>,
}

impl PackageEntry {
    pub fn from_fields(fields: &BTreeMap<String, String>) -> anyhow::Result<Self> {
        let package = required(fields, "Package")?.to_string();
        let version = required(fields, "Version")?.to_string();
        let architecture = required(fields, "Architecture")?.to_string();
        let filename = required(fields, "Filename")?.to_string();
        let size = required(fields, "Size")?
            .parse()
            .with_context(|| format!("invalid Size field for {package}"))?;
        let md5sum = required(fields, "MD5sum")?.to_string();
        let sha1 = required(fields, "SHA1")?.to_string();
        let sha256 = required(fields, "SHA256")?.to_string();

        let known = [
            "Package",
            "Version",
            "Architecture",
            "Filename",
            "Size",
            "MD5sum",
            "SHA1",
            "SHA256",
            "Maintainer",
            "Description",
            "Section",
            "Priority",
            "Installed-Size",
            "Depends",
            "Recommends",
            "Suggests",
            "Conflicts",
            "Breaks",
            "Replaces",
            "Provides",
            "Homepage",
        ];

        let extra = fields
            .iter()
            .filter(|(key, value)| !known.contains(&key.as_str()) && !value.is_empty())
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect();

        Ok(Self {
            package,
            version,
            architecture,
            filename,
            size,
            md5sum,
            sha1,
            sha256,
            maintainer: fields.get("Maintainer").cloned(),
            description: fields.get("Description").cloned(),
            section: fields.get("Section").cloned(),
            priority: fields.get("Priority").cloned(),
            installed_size: fields
                .get("Installed-Size")
                .map(|value| value.parse())
                .transpose()
                .context("invalid Installed-Size field")?,
            depends: fields.get("Depends").cloned(),
            recommends: fields.get("Recommends").cloned(),
            suggests: fields.get("Suggests").cloned(),
            conflicts: fields.get("Conflicts").cloned(),
            breaks: fields.get("Breaks").cloned(),
            replaces: fields.get("Replaces").cloned(),
            provides: fields.get("Provides").cloned(),
            homepage: fields.get("Homepage").cloned(),
            extra,
        })
    }
}

#[derive(Debug, Clone)]
pub struct ReleaseFile {
    pub origin: String,
    pub label: String,
    pub description: String,
    pub distribution: String,
    pub components: Vec<String>,
    pub architectures: Vec<String>,
    pub date: DateTime<Utc>,
    pub files: Vec<ReleaseFileEntry>,
}

#[derive(Debug, Clone)]
pub struct ReleaseFileEntry {
    pub filename: String,
    pub size: usize,
    pub md5: String,
    pub sha1: String,
    pub sha256: String,
}

impl ReleaseFile {
    pub fn generate(&self) -> Vec<u8> {
        let mut out = String::new();
        out.push_str(&format!(
            "Origin: {}\n",
            defaulted(&self.origin, "APT S3 Repository")
        ));
        out.push_str(&format!(
            "Label: {}\n",
            defaulted(&self.label, "APT S3 Repository")
        ));
        out.push_str(&format!("Suite: {}\n", self.distribution));
        out.push_str(&format!("Codename: {}\n", self.distribution));
        out.push_str(&format!(
            "Date: {}\n",
            self.date.format("%a, %d %b %Y %H:%M:%S UTC")
        ));
        out.push_str(&format!(
            "Architectures: {}\n",
            self.architectures.join(" ")
        ));
        out.push_str(&format!("Components: {}\n", self.components.join(" ")));
        out.push_str(&format!(
            "Description: {}\n",
            defaulted(&self.description, "APT repository hosted on S3")
        ));

        if !self.files.is_empty() {
            out.push_str("MD5Sum:\n");
            for file in &self.files {
                out.push_str(&format!(
                    " {} {:16} {}\n",
                    file.md5, file.size, file.filename
                ));
            }
            out.push_str("SHA1:\n");
            for file in &self.files {
                out.push_str(&format!(
                    " {} {:16} {}\n",
                    file.sha1, file.size, file.filename
                ));
            }
            out.push_str("SHA256:\n");
            for file in &self.files {
                out.push_str(&format!(
                    " {} {:16} {}\n",
                    file.sha256, file.size, file.filename
                ));
            }
        }

        out.into_bytes()
    }
}

pub fn parse_control_paragraphs(content: &str) -> anyhow::Result<Vec<BTreeMap<String, String>>> {
    if content.trim().is_empty() {
        return Ok(Vec::new());
    }

    let normalized = content.replace("\r\n", "\n");
    normalized
        .split("\n\n")
        .filter(|p| !p.trim().is_empty())
        .map(parse_paragraph)
        .collect()
}

pub fn parse_packages(content: &str) -> anyhow::Result<Vec<PackageEntry>> {
    parse_control_paragraphs(content)?
        .iter()
        .map(PackageEntry::from_fields)
        .collect()
}

pub fn generate_packages_content(packages: &[PackageEntry]) -> String {
    if packages.is_empty() {
        return String::new();
    }

    let entries = packages
        .iter()
        .map(generate_package_entry)
        .collect::<Vec<_>>();
    format!("{}\n\n", entries.join("\n\n"))
}

pub fn compress_gzip(data: &[u8]) -> anyhow::Result<Vec<u8>> {
    let mut encoder = GzEncoder::new(Vec::new(), Compression::default());
    encoder.write_all(data)?;
    Ok(encoder.finish()?)
}

pub fn calculate_hashes(data: &[u8]) -> (String, String, String) {
    let md5 = hex::encode(Md5::digest(data));
    let sha1 = hex::encode(Sha1::digest(data));
    let sha256 = hex::encode(Sha256::digest(data));
    (md5, sha1, sha256)
}

fn parse_paragraph(paragraph: &str) -> anyhow::Result<BTreeMap<String, String>> {
    let mut fields = BTreeMap::<String, String>::new();
    let mut current_key: Option<String> = None;

    for raw_line in paragraph.lines() {
        if raw_line.trim().is_empty() {
            continue;
        }

        if raw_line.starts_with(' ') || raw_line.starts_with('\t') {
            let key = current_key
                .as_ref()
                .context("control continuation line without a preceding field")?;
            let value = fields.get_mut(key).expect("current key exists");
            value.push('\n');
            value.push_str(raw_line);
            continue;
        }

        let (key, value) = raw_line
            .split_once(':')
            .with_context(|| format!("invalid control field line: {raw_line}"))?;
        let key = key.trim();
        if key.is_empty() {
            bail!("empty control field name");
        }
        let key = key.to_string();
        fields.insert(key.clone(), value.trim_start().to_string());
        current_key = Some(key);
    }

    Ok(fields)
}

fn generate_package_entry(pkg: &PackageEntry) -> String {
    let mut lines = vec![
        format!("Package: {}", pkg.package),
        format!("Version: {}", pkg.version),
        format!("Architecture: {}", pkg.architecture),
    ];

    push_opt(&mut lines, "Maintainer", &pkg.maintainer);
    if let Some(installed_size) = pkg.installed_size {
        lines.push(format!("Installed-Size: {installed_size}"));
    }
    push_opt(&mut lines, "Depends", &pkg.depends);
    push_opt(&mut lines, "Recommends", &pkg.recommends);
    push_opt(&mut lines, "Suggests", &pkg.suggests);
    push_opt(&mut lines, "Conflicts", &pkg.conflicts);
    push_opt(&mut lines, "Breaks", &pkg.breaks);
    push_opt(&mut lines, "Replaces", &pkg.replaces);
    push_opt(&mut lines, "Provides", &pkg.provides);
    push_opt(&mut lines, "Section", &pkg.section);
    push_opt(&mut lines, "Priority", &pkg.priority);
    push_opt(&mut lines, "Homepage", &pkg.homepage);

    for (key, value) in &pkg.extra {
        lines.push(format!("{key}: {value}"));
    }

    lines.push(format!("Filename: {}", pkg.filename));
    lines.push(format!("Size: {}", pkg.size));
    lines.push(format!("MD5sum: {}", pkg.md5sum));
    lines.push(format!("SHA1: {}", pkg.sha1));
    lines.push(format!("SHA256: {}", pkg.sha256));
    push_opt(&mut lines, "Description", &pkg.description);

    lines.join("\n")
}

fn push_opt(lines: &mut Vec<String>, key: &str, value: &Option<String>) {
    if let Some(value) = value.as_ref().filter(|v| !v.is_empty()) {
        lines.push(format!("{key}: {value}"));
    }
}

fn required<'a>(fields: &'a BTreeMap<String, String>, key: &str) -> anyhow::Result<&'a str> {
    fields
        .get(key)
        .map(String::as_str)
        .filter(|v| !v.is_empty())
        .with_context(|| format!("package entry missing required '{key}' field"))
}

fn defaulted<'a>(value: &'a str, default: &'a str) -> &'a str {
    if value.is_empty() { default } else { value }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pool_path_uses_lib_prefix() {
        let repo = RepositoryPath {
            distribution: "stable".into(),
            component: "main".into(),
            architecture: "amd64".into(),
        };
        assert_eq!(repo.pool_path("nginx"), "pool/main/n/nginx");
        assert_eq!(repo.pool_path("libssl"), "pool/main/libs/libssl");
    }

    #[test]
    fn parses_and_generates_packages() {
        let content = "\
Package: pkg
Version: 1.0
Architecture: amd64
Filename: pool/main/p/pkg/pkg_1.0_amd64.deb
Size: 12
MD5sum: d41d8cd98f00b204e9800998ecf8427e
SHA1: da39a3ee5e6b4b0d3255bfef95601890afd80709
SHA256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855

";
        let packages = parse_packages(content).unwrap();
        assert_eq!(packages.len(), 1);
        assert_eq!(packages[0].package, "pkg");
        assert!(generate_packages_content(&packages).contains("Package: pkg"));
    }
}
