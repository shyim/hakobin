# How Data is Stored

Hakobin is a **stateless CLI tool**. It does not run a server or maintain a separate database (like PostgreSQL or Redis). Instead, it stores all state directly inside the S3 bucket using standard APT and RPM directory layouts and metadata files.

---

## Bucket Layout

Here is the directory tree that Hakobin creates and maintains inside your S3 bucket:

```text
bucket/
├── deb/
│   ├── .hakobin.lock                       # Concurrency lock file
│   ├── apt-repo.json                       # Hakobin-specific repository configuration
│   ├── pubkey.asc                          # Armored public key bundle (ASCII text)
│   ├── pubkey.gpg                          # Binary public key bundle
│   ├── setup.sh                            # One-line client bootstrap script
│   ├── pool/                               # Where the actual .deb package files are stored
│   │   └── <component>/                    # e.g., main
│   │       └── <first-letter>/             # e.g., n/ (from nginx)
│   │           └── <package-name>/         # e.g., nginx/
│   │               └── nginx_1.2.3_amd64.deb
│   └── dists/                              # APT Distribution metadata
│       └── <distribution>/                 # e.g., stable
│           ├── Release                     # Plaintext index of components & file checksums
│           ├── Release.gpg                 # Detached GPG signature for 'Release'
│           ├── InRelease                   # Inline GPG-signed version of 'Release'
│           └── <component>/                # e.g., main
│               └── binary-<architecture>/  # e.g., binary-amd64
│                   ├── Packages            # Index list of all packages in this component/arch
│                   └── Packages.gz         # Compressed index list
└── rpm/
    ├── .hakobin.lock                       # Concurrency lock file
    └── <repo-name>/                        # e.g., stable
        └── <architecture>/                 # e.g., x86_64
            ├── RPM-GPG-KEY-hakobin.asc     # Public key bundle (ASCII text)
            ├── Packages/                   # Where the actual .rpm package files are stored
            │   └── nginx-1.2.3-1.el9.x86_64.rpm
            └── repodata/                   # RPM repository metadata
                ├── repomd.xml              # Index pointing to metadata files
                ├── repomd.xml.asc          # GPG signature for 'repomd.xml'
                ├── <hash>-primary.xml.gz   # Package dependency and checksum list
                ├── <hash>-filelists.xml.gz # Lists of all files in all packages
                └── <hash>-other.xml.gz     # Changelog and extra metadata
```

---

## File Explanations for Beginners

### 1. Debian/APT Metadata (under `deb/`)

*   **`apt-repo.json`**: This is a simple JSON file created by Hakobin. It acts as Hakobin's configuration registry, letting Hakobin know which components (like `main`), distributions (like `stable`), and architectures (like `amd64`) are supported by the repository.
*   **`pool/`**: This directory structure is defined by Debian standards. The first letter subdirectories (e.g. `n/` for `nginx`) are used to prevent a single directory from containing tens of thousands of package files, which could cause filesystem performance issues.
*   **`Packages`**: A plaintext index containing the control information of every package in the repository (e.g., package name, version, description, dependencies, SHA256 checksum, and its relative S3 path).
*   **`Release` & `InRelease`**: The `Release` file contains the list of all index files (like `Packages`) and their checksums. `InRelease` is the same file but cryptographically signed inline by your GPG private key. Clients download this first, verify the signature against their trusted keyring, and then use the checksums inside it to make sure the downloaded package indexes haven't been tampered with.

### 2. RPM/YUM Metadata (under `rpm/`)

*   **`repodata/`**: Under YUM/DNF, metadata is stored as XML files compressed with gzip.
*   **`repomd.xml`**: The master index of the repository. It points to the paths and checksums of `primary.xml.gz`, `filelists.xml.gz`, and `other.xml.gz`.
*   **`repomd.xml.asc`**: The GPG signature of `repomd.xml`. YUM/DNF clients verify this signature to confirm that the package list itself is genuine (known as `repo_gpgcheck=1`).
*   **`primary.xml.gz`**: Contains the metadata for all packages in the repository, including dependency information, descriptions, and file hashes.

---

## Why is there `apt-repo.json` but no equivalent for RPM?

A common question is why APT requires an extra tracking file (`apt-repo.json`) while RPM repositories do not:

1.  **APT's Multi-Distribution & Multi-Component Nature:**
    A single Debian/APT repository is designed to house multiple parallel distributions (e.g., `stable`, `testing`, `unstable`) and components (e.g., `main`, `contrib`) within the **same directory structure**. Because Hakobin needs to regenerate index files for all configured combinations of distributions, components, and architectures, it needs a registry file (`apt-repo.json`) to keep track of what was configured during initialization. Without this file, Hakobin would have no way of knowing what distributions or architectures were defined unless it crawled the entire S3 bucket recursively on every command.
2.  **RPM's Flat Repository Layout:**
    RPM (YUM/DNF) repositories do not support multiple distributions or components in a single folder. An RPM repository folder (e.g., `rpm/stable/x86_64/`) is dedicated to a **single** repository name (`stable`) and a **single** architecture (`x86_64`). Because the repository structure is completely flat and self-contained, Hakobin does not need an external registry to understand the layout; the standard `repodata/repomd.xml` file inside that folder fully describes everything that exists, making an extra config file unnecessary.

---

## Concurrency and Locking

What happens if two CI/CD builds try to upload a package at the same time?

To prevent corrupting the repository index files, Hakobin uses a lock-based synchronization mechanism on S3:
1.  Before making any changes, Hakobin creates an empty object named `.hakobin.lock` under the repository prefix (e.g., `deb/.hakobin.lock` or `rpm/.hakobin.lock`).
2.  If the lock already exists, Hakobin will wait (poll) until it is released or until the lock's Time-To-Live (TTL) expires (default is 5 minutes). This safeguards against a stalled build locking the repository forever.
3.  Once the upload and metadata generation are complete, Hakobin deletes the lock file.
