# Key Rotation

Hakobin supports key rotation by signing metadata with one active private key while publishing a public key bundle that contains both the new and old trusted keys.

This gives clients time to refresh their keyring before the old key is removed.

## Rotation Model

- The active private key signs `Release.gpg` for APT, RPM package files, and `repomd.xml.asc` for RPM repository metadata.
- Trusted keys are not used for signing.
- Active and trusted public certs are published together in the client key bundle.
- Duplicate keys are de-duplicated by OpenPGP fingerprint.
- `rotate-key` fails if no active private signing key can be loaded.

## 1. Preserve the Old Public Key

Keep a copy of the current public key before switching the active signing key.

For APT, use the currently published armored key:

```bash
curl -fsSL https://packages.example.com/deb/pubkey.asc -o old-pubkey.asc
```

For RPM, use the currently published key for that repo and architecture:

```bash
curl -fsSL https://packages.example.com/rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc -o old-rpm-pubkey.asc
```

If you still have the old private key, you can also pass it to `--trusted-key`; Hakobin will publish only the public cert.

## 2. Start Signing with the New Key

Use the new private key as the active `--signing-key`, and pass the old public key as trusted:

```bash
hakobin \
  --signing-key ./new-signing-key.gpg \
  --trusted-key ./old-pubkey.asc \
  deb rotate-key
```

For RPM, the command discovers all repo/arch pairs under the `rpm/` S3 prefix and rotates each one:

```bash
hakobin \
  --signing-key ./new-signing-key.gpg \
  --trusted-key ./old-rpm-pubkey.asc \
  rpm rotate-key
```

After this, APT metadata, RPM package files, and RPM repository metadata are signed by the new key, and the published key bundle contains both old and new public keys.

## 3. Refresh Clients

APT clients should refresh their keyring and package indexes:

```bash
curl -fsSL https://packages.example.com/deb/pubkey.gpg \
  | sudo tee /etc/apt/keyrings/hakobin.gpg >/dev/null
sudo chmod 0644 /etc/apt/keyrings/hakobin.gpg
sudo apt update
```

DNF/YUM clients should refresh metadata. Depending on client caching, this may require cleaning metadata:

```bash
sudo dnf clean metadata
sudo dnf makecache
```

Keep publishing the old key as trusted until all expected clients have refreshed their keyring or repository metadata.

## 4. Retire the Old Key

After the overlap window, stop passing the old key:

```bash
hakobin --signing-key ./new-signing-key.gpg \
  deb rotate-key
```

For RPM:

```bash
hakobin --signing-key ./new-signing-key.gpg \
  rpm rotate-key
```

This refreshes the published key bundle so it contains only the new active public key. For RPM repositories, package files and repository metadata are re-signed again during this step.

## CI Pattern

Use `GPG_PRIVATE_KEY` for the new active key and `--trusted-key` for old public certs:

```bash
export GPG_PRIVATE_KEY="$(cat new-signing-key.gpg)"

hakobin --trusted-key ./old-pubkey.asc \
  deb rotate-key
```

When `GPG_PRIVATE_KEY` is set, every `--signing-key` path is treated as an additional trusted key path, not as the active signing key.
