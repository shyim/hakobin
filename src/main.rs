use std::path::PathBuf;

use anyhow::bail;
use chrono::Utc;
use clap::Parser;
use hakobin::cli::{Cli, DebCommands, FormatCommands, RpmCommands};
use hakobin::config::Config;
use hakobin::openpgp::{KeyPair, PublicKeyCert, SigningKeys};
use hakobin::repository::{InitRequest, RemoveRequest, RepositoryManager, UploadRequest};
use hakobin::rpm_repo::{
    RpmInitRequest, RpmListRequest, RpmRemoveRequest, RpmRepositoryManager, RpmUploadRequest,
};

#[tokio::main]
async fn main() {
    if let Err(err) = run().await {
        eprintln!("Error: {err:#}");
        std::process::exit(1);
    }
}

async fn run() -> anyhow::Result<()> {
    let cli = Cli::parse();
    let mut cfg = Config::from_env();
    if cfg.public_url.is_none()
        && let Some(public_url) = &cli.public_url
    {
        cfg.public_url = Some(public_url.clone());
    }
    let signing_key = cli.signing_key;
    let trusted_key = cli.trusted_key;

    match cli.command {
        FormatCommands::Deb(command) => {
            run_deb(command.command, cfg, &signing_key, &trusted_key).await
        }
        FormatCommands::Rpm(command) => {
            run_rpm(command.command, cfg, &signing_key, &trusted_key).await
        }
    }
}

async fn run_deb(
    command: DebCommands,
    cfg: Config,
    signing_key_paths: &[PathBuf],
    trusted_key_paths: &[PathBuf],
) -> anyhow::Result<()> {
    let manager = RepositoryManager::connect(cfg.clone()).await?;

    match command {
        DebCommands::Init(args) => {
            let request = InitRequest::from_args(args, &cfg)?;
            manager.init(request).await?;
        }
        DebCommands::Upload(args) => {
            cfg.require_s3()?;
            let signing_keys = load_signing_keys(signing_key_paths, trusted_key_paths);
            manager
                .upload(UploadRequest {
                    deb_files: args.deb_files,
                    distribution: args.distribution,
                    component: args.component,
                    force: args.force,
                    signing_keys,
                })
                .await?;
        }
        DebCommands::List(args) => {
            cfg.require_s3()?;
            let package_name = args.package_name.or(args.package_filter);
            manager
                .list_packages(args.distribution, args.component, package_name)
                .await?;
        }
        DebCommands::Remove(args) => {
            cfg.require_s3()?;
            let signing_keys = load_signing_keys(signing_key_paths, trusted_key_paths);
            manager
                .remove(RemoveRequest {
                    package: args.package,
                    version: args.version,
                    architecture: args.architecture,
                    distribution: args.distribution,
                    component: args.component,
                    force: args.force,
                    signing_keys,
                })
                .await?;
        }
        DebCommands::RotateKey => {
            cfg.require_s3()?;
            let signing_keys = load_required_signing_keys(signing_key_paths, trusted_key_paths)?;
            manager.rotate_key(signing_keys).await?;
        }
    }

    Ok(())
}

async fn run_rpm(
    command: RpmCommands,
    cfg: Config,
    signing_key_paths: &[PathBuf],
    trusted_key_paths: &[PathBuf],
) -> anyhow::Result<()> {
    cfg.require_s3()?;
    let manager = RpmRepositoryManager::connect(cfg).await?;

    match command {
        RpmCommands::Init(args) => {
            let signing_keys = load_signing_keys(signing_key_paths, trusted_key_paths);
            manager
                .init(RpmInitRequest {
                    repo: args.repo,
                    arch: args.arch,
                    signing_keys,
                })
                .await?;
        }
        RpmCommands::Upload(args) => {
            let signing_keys = load_signing_keys(signing_key_paths, trusted_key_paths);
            manager
                .upload(RpmUploadRequest {
                    rpm_files: args.rpm_files,
                    repo: args.repo,
                    arch: args.arch,
                    force: args.force,
                    signing_keys,
                })
                .await?;
        }
        RpmCommands::List(args) => {
            let package_name = args.package_name.or(args.package_filter);
            manager
                .list(RpmListRequest {
                    repo: args.repo,
                    arch: args.arch,
                    package_name,
                })
                .await?;
        }
        RpmCommands::Remove(args) => {
            let signing_keys = load_signing_keys(signing_key_paths, trusted_key_paths);
            manager
                .remove(RpmRemoveRequest {
                    package: args.package,
                    epoch: args.epoch,
                    version: args.version,
                    release: args.release,
                    arch: args.arch,
                    repo: args.repo,
                    repo_arch: args.repo_arch,
                    force: args.force,
                    signing_keys,
                })
                .await?;
        }
        RpmCommands::RotateKey => {
            let signing_keys = load_required_signing_keys(signing_key_paths, trusted_key_paths)?;
            manager.rotate_key(signing_keys).await?;
        }
    }

    Ok(())
}

