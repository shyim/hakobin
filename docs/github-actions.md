# GitHub Actions Integration

This guide walks you through integrating Hakobin into your GitHub Actions workflow to automate package building, uploading, and repository publishing.

---

## 1. Downloading Hakobin in CI

Instead of manually fetching the Hakobin binary with `curl` or compiling it from source in every pipeline run, you can use the open-source **[action-install-gh-release](https://github.com/jaxxstorm/action-install-gh-release)** action to pull the binary directly from GitHub releases.

Add this step to your workflow:

```yaml
- name: Install Hakobin
  uses: jaxxstorm/action-install-gh-release@v1.11.0
  with:
    repo: shyim/hakobin
    # Replace with a specific tag (e.g., v1.0.0) or omit to get the latest release
    tag: v1.0.0
```

> [!NOTE]
> This action automatically detects the GitHub runner's OS and architecture, downloads the correct asset, untars it, and adds the `hakobin` binary to the system PATH.

---

## 2. Managing Your GPG Signing Key

To sign your repository, you should store your GPG private key in **GitHub Secrets** (e.g. `GPG_PRIVATE_KEY` under *Settings > Secrets and variables > Actions*).

Since GPG keys are multiline text, you can paste the entire block including the `-----BEGIN PGP PRIVATE KEY BLOCK-----` header and footer.

In your workflow, pass the secret as an environment variable to the step executing Hakobin:

```yaml
env:
  GPG_PRIVATE_KEY: ${{ secrets.GPG_PRIVATE_KEY }}
```

---

## 3. Authenticating with S3 Storage

You have two options for authenticating Hakobin with your S3 bucket in GitHub Actions.

### Option A: Static Secrets (Simple)

Save your static S3 credentials under GitHub Secrets and export them directly to the environment:

```yaml
name: Publish Package
on:
  push:
    tags:
      - 'v*'

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout Code
        uses: actions/checkout@v4

      - name: Install Hakobin
        uses: jaxxstorm/action-install-gh-release@v1.11.0
        with:
          repo: shyim/hakobin

      # Run your custom build step to produce package.deb or package.rpm
      - name: Build Package
        run: |
          echo "Building package..."
          # Generates package.deb

      - name: Upload to S3
        env:
          AWS_ACCESS_KEY_ID: ${{ secrets.AWS_ACCESS_KEY_ID }}
          AWS_SECRET_ACCESS_KEY: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          S3_BUCKET_NAME: ${{ secrets.S3_BUCKET_NAME }}
          AWS_REGION: us-east-1
          GPG_PRIVATE_KEY: ${{ secrets.GPG_PRIVATE_KEY }}
        run: |
          hakobin deb upload ./package.deb --distribution stable --component main
```

---

### Option B: AWS OpenID Connect (OIDC) (Recommended & Secure)

Using static Access Keys in GitHub is discouraged because secrets can be leaked or misconfigured, and they require manual rotation.

A better practice is to use **OpenID Connect (OIDC)**. This permits GitHub Actions to request a temporary, short-lived token directly from AWS, removing the need for static `AWS_ACCESS_KEY_ID` or `AWS_SECRET_ACCESS_KEY` secrets entirely.

#### Step 1: Set up the IAM Role in AWS
You must configure an IAM Role in your AWS account with a trust policy that trusts GitHub's OIDC provider.

**Trust Policy Example:**
```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::YOUR_AWS_ACCOUNT_ID:oidc-provider/token.actions.githubusercontent.com"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
        },
        "StringLike": {
          "token.actions.githubusercontent.com:sub": "repo:your-org/your-repo:*"
        }
      }
    }
  ]
}
```

Make sure the role has an attached IAM Policy granting `s3:GetObject`, `s3:PutObject`, and `s3:DeleteObject` permissions for your repository bucket path.

#### Step 2: Configure your Workflow
In your GitHub Actions workflow:
1.  Add `permissions: id-token: write` and `contents: read` to grant GitHub the right to ask AWS for temporary tokens.
2.  Use the official `aws-actions/configure-aws-credentials` action.

```yaml
name: Publish Package (OIDC)
on:
  push:
    tags:
      - 'v*'

# Required for requesting the JWT token from AWS
permissions:
  id-token: write
  contents: read

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout Code
        uses: actions/checkout@v4

      - name: Install Hakobin
        uses: jaxxstorm/action-install-gh-release@v1.11.0
        with:
          repo: shyim/hakobin

      # Request temporary AWS credentials using OIDC
      - name: Configure AWS Credentials
        uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::YOUR_AWS_ACCOUNT_ID:role/YourGithubActionsS3Role
          aws-region: us-east-1

      - name: Build Package
        run: |
          echo "Building package..."

      # Hakobin will automatically detect the temporary AWS credentials
      # provided in the environment by the configure-aws-credentials step.
      - name: Upload to S3
        env:
          S3_BUCKET_NAME: ${{ secrets.S3_BUCKET_NAME }}
          GPG_PRIVATE_KEY: ${{ secrets.GPG_PRIVATE_KEY }}
        run: |
          hakobin deb upload ./package.deb --distribution stable --component main
```
