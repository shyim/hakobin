# RPM/YUM Repository Setup

This guide walks you through setting up and maintaining a signed RPM repository under the `rpm/` prefix in your S3-compatible bucket, suitable for RedHat, CentOS, Rocky Linux, AlmaLinux, and Fedora clients.

---

## What is an RPM Repository?

An RPM repository is a directory structure containing RPM package files (`.rpm`) and metadata stored in a subdirectory named `repodata/`. The metadata defines which packages are available, their dependencies, and their checksums.

Unlike APT which uses a unified configuration for multiple distributions, RPM repositories are typically separated by **repository name** (e.g., `stable`, `testing`) and **architecture** (e.g., `x86_64`, `noarch`). Hakobin organises these under a clear folder layout: `rpm/<repo-name>/<architecture>/`.

---

## 1. Configure Storage

First, configure your S3 storage credentials. You must export these environment variables before running any Hakobin command:

```bash
export AWS_ACCESS_KEY_ID=your-access-key
export AWS_SECRET_ACCESS_KEY=your-secret-key
export S3_BUCKET_NAME=your-bucket-name
export AWS_REGION=us-east-1
```

For custom S3 endpoints (like MinIO, Cloudflare R2, or DigitalOcean Spaces):

```bash
export S3_ENDPOINT=http://localhost:9000
export S3_USE_PATH_STYLE=true # Required for MinIO / path-style hosts
```

Set the public base URL that clients will use to download packages:

```bash
export HAKOBIN_PUBLIC_URL=https://packages.example.com
```

---

## 2. Generate a GPG Signing Key

Both RPM packages and the repository metadata (`repomd.xml`) should be signed so that client systems can verify they haven't been tampered with.

If you don't have a GPG key, you can generate one using standard GPG commands:

```bash
# Generate a key (use RSA, at least 2048 or 4096 bits)
gpg --batch --passphrase "" --quick-gen-key "Example Repository Signing Key <packages@example.com>" default default never

# Export the private key to a file
gpg --armor --export-secret-keys "packages@example.com" > signing-key.gpg

# Export the public key for client distribution
gpg --armor --export "packages@example.com" > public-key.asc
```

> [!WARNING]
> Keep `signing-key.gpg` extremely secure. Never commit it to git. Pass it to your CI pipeline using environment secrets.

---

## 3. Initialize the Repository

To initialize an RPM repository for a specific branch (e.g. `stable`) and architecture (e.g. `x86_64`), run:

```bash
hakobin --signing-key ./signing-key.gpg rpm init --repo stable --arch x86_64
```

This command will:
1. Create the `rpm/stable/x86_64/repodata/` directory structure on S3.
2. Upload the GPG public key as `RPM-GPG-KEY-hakobin.asc` under that directory.
3. Publish signed repository metadata.

---

## 4. Upload RPM Packages

To upload an RPM package to the repository:

```bash
hakobin --signing-key ./signing-key.gpg \
  rpm upload ./package.rpm \
  --repo stable \
  --arch x86_64
```

If you are running this in a CI environment, you can pass the signing key via the environment variable `GPG_PRIVATE_KEY` instead of storing it on disk:

```bash
export GPG_PRIVATE_KEY="$(cat signing-key.gpg)"
hakobin rpm upload ./package.rpm --repo stable --arch x86_64
```

### Batch Uploads
You can upload multiple files at once using wildcards. If you want to overwrite existing packages, append the `--force` flag:

```bash
hakobin --signing-key ./signing-key.gpg rpm upload --force ./*.rpm --repo stable --arch x86_64
```

> [!NOTE]
> During upload, Hakobin signs the `.rpm` file itself (enabling `gpgcheck` verification on clients) and updates the repository index `repomd.xml` and signs it (enabling `repo_gpgcheck`).

---

## 5. Query and Manage Packages

### List Packages
To see what packages are currently published in a repository:

```bash
hakobin rpm list --repo stable --arch x86_64
```

### Remove a Package
To remove a package, you need to specify its coordinates precisely to prevent accidental deletion of wrong versions:

```bash
hakobin rpm remove nginx \
  --epoch 0 \
  --version 1.2.3 \
  --release 1.el9 \
  --arch x86_64 \
  --repo stable \
  --repo-arch x86_64 \
  --force
```

---

## 6. Configure Client Systems (DNF/YUM)

To configure client servers (such as AlmaLinux, Rocky Linux, or RedHat) to use your new repository, create a repository configuration file at `/etc/yum.repos.d/hakobin.repo`:

```ini
[hakobin-stable]
name=Hakobin Stable Repository
baseurl=https://packages.example.com/rpm/stable/x86_64
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://packages.example.com/rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc
```

### Explanation of Security Directives:
*   `gpgcheck=1`: Tells the client's package manager to verify the GPG signature on every `.rpm` file before installing it.
*   `repo_gpgcheck=1`: Tells the package manager to verify the GPG signature on the repository metadata (`repomd.xml.asc`) to prevent Man-in-the-Middle (MitM) attacks altering package lists.
*   `gpgkey=...`: The URL where the client can retrieve the public key bundle to verify both signatures.

### Apply the Configuration
Run the following commands on the client machine to flush local cache and index the new repository:

```bash
sudo dnf clean metadata
sudo dnf makecache
sudo dnf install package-name
```