fn load_signing_keys(signing_key_paths: &[PathBuf], trusted_key_paths: &[PathBuf]) -> SigningKeys {
    let env_key = std::env::var("GPG_PRIVATE_KEY")
        .ok()
        .filter(|value| !value.trim().is_empty());
    let env_key_present = env_key.is_some();
    let mut active_source = None;
    let active = if let Some(key) = env_key {
        match KeyPair::from_armored_private_key(&key) {
            Ok(key_pair) => {
                active_source = Some("environment variable".to_string());
                Some(key_pair)
            }
            Err(err) => {
                eprintln!("Warning: Failed to load signing key: {err:#}");
                None
            }
        }
    } else {
        let active_path = signing_key_paths.first().cloned().or_else(|| {
            std::env::current_dir()
                .map(|dir| dir.join("signing-key.gpg"))
                .map_err(|err| eprintln!("Warning: Failed to resolve signing key path: {err:#}"))
                .ok()
        });

        match active_path {
            Some(path) if path.exists() => match KeyPair::load_from_path(&path) {
                Ok(key_pair) => {
                    active_source = Some(format!("file {}", path.display()));
                    Some(key_pair)
                }
                Err(err) => {
                    eprintln!("Warning: Failed to load signing key: {err:#}");
                    None
                }
            },
            Some(_) | None => None,
        }
    };

    let mut trusted_public_keys = Vec::new();
    let extra_signing_keys = if env_key_present {
        signing_key_paths
    } else {
        signing_key_paths.get(1..).unwrap_or_default()
    };

    for path in extra_signing_keys.iter().chain(trusted_key_paths) {
        match PublicKeyCert::load_many_from_path(path) {
            Ok(keys) => trusted_public_keys.extend(keys),
            Err(err) => eprintln!(
                "Warning: Failed to load trusted signing key {}: {err:#}",
                path.display()
            ),
        }
    }

    let signing_keys = SigningKeys::new(active, trusted_public_keys);
    print_signing_key_status(&signing_keys, active_source.as_deref());
    signing_keys
}

fn load_required_signing_keys(
    signing_key_paths: &[PathBuf],
    trusted_key_paths: &[PathBuf],
) -> anyhow::Result<SigningKeys> {
    let signing_keys = load_signing_keys(signing_key_paths, trusted_key_paths);
    if signing_keys.active().is_none() {
        bail!(
            "rotate-key requires an active private signing key from GPG_PRIVATE_KEY, --signing-key, or ./signing-key.gpg"
        );
    }
    Ok(signing_keys)
}

fn print_signing_key_status(signing_keys: &SigningKeys, active_source: Option<&str>) {
    if let Some(key_pair) = signing_keys.active() {
        println!(
            "Using GPG signing key: {} (loaded from {}){}",
            key_pair.key_id,
            active_source.unwrap_or("file"),
            expiration_status(key_pair.expiration)
        );
    }

    if !signing_keys.trusted_public_keys.is_empty() {
        if signing_keys.active().is_none() {
            println!("Trusted public signing keys loaded, but no active signing key is available:");
        } else {
            println!("Trusted public signing keys:");
        }
        for key in &signing_keys.trusted_public_keys {
            println!("  - {}{}", key.key_id, expiration_status(key.expiration));
        }
    }
}

fn expiration_status(expiration: Option<chrono::DateTime<Utc>>) -> String {
    if expiration.is_some_and(|expiration| expiration <= Utc::now()) {
        " [EXPIRED]".to_string()
    } else if let Some(expiration) = expiration {
        format!(" [expires {}]", expiration.format("%Y-%m-%d"))
    } else {
        String::new()
    }
}
