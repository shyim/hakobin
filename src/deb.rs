use std::collections::BTreeMap;
use std::io::{Cursor, Read};
use std::path::Path;

use anyhow::{Context, bail};
use bzip2::read::BzDecoder;
use flate2::read::GzDecoder;
use md5::Md5;
use sha1::Sha1;
use sha2::{Digest, Sha256};
use tar::Archive;
use xz2::read::XzDecoder;
use zstd::stream::read::Decoder as ZstdDecoder;

use crate::apt::parse_control_paragraphs;

#[derive(Debug, Clone)]
pub struct DebPackage {
    pub filename: String,
    pub size: u64,
    pub md5: String,
    pub sha1: String,
    pub sha256: String,
    pub control: BTreeMap<String, String>,
    pub raw: Vec<u8>,
}

impl DebPackage {
    pub fn from_path(path: &Path) -> anyhow::Result<Self> {
        let raw =
            std::fs::read(path).with_context(|| format!("failed to read {}", path.display()))?;
        let filename = path
            .file_name()
            .and_then(|f| f.to_str())
            .context("package path has no UTF-8 filename")?
            .to_string();
        Self::parse(filename, raw)
    }

    pub fn parse(filename: String, raw: Vec<u8>) -> anyhow::Result<Self> {
        let size = raw.len() as u64;
        let md5 = hex::encode(Md5::digest(&raw));
        let sha1 = hex::encode(Sha1::digest(&raw));
        let sha256 = hex::encode(Sha256::digest(&raw));
        let control = extract_control(&raw)?;

        Ok(Self {
            filename,
            size,
            md5,
            sha1,
            sha256,
            control,
            raw,
        })
    }

    pub fn package(&self) -> anyhow::Result<&str> {
        self.control
            .get("Package")
            .map(String::as_str)
            .context("package does not specify Package")
    }

    pub fn version(&self) -> anyhow::Result<&str> {
        self.control
            .get("Version")
            .map(String::as_str)
            .context("package does not specify Version")
    }

    pub fn architecture(&self) -> anyhow::Result<&str> {
        self.control
            .get("Architecture")
            .map(String::as_str)
            .context("package does not specify Architecture")
    }

    pub fn generated_filename(&self) -> anyhow::Result<String> {
        Ok(format!(
            "{}_{}_{}.deb",
            self.package()?,
            self.version()?,
            self.architecture()?
        ))
    }

    pub fn package_entry(&self, pool_path: &str) -> String {
        let mut out = String::new();
        for (key, value) in &self.control {
            out.push_str(&format!("{key}: {value}\n"));
        }
        out.push_str(&format!("Filename: {pool_path}\n"));
        out.push_str(&format!("Size: {}\n", self.size));
        out.push_str(&format!("MD5sum: {}\n", self.md5));
        out.push_str(&format!("SHA1: {}\n", self.sha1));
        out.push_str(&format!("SHA256: {}\n", self.sha256));
        out
    }
}

fn extract_control(data: &[u8]) -> anyhow::Result<BTreeMap<String, String>> {
    let mut reader = ArReader::new(data)?;
    while let Some(entry) = reader.next_entry()? {
        if entry.name.starts_with("control.tar") {
            return extract_control_from_tar(&entry.data, &entry.name);
        }
    }
    bail!("control.tar not found in deb package")
}

fn extract_control_from_tar(data: &[u8], name: &str) -> anyhow::Result<BTreeMap<String, String>> {
    let tar_data = decompress_control_tar(data, name)?;
    let mut archive = Archive::new(Cursor::new(tar_data));

    for entry in archive.entries()? {
        let mut entry = entry?;
        let path = entry.path()?;
        if path == Path::new("./control") || path == Path::new("control") {
            let mut control = String::new();
            entry.read_to_string(&mut control)?;
            let paragraphs = parse_control_paragraphs(&control)?;
            return paragraphs
                .into_iter()
                .next()
                .context("control file did not contain a paragraph");
        }
    }

    bail!("control file not found in {name}")
}

fn decompress_control_tar(data: &[u8], name: &str) -> anyhow::Result<Vec<u8>> {
    if name.ends_with(".gz") || data.starts_with(&[0x1f, 0x8b]) {
        read_all(GzDecoder::new(Cursor::new(data)))
    } else if name.ends_with(".xz") || data.starts_with(&[0xfd, b'7', b'z', b'X', b'Z']) {
        read_all(XzDecoder::new(Cursor::new(data)))
    } else if name.ends_with(".zst") || data.starts_with(&[0x28, 0xb5, 0x2f, 0xfd]) {
        read_all(ZstdDecoder::new(Cursor::new(data))?)
    } else if name.ends_with(".bz2") || data.starts_with(b"BZh") {
        read_all(BzDecoder::new(Cursor::new(data)))
    } else {
        Ok(data.to_vec())
    }
}

fn read_all(mut reader: impl Read) -> anyhow::Result<Vec<u8>> {
    let mut out = Vec::new();
    reader.read_to_end(&mut out)?;
    Ok(out)
}

struct ArReader<'a> {
    data: &'a [u8],
    pos: usize,
}

struct ArEntry {
    name: String,
    data: Vec<u8>,
}

impl<'a> ArReader<'a> {
    fn new(data: &'a [u8]) -> anyhow::Result<Self> {
        if !data.starts_with(b"!<arch>\n") {
            bail!("not an ar archive");
        }
        Ok(Self { data, pos: 8 })
    }

    fn next_entry(&mut self) -> anyhow::Result<Option<ArEntry>> {
        if self.pos >= self.data.len() {
            return Ok(None);
        }
        if self.pos + 60 > self.data.len() {
            bail!("truncated ar header");
        }

        let header = &self.data[self.pos..self.pos + 60];
        self.pos += 60;

        if &header[58..60] != b"`\n" {
            bail!("invalid ar header ending");
        }

        let name = std::str::from_utf8(&header[0..16])?
            .trim()
            .trim_end_matches('/')
            .to_string();
        let size: usize = std::str::from_utf8(&header[48..58])?
            .trim()
            .parse()
            .context("invalid ar member size")?;

        if self.pos + size > self.data.len() {
            bail!("truncated ar member {name}");
        }

        let entry_data = self.data[self.pos..self.pos + size].to_vec();
        self.pos += size;
        if size % 2 == 1 && self.pos < self.data.len() {
            self.pos += 1;
        }

        Ok(Some(ArEntry {
            name,
            data: entry_data,
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flate2::Compression;
    use flate2::write::GzEncoder;
    use std::io::Write;
    use tar::{Builder, Header};

    #[test]
    fn parses_gzipped_control_tar() {
        let deb = test_deb(
            b"Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nDescription: Demo package\n",
        );
        let package = DebPackage::parse("demo.deb".to_string(), deb).unwrap();
        assert_eq!(package.package().unwrap(), "demo");
        assert_eq!(package.version().unwrap(), "1.0.0");
        assert_eq!(package.architecture().unwrap(), "amd64");
        assert_eq!(
            package.generated_filename().unwrap(),
            "demo_1.0.0_amd64.deb"
        );
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
