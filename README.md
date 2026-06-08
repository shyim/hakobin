# Hakobin Package

Rust CLI for creating and maintaining DEB/APT and RPM/YUM repositories on S3-compatible storage.

## Build

```bash
cargo build --release
```

The binary is written to `target/release/hakobin`.

## Documentation

See [docs/](docs/) for operational guides:

- [APT repository setup](docs/apt-setup.md)
- [Signing key publication](docs/signing-keys.md)
- [Key rotation](docs/key-rotation.md)

## Configuration

```bash
export AWS_ACCESS_KEY_ID=your-access-key
export AWS_SECRET_ACCESS_KEY=your-secret-key
export S3_BUCKET_NAME=your-bucket-name
export AWS_REGION=us-east-1
```

For MinIO or other S3-compatible stores:

```bash
export S3_ENDPOINT=http://localhost:9000
export S3_USE_PATH_STYLE=true
```

Optional public URL and repository signing configuration:

```bash
export HAKOBIN_PUBLIC_URL=https://packages.example.com
export GPG_PRIVATE_KEY="$(cat signing-key.gpg)"
```

`GPG_PRIVATE_KEY` or the first `--signing-key ./signing-key.gpg` is used for
DEB `Release.gpg`, RPM package, and RPM `repodata/repomd.xml.asc` signing.
Additional `--signing-key` values and any `--trusted-key` values are published
as trusted public keys for rotation, but are not used to sign artifacts.

Key rotation example:

```bash
hakobin \
  --signing-key ./new-signing-key.gpg \
  --trusted-key ./old-pubkey.asc \
  deb rotate-key

hakobin \
  --signing-key ./new-signing-key.gpg \
  --trusted-key ./old-pubkey.asc \
  rpm rotate-key
```

During the overlap window, clients trust both keys through the published DEB
`pubkey.asc`/`pubkey.gpg` or RPM `RPM-GPG-KEY-hakobin.asc` bundle. Remove the
old `--trusted-key` after clients have refreshed their repository keyring.

## DEB Commands

```bash
hakobin deb init
hakobin --signing-key ./signing-key.gpg deb upload ./package.deb --distribution stable --component main
hakobin deb upload --force ./*.deb
hakobin deb list
hakobin deb list nginx --distribution stable --component main
hakobin deb remove nginx --version 1.2.3 --architecture amd64 --force
hakobin --signing-key ./new-signing-key.gpg --trusted-key ./old-pubkey.asc deb rotate-key
```

## RPM Commands

```bash
hakobin --signing-key ./signing-key.gpg rpm init --repo stable --arch x86_64
hakobin --signing-key ./signing-key.gpg rpm upload ./package.rpm --repo stable --arch x86_64
hakobin rpm upload --force ./*.rpm --repo stable --arch x86_64
hakobin rpm list --repo stable --arch x86_64
hakobin rpm remove nginx --epoch 0 --version 1.2.3 --release 1.el9 --arch x86_64 --repo stable --repo-arch x86_64 --force
hakobin --signing-key ./new-signing-key.gpg --trusted-key ./old-pubkey.asc rpm rotate-key
```

RPM package signing is supported for DNF/YUM `gpgcheck=1`, and repository
metadata signing is supported for `repo_gpgcheck=1`.

Example `.repo` file:

```ini
[hakobin-stable]
name=Hakobin stable
baseurl=https://packages.example.com/rpm/stable/x86_64
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://packages.example.com/rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc
```

## Repository Layout

Hakobin stores format-specific repositories under separate bucket prefixes:

```text
bucket/
├── deb/
│   ├── apt-repo.json
│   ├── dists/
│   ├── pool/
│   ├── pubkey.asc
│   ├── pubkey.gpg
│   └── setup.sh
└── rpm/
    └── stable/
        └── x86_64/
            ├── Packages/
            │   └── package-1.2.3-1.el9.x86_64.rpm
            ├── RPM-GPG-KEY-hakobin.asc
            └── repodata/
                ├── repomd.xml
                ├── repomd.xml.asc
                ├── <checksum>-primary.xml.gz
                ├── <checksum>-filelists.xml.gz
                └── <checksum>-other.xml.gz
```
