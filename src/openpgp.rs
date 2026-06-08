use std::collections::HashSet;
use std::io::Write;
use std::path::Path;
use std::time::Duration;

use anyhow::{Context, bail};
use chrono::{DateTime, Utc};
use sequoia_openpgp as sq;
use sq::cert::{CertParser, prelude::*};
use sq::parse::Parse;
use sq::policy::StandardPolicy;
use sq::serialize::stream::{Armorer, Message, Signer};
use sq::serialize::{Serialize, SerializeInto};

#[derive(Clone)]
pub struct KeyPair {
    cert: sq::Cert,
    pub public_key: String,
    pub key_id: String,
    pub fingerprint: String,
    pub expiration: Option<DateTime<Utc>>,
}

#[derive(Clone)]
pub struct PublicKeyCert {
    cert: sq::Cert,
    pub public_key: String,
    pub key_id: String,
    pub fingerprint: String,
    pub expiration: Option<DateTime<Utc>>,
}

#[derive(Clone, Default)]
pub struct SigningKeys {
    pub active: Option<KeyPair>,
    pub trusted_public_keys: Vec<PublicKeyCert>,
}

impl KeyPair {
    pub fn generate(
        name: &str,
        email: &str,
        comment: &str,
        expiration_years: u32,
    ) -> anyhow::Result<Self> {
        let user_id = if comment.trim().is_empty() {
            format!("{name} <{email}>")
        } else {
            format!("{name} ({comment}) <{email}>")
        };

        let mut builder = CertBuilder::new()
            .set_cipher_suite(CipherSuite::RSA4k)
            .add_userid(user_id)
            .add_signing_subkey();

        let expiration = if expiration_years == 0 {
            None
        } else {
            let duration = Duration::from_secs(expiration_years as u64 * 365 * 24 * 60 * 60);
            builder = builder.set_validity_period(duration);
            Some(Utc::now() + chrono::Duration::seconds(duration.as_secs() as i64))
        };

        let (cert, _revocation) = builder.generate()?;
        Self::from_cert(cert, expiration)
    }

    pub fn load_optional(path: Option<&Path>) -> anyhow::Result<Option<Self>> {
        if let Some(key) = std::env::var("GPG_PRIVATE_KEY")
            .ok()
            .filter(|v| !v.trim().is_empty())
        {
            return Self::from_armored_private_key(&key).map(Some);
        }

        let path = match path {
            Some(path) => path.to_path_buf(),
            None => std::env::current_dir()?.join("signing-key.gpg"),
        };

        if !path.exists() {
            return Ok(None);
        }

        Self::load_from_path(&path).map(Some)
    }

    pub fn load_from_path(path: &Path) -> anyhow::Result<Self> {
        let data = std::fs::read_to_string(path)
            .with_context(|| format!("failed to read signing key {}", path.display()))?;
        Self::from_armored_private_key(&data)
    }

    pub fn from_armored_private_key(data: &str) -> anyhow::Result<Self> {
        if !data
            .trim_start()
            .starts_with("-----BEGIN PGP PRIVATE KEY BLOCK-----")
        {
            bail!("invalid private key format: expected ASCII armored PGP private key");
        }
        let cert = sq::Cert::from_bytes(data.as_bytes())?;
        Self::from_cert(cert, None)
    }

    pub fn save_private_key(&self, path: &Path) -> anyhow::Result<()> {
        let armored = String::from_utf8(self.private_key_armored()?)?;
        std::fs::write(path, armored)
            .with_context(|| format!("failed to write private key {}", path.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
        }
        Ok(())
    }

    pub fn private_key_armored(&self) -> anyhow::Result<Vec<u8>> {
        self.cert.as_tsk().armored().to_vec()
    }

    pub fn public_key_binary(&self) -> anyhow::Result<Vec<u8>> {
        let mut out = Vec::new();
        self.cert.serialize(&mut out)?;
        Ok(out)
    }

    pub fn public_cert(&self) -> anyhow::Result<PublicKeyCert> {
        PublicKeyCert::from_cert(self.cert.clone())
    }

    pub fn sign_detached(&self, data: &[u8]) -> anyhow::Result<String> {
        let policy = StandardPolicy::new();
        let mut signing_keys = self
            .cert
            .keys()
            .with_policy(&policy, None)
            .alive()
            .revoked(false)
            .for_signing()
            .secret()
            .map(|key| key.key().clone().into_keypair())
            .collect::<Result<Vec<_>, _>>()?;

        let keypair = signing_keys
            .pop()
            .context("no suitable signing key found in PGP certificate")?;

        let mut sink = Vec::new();
        {
            let message = Message::new(&mut sink);
            let message = Armorer::new(message)
                .kind(sq::armor::Kind::Signature)
                .build()?;
            let mut signer = Signer::new(message, keypair)?;
            for key in signing_keys {
                signer = signer.add_signer(key)?;
            }
            let mut message = signer.detached().build()?;
            message.write_all(data)?;
            message.finalize()?;
        }

        Ok(String::from_utf8(sink)?)
    }

    fn from_cert(cert: sq::Cert, expiration: Option<DateTime<Utc>>) -> anyhow::Result<Self> {
        let public_cert = cert.clone().strip_secret_key_material();
        let public_key = String::from_utf8(public_cert.armored().to_vec()?)?;
        let key_id = cert.primary_key().key().keyid().to_hex();
        let fingerprint = cert.fingerprint().to_hex();
        let policy = StandardPolicy::new();
        let expiration = expiration.or_else(|| {
            cert.primary_key()
                .with_policy(&policy, None)
                .ok()
                .and_then(|key| key.key_expiration_time())
                .map(DateTime::<Utc>::from)
        });
        Ok(Self {
            cert,
            public_key,
            key_id,
            fingerprint,
            expiration,
        })
    }
}

impl PublicKeyCert {
    pub fn load_many_from_path(path: &Path) -> anyhow::Result<Vec<Self>> {
        let data = std::fs::read(path)
            .with_context(|| format!("failed to read trusted key {}", path.display()))?;
        Self::from_bytes(&data)
            .with_context(|| format!("failed to parse trusted key {}", path.display()))
    }

