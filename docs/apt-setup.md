# APT Repository Setup

This guide creates a signed APT repository under the `deb/` prefix in your S3-compatible bucket.

## 1. Configure Storage

Set the S3 credentials and bucket Hakobin should write to:

```bash
export AWS_ACCESS_KEY_ID=your-access-key
export AWS_SECRET_ACCESS_KEY=your-secret-key
export S3_BUCKET_NAME=your-bucket-name
export AWS_REGION=us-east-1
```

For MinIO or another S3-compatible service:

```bash
export S3_ENDPOINT=http://localhost:9000
export S3_USE_PATH_STYLE=true
```

Set the public base URL that clients will use:

```bash
export HAKOBIN_PUBLIC_URL=https://packages.example.com
```

## 2. Initialize the Repository

Initialize APT metadata and generate the initial signing key:

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

This writes:

- `signing-key.gpg` in the current directory.
- `deb/apt-repo.json`
- empty `Packages` indexes.
- signed `Release` metadata.
- `deb/pubkey.asc`, `deb/pubkey.gpg`, and `deb/setup.sh`.

Keep `signing-key.gpg` private. It is the private key used for future repository metadata signatures.

## 3. Upload Packages

Upload one or more `.deb` files:

```bash
hakobin --signing-key ./signing-key.gpg \
  deb upload ./package.deb \
  --distribution stable \
  --component main
```

In CI, provide the private key through the environment instead of a file path:

```bash
export GPG_PRIVATE_KEY="$(cat signing-key.gpg)"
hakobin deb upload ./package.deb --distribution stable --component main
```

Hakobin updates the package indexes, regenerates `Release`, signs `Release.gpg`, and refreshes the published public key bundle.

## 4. Configure APT Clients

The generated setup script is the simplest client setup path:

```bash
curl -fsSL https://packages.example.com/deb/setup.sh | sudo bash
```

Manual setup:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://packages.example.com/deb/pubkey.gpg \
  | sudo tee /etc/apt/keyrings/hakobin.gpg >/dev/null
sudo chmod 0644 /etc/apt/keyrings/hakobin.gpg

echo "deb [signed-by=/etc/apt/keyrings/hakobin.gpg] https://packages.example.com/deb stable main" \
  | sudo tee /etc/apt/sources.list.d/hakobin.list

sudo apt update
```

After `apt update`, clients can install packages from the repository:

```bash
sudo apt install package-name
```

