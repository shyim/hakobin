# APT Repository Setup

This guide walks you through creating and maintaining a signed Debian/APT repository under the `deb/` prefix in your S3-compatible bucket, suitable for Debian, Ubuntu, Linux Mint, and other Debian-based client systems.

---

## Understanding APT Repository Concepts

If you are new to the Debian ecosystem, the repository structure might look complex. Let's break down the three main variables:

*   **Distribution** (e.g., `stable`, `testing`, `bookworm`, `focal`): Represents the target release or release state. You can name these whatever you like (e.g., `production`, `staging`).
*   **Component** (e.g., `main`, `contrib`, `non-free`): A sub-section of the distribution. Typically, `main` is used for the primary software packages.
*   **Architecture** (e.g., `amd64`, `arm64`, `all`): The hardware architecture the packages are compiled for. `all` is used for architecture-independent packages (like Python scripts, configuration files, or themes).

---

## 1. Configure Storage

Hakobin needs credentials and settings to communicate with your S3 bucket. Export these environment variables:

```bash
export AWS_ACCESS_KEY_ID=your-access-key
export AWS_SECRET_ACCESS_KEY=your-secret-key
export S3_BUCKET_NAME=your-bucket-name
export AWS_REGION=us-east-1
```

For custom endpoints (like MinIO, Cloudflare R2, or DigitalOcean Spaces):

```bash
export S3_ENDPOINT=http://localhost:9000
export S3_USE_PATH_STYLE=true
```

Set the public base URL that clients will use to download packages:

```bash
export HAKOBIN_PUBLIC_URL=https://packages.example.com
```

---

## 2. Initialize the Repository

Initialize your APT metadata and let Hakobin generate the initial repository signing key:

```bash
hakobin deb init \
  --origin "Example Inc" \
  --label "Example Packages" \
  --description "APT repository for Example packages" \
  --distributions stable \
  --components main \
  --architectures amd64,all \
  --key-name "Example Repository Signing Key" \
  --key-email "packages@example.com"
```

### What does this command do?

*   Generates a private GPG key named `signing-key.gpg` in your current local directory.
*   Creates `deb/apt-repo.json` in your bucket to store these configurations.
*   Creates empty package indexes.
*   Signs the repository metadata.
*   Writes `deb/pubkey.asc`, `deb/pubkey.gpg` (the public keys), and `deb/setup.sh` (a client bootstrapping script) to S3.

> [!IMPORTANT]
> Save `signing-key.gpg` in a secure location (e.g., a credential manager or CI secrets). Do **not** commit it to source control. It is used to sign all future package uploads.

---

## 3. Upload Packages

To upload a `.deb` package file:

```bash
hakobin --signing-key ./signing-key.gpg \
  deb upload ./package.deb \
  --distribution stable \
  --component main
```

If you are running this in a CI pipeline (such as GitHub Actions or GitLab CI), passing file paths can be cumbersome. Instead, pass the key content directly via an environment variable:

```bash
export GPG_PRIVATE_KEY="$(cat signing-key.gpg)"
hakobin deb upload ./package.deb --distribution stable --component main
```

### Forcing Overwrites
By default, Hakobin will fail if you try to upload a package version that already exists. To overwrite it, use the `--force` flag:

```bash
hakobin --signing-key ./signing-key.gpg deb upload --force ./*.deb
```

---

## 4. Query and Manage Packages

### List Published Packages
To see what packages are currently published in your repository:

```bash
hakobin deb list
```

You can filter the list by package name, distribution, or component:

```bash
hakobin deb list nginx --distribution stable --component main
```

### Remove a Package
To delete a specific package version and architecture from the repository:

```bash
hakobin deb remove nginx --version 1.2.3 --architecture amd64 --force
```

---

## 5. Configure APT Clients

There are two ways to configure your client machines to download packages from your repository.

### Option A: The One-Liner Script (Recommended)
Hakobin automatically generates a helper shell script called `setup.sh` and publishes it to your bucket. Clients can run it directly:

```bash
curl -fsSL https://packages.example.com/deb/setup.sh | sudo bash
```

> [!NOTE]
> The `setup.sh` script detects the client's OS, installs the public GPG key into `/etc/apt/keyrings/hakobin.gpg`, adds the repository sources list, and runs `apt update`.

### Option B: Manual Client Setup
If you want to configure clients manually or write Ansible/Chef playbooks, run:

```bash
# 1. Create the keyrings folder if it doesn't exist
sudo install -d -m 0755 /etc/apt/keyrings

# 2. Download the binary public GPG key
curl -fsSL https://packages.example.com/deb/pubkey.gpg \
  | sudo tee /etc/apt/keyrings/hakobin.gpg >/dev/null
sudo chmod 0644 /etc/apt/keyrings/hakobin.gpg

# 3. Add the repository sources list pointing to the downloaded keyring
echo "deb [signed-by=/etc/apt/keyrings/hakobin.gpg] https://packages.example.com/deb stable main" \
  | sudo tee /etc/apt/sources.list.d/hakobin.list

# 4. Refresh package indexes
sudo apt update
```

Once completed, users can install your packages using standard apt commands:

```bash
sudo apt install package-name
```