    pub fn from_bytes(data: &[u8]) -> anyhow::Result<Vec<Self>> {
        let certs = CertParser::from_bytes(data)?
            .map(|cert| cert.and_then(Self::from_cert))
            .collect::<anyhow::Result<Vec<_>>>()?;
        if certs.is_empty() {
            bail!("no OpenPGP public certificates found");
        }
        Ok(certs)
    }

    pub fn to_binary(&self) -> anyhow::Result<Vec<u8>> {
        let mut out = Vec::new();
        self.cert.serialize(&mut out)?;
        Ok(out)
    }

    fn from_cert(cert: sq::Cert) -> anyhow::Result<Self> {
        let cert = cert.strip_secret_key_material();
        let public_key = String::from_utf8(cert.armored().to_vec()?)?;
        let key_id = cert.primary_key().key().keyid().to_hex();
        let fingerprint = cert.fingerprint().to_hex();
        let policy = StandardPolicy::new();
        let expiration = cert
            .primary_key()
            .with_policy(&policy, None)
            .ok()
            .and_then(|key| key.key_expiration_time())
            .map(DateTime::<Utc>::from);
        Ok(Self {
            cert,
            public_key,
            key_id,
            fingerprint,
            expiration,
        })
    }
}

impl SigningKeys {
    pub fn new(active: Option<KeyPair>, trusted_public_keys: Vec<PublicKeyCert>) -> Self {
        let mut seen = HashSet::new();
        if let Some(active) = &active {
            seen.insert(active.fingerprint.clone());
        }

        let trusted_public_keys = trusted_public_keys
            .into_iter()
            .filter(|cert| seen.insert(cert.fingerprint.clone()))
            .collect();

        Self {
            active,
            trusted_public_keys,
        }
    }

    pub fn from_active(active: KeyPair) -> Self {
        Self::new(Some(active), Vec::new())
    }

    pub fn active(&self) -> Option<&KeyPair> {
        self.active.as_ref()
    }

    pub fn public_certs(&self) -> anyhow::Result<Vec<PublicKeyCert>> {
        let mut certs = Vec::new();
        if let Some(active) = &self.active {
            certs.push(active.public_cert()?);
        }
        certs.extend(self.trusted_public_keys.clone());
        Ok(certs)
    }

    pub fn public_key_armored(&self) -> anyhow::Result<Vec<u8>> {
        let mut out = String::new();
        for cert in self.public_certs()? {
            if !out.is_empty() && !out.ends_with('\n') {
                out.push('\n');
            }
            out.push_str(&cert.public_key);
            if !out.ends_with('\n') {
                out.push('\n');
            }
        }
        Ok(out.into_bytes())
    }

    pub fn public_key_binary(&self) -> anyhow::Result<Vec<u8>> {
        let mut out = Vec::new();
        for cert in self.public_certs()? {
            out.extend(cert.to_binary()?);
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn generated_key(name: &str) -> KeyPair {
        KeyPair::generate(name, "hakobin@example.com", "test", 0).unwrap()
    }

    #[test]
    fn public_key_bundle_includes_active_and_trusted_keys() {
        let active = generated_key("Hakobin Active");
        let trusted = generated_key("Hakobin Old").public_cert().unwrap();
        let signing_keys = SigningKeys::new(Some(active), vec![trusted]);

        let armored = String::from_utf8(signing_keys.public_key_armored().unwrap()).unwrap();
        let binary = signing_keys.public_key_binary().unwrap();
        let parsed_armored = PublicKeyCert::from_bytes(armored.as_bytes()).unwrap();
        let binary_count = CertParser::from_bytes(&binary)
            .unwrap()
            .collect::<Result<Vec<_>, _>>()
            .unwrap()
            .len();

        assert_eq!(
            armored
                .matches("-----BEGIN PGP PUBLIC KEY BLOCK-----")
                .count(),
            2
        );
        assert_eq!(parsed_armored.len(), 2);
        assert_eq!(binary_count, 2);
    }

    #[test]
    fn public_key_bundle_deduplicates_active_key() {
        let active = generated_key("Hakobin Active");
        let duplicate = active.public_cert().unwrap();
        let signing_keys = SigningKeys::new(Some(active), vec![duplicate]);

        let armored = String::from_utf8(signing_keys.public_key_armored().unwrap()).unwrap();

        assert_eq!(
            armored
                .matches("-----BEGIN PGP PUBLIC KEY BLOCK-----")
                .count(),
            1
        );
    }
}
