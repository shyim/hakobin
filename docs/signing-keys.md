# Signing Key Publication

Hakobin relies on OpenPGP/GPG to sign repository metadata and package files. This ensures that client systems can verify the authenticity of packages before installing them.

---

## Active Signing Key Selection

Whenever you run a mutating command (such as `upload`, `remove`, or `rotate-key`), Hakobin looks for your private key in the following order:

1.  **`GPG_PRIVATE_KEY` Environment Variable:** The plaintext armored private key content. This is the recommended method for CI/CD pipelines.
2.  **`--signing-key <PATH>` Argument:** The file path to your GPG private key.
3.  **`./signing-key.gpg`:** A file named `signing-key.gpg` in the current working directory.

### Signing Safeguards

*   **Signed Repositories Require Keys:** If a repository has already been signed in the past, mutating commands (`deb upload`, `deb remove`, etc.) will **fail** if you do not provide a valid signing key. This prevents the repository's plaintext metadata from going out of sync with the signatures, which would break clients.
*   **Expiration Guard:** If the active signing key has expired, Hakobin will refuse to publish the repository updates. You can bypass this check in emergency recoveries by setting the environment variable `HAKOBIN_ALLOW_EXPIRED_KEY=1`.

---

## Armored vs. Binary Public Keys

Junior developers are often confused by the different GPG public key formats. Hakobin automatically generates and publishes both:

1.  **Armored Public Keys (`.asc`):** These are ASCII-plaintext representations of the GPG public key, starting with `-----BEGIN PGP PUBLIC KEY BLOCK-----`. They are human-readable, easy to copy-paste, and used for manual setup or YUM/DNF configurations.
2.  **Binary Public Keys (`.gpg`):** These are raw binary keyrings. Modern Debian/Ubuntu systems (specifically `apt` since version 2.4) prefer binary keyrings stored under `/etc/apt/keyrings/` to improve security.

### Published Key Paths

Hakobin publishes these files automatically under your S3 bucket prefixes:

*   **Debian/APT:**
    *   `deb/pubkey.asc` (Armored)
    *   `deb/pubkey.gpg` (Binary)
*   **RPM/YUM:**
    *   `rpm/<repo-name>/<architecture>/RPM-GPG-KEY-hakobin.asc` (Armored)

---

## The Trusted Key Bundle

During a **key rotation**, you must introduce a new key while still trusting the old key. Hakobin handles this by publishing a public key bundle.

You can specify additional trusted public keys via:
- Multiple `--signing-key` parameters
- The `--trusted-key` flag

```bash
hakobin \
  --signing-key ./new-signing-key.gpg \
  --trusted-key ./old-pubkey.asc \
  deb rotate-key
```

*   **Active Key:** Only the primary active key is used to sign package files and metadata.
*   **Trusted Keys:** All other keys provided via `--trusted-key` are merged into the public key bundle (`pubkey.asc`, `pubkey.gpg`, etc.). This enables client systems to trust both keys concurrently during transition windows.
