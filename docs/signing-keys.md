# Signing Key Publication

Hakobin signs repository metadata with one active private OpenPGP key and publishes public keys so clients can verify the metadata. For RPM repositories, the same active key also signs uploaded package files.

## Active Signing Key

For mutating commands, Hakobin chooses the active private key in this order:

1. `GPG_PRIVATE_KEY`, if set.
2. The first `--signing-key <PATH>`, if provided.
3. `./signing-key.gpg`, if it exists.

Examples:

```bash
hakobin --signing-key ./signing-key.gpg deb upload ./package.deb
```

```bash
export GPG_PRIVATE_KEY="$(cat signing-key.gpg)"
hakobin deb upload ./package.deb
```

If no active key can be loaded, Hakobin keeps the current behavior: it warns and continues without writing signature files.

## Published Keys

APT repositories publish:

- `deb/pubkey.asc`: armored public key bundle for manual import.
- `deb/pubkey.gpg`: binary public key bundle for `/etc/apt/keyrings`.

RPM repositories publish:

- `rpm/<repo>/<arch>/RPM-GPG-KEY-hakobin.asc`: armored public key bundle for `gpgkey=`.

These files are refreshed whenever Hakobin writes signed metadata:

```bash
hakobin --signing-key ./signing-key.gpg deb upload ./package.deb
hakobin --signing-key ./signing-key.gpg rpm upload ./package.rpm --repo stable --arch x86_64
hakobin --signing-key ./signing-key.gpg deb rotate-key
hakobin --signing-key ./signing-key.gpg rpm rotate-key
```

APT clients use `deb/pubkey.gpg` through `signed-by=`.

DNF/YUM clients use `RPM-GPG-KEY-hakobin.asc` with package and repository metadata verification:

```ini
[hakobin-stable]
name=Hakobin stable
baseurl=https://packages.example.com/rpm/stable/x86_64
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://packages.example.com/rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc
```

`gpgcheck=1` verifies RPM package signatures. `repo_gpgcheck=1` verifies repository metadata signatures.

## Trusted Key Bundle

For key rotation, Hakobin can publish extra public certificates in the same key bundle.

Use additional `--signing-key` values or `--trusted-key`:

```bash
hakobin \
  --signing-key ./new-signing-key.gpg \
  --trusted-key ./old-pubkey.asc \
  deb rotate-key
```

Only the active key signs metadata and RPM packages. Extra keys are published for clients to trust during a rotation window.

`--trusted-key` may point to an armored public key, an armored private key, or a keyring containing multiple certificates. Hakobin publishes only public certificate material.

`deb rotate-key` refreshes APT key bundles, regenerates `setup.sh`, and re-signs all APT distributions. `rpm rotate-key` refreshes key bundles, re-signs every RPM package object, and re-signs every discovered RPM repo/arch under the `rpm/` S3 prefix.
