use std::path::PathBuf;

use clap::{Args, Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(name = "hakobin")]
#[command(about = "Hakobin Package manages DEB and RPM repositories on S3-compatible storage")]
pub struct Cli {
    #[arg(long, global = true)]
    pub signing_key: Vec<PathBuf>,

    #[arg(long, global = true)]
    pub trusted_key: Vec<PathBuf>,

    #[arg(long, global = true, env = "HAKOBIN_PUBLIC_URL")]
    pub public_url: Option<String>,

    #[command(subcommand)]
    pub command: FormatCommands,
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::Parser;

    #[test]
    fn parses_repeated_signing_and_trusted_keys() {
        let cli = Cli::parse_from([
            "hakobin",
            "--signing-key",
            "new.gpg",
            "--signing-key",
            "old.gpg",
            "--trusted-key",
            "old-public.asc",
            "rpm",
            "list",
        ]);

        assert_eq!(
            cli.signing_key,
            vec![PathBuf::from("new.gpg"), PathBuf::from("old.gpg")]
        );
        assert_eq!(cli.trusted_key, vec![PathBuf::from("old-public.asc")]);
    }

    #[test]
    fn parses_rotate_key_commands() {
        assert!(matches!(
            Cli::parse_from(["hakobin", "deb", "rotate-key"]).command,
            FormatCommands::Deb(DebCommand {
                command: DebCommands::RotateKey
            })
        ));
        assert!(matches!(
            Cli::parse_from(["hakobin", "rpm", "rotate-key"]).command,
            FormatCommands::Rpm(RpmCommand {
                command: RpmCommands::RotateKey
            })
        ));
    }
}

#[derive(Debug, Subcommand)]
pub enum FormatCommands {
    #[command(about = "Manage Debian APT repositories")]
    Deb(DebCommand),

    #[command(about = "Manage RPM YUM/DNF repositories")]
    Rpm(RpmCommand),
}

#[derive(Debug, Args)]
pub struct DebCommand {
    #[command(subcommand)]
    pub command: DebCommands,
}

#[derive(Debug, Subcommand)]
pub enum DebCommands {
    #[command(about = "Initialize a new APT repository")]
    Init(DebInitArgs),

    #[command(about = "Upload one or more .deb packages")]
    Upload(DebUploadArgs),

    #[command(about = "List packages in the APT repository")]
    List(DebListArgs),

    #[command(about = "Remove a package from the APT repository")]
    Remove(DebRemoveArgs),

    #[command(about = "Refresh APT signing keys and re-sign repository metadata")]
    RotateKey,
}

#[derive(Debug, Args)]
pub struct DebInitArgs {
    #[arg(long)]
    pub origin: Option<String>,

    #[arg(long)]
    pub label: Option<String>,

    #[arg(long)]
    pub description: Option<String>,

    #[arg(long, value_delimiter = ',')]
    pub distributions: Option<Vec<String>>,

    #[arg(long, value_delimiter = ',')]
    pub components: Option<Vec<String>>,

    #[arg(long, value_delimiter = ',')]
    pub architectures: Option<Vec<String>>,

    #[arg(long)]
    pub key_name: Option<String>,

    #[arg(long)]
    pub key_email: Option<String>,

    #[arg(long, default_value_t = 2)]
    pub key_expiration_years: u32,
}

#[derive(Debug, Args)]
pub struct DebUploadArgs {
    #[arg(required = true)]
    pub deb_files: Vec<PathBuf>,

    #[arg(short, long, default_value = "stable")]
    pub distribution: String,

    #[arg(short, long, default_value = "main")]
    pub component: String,

    #[arg(short, long)]
    pub force: bool,
}

#[derive(Debug, Args)]
pub struct DebListArgs {
    pub package_name: Option<String>,

    #[arg(short, long)]
    pub distribution: Option<String>,

    #[arg(short, long)]
    pub component: Option<String>,

    #[arg(short = 'p', long = "package")]
    pub package_filter: Option<String>,
}

#[derive(Debug, Args)]
pub struct DebRemoveArgs {
    pub package: String,

    #[arg(short, long)]
    pub version: String,

    #[arg(short, long)]
    pub architecture: String,

    #[arg(short, long, default_value = "stable")]
    pub distribution: String,

    #[arg(short, long, default_value = "main")]
    pub component: String,

    #[arg(short, long)]
    pub force: bool,
}

#[derive(Debug, Args)]
pub struct RpmCommand {
    #[command(subcommand)]
    pub command: RpmCommands,
}

#[derive(Debug, Subcommand)]
pub enum RpmCommands {
    #[command(about = "Initialize a new RPM YUM/DNF repository")]
    Init(RpmInitArgs),

    #[command(about = "Upload one or more .rpm packages")]
    Upload(RpmUploadArgs),

    #[command(about = "List packages in an RPM repository")]
    List(RpmListArgs),

    #[command(about = "Remove a package from an RPM repository")]
    Remove(RpmRemoveArgs),

    #[command(about = "Refresh RPM signing keys and re-sign packages and repository metadata")]
    RotateKey,
}

#[derive(Debug, Args)]
pub struct RpmInitArgs {
    #[arg(long, default_value = "stable")]
    pub repo: String,

    #[arg(long, default_value = "x86_64")]
    pub arch: String,
}

#[derive(Debug, Args)]
pub struct RpmUploadArgs {
    #[arg(required = true)]
    pub rpm_files: Vec<PathBuf>,

    #[arg(long, default_value = "stable")]
    pub repo: String,

    #[arg(long, default_value = "x86_64")]
    pub arch: String,

    #[arg(short, long)]
    pub force: bool,
}

#[derive(Debug, Args)]
pub struct RpmListArgs {
    pub package_name: Option<String>,

    #[arg(long, default_value = "stable")]
    pub repo: String,

    #[arg(long, default_value = "x86_64")]
    pub arch: String,

    #[arg(short = 'p', long = "package")]
    pub package_filter: Option<String>,
}

#[derive(Debug, Args)]
pub struct RpmRemoveArgs {
    pub package: String,

    #[arg(long, default_value = "0")]
    pub epoch: String,

    #[arg(short, long)]
    pub version: String,

    #[arg(short = 'r', long)]
    pub release: String,

    #[arg(long)]
    pub arch: String,

    #[arg(long, default_value = "stable")]
    pub repo: String,

    #[arg(long, default_value = "x86_64")]
    pub repo_arch: String,

    #[arg(short, long)]
    pub force: bool,
}
