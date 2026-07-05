package repository

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"

	"hakobin/internal/apt"
	"hakobin/internal/cdn"
	"hakobin/internal/config"
	"hakobin/internal/deb"
	"hakobin/internal/openpgp"
	"hakobin/internal/storage"
)

const (
	DebPrefix    = "deb"
	MetadataPath = "apt-repo.json"
)

type RepoMetadata struct {
	Origin        string   `json:"origin"`
	Label         string   `json:"label"`
	Description   string   `json:"description"`
	Distributions []string `json:"distributions"`
	Components    []string `json:"components"`
	Architectures []string `json:"architectures"`
}

type RepositoryManager struct {
	cfg   *config.Config
	store storage.Store
}

func NewRepositoryManager(cfg *config.Config, store storage.Store) *RepositoryManager {
	return &RepositoryManager{
		cfg:   cfg,
		store: store,
	}
}

type InitRequest struct {
	Metadata           RepoMetadata
	KeyName            string
	KeyEmail           string
	KeyExpirationYears uint32
}

type UploadRequest struct {
	DebFiles     []string
	Distribution string
	Component    string
	Force        bool
	SigningKeys  *openpgp.SigningKeys
}

type RemoveRequest struct {
	Package      string
	Version      string
	Architecture string
	Distribution string
	Component    string
	Force        bool
	SigningKeys  *openpgp.SigningKeys
}

type PackageRow struct {
	Name         string
	Version      string
	Architecture string
	Component    string
	Distribution string
	Description  string
}

func (rm *RepositoryManager) Init(ctx context.Context, req *InitRequest) error {
	metaKey := rm.key(MetadataPath)
	exists, err := rm.store.Exists(ctx, metaKey)
	if err != nil {
		return err
	}
	if exists {
		fmt.Println("Repository already initialized. Use --force to reinitialize.")
		return nil
	}

	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}
	fmt.Printf("Initializing APT repository in S3 bucket: %s\n", bucket)

	keyPair, err := openpgp.GenerateKeyPair(req.KeyName, req.KeyEmail, "APT Repository Signing Key", req.KeyExpirationYears)
	if err != nil {
		return fmt.Errorf("failed to generate GPG key: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(cwd, "signing-key.gpg")
	if err := keyPair.SavePrivateKey(keyPath); err != nil {
		return fmt.Errorf("failed to save private key: %w", err)
	}

	fmt.Println("GPG key generated successfully!")
	fmt.Printf("   Key ID: %s\n", keyPair.KeyID)
	if keyPair.Expiration != nil {
		fmt.Printf("   Expires: %s\n", keyPair.Expiration.Format("2006-01-02"))
	} else {
		fmt.Println("   Expires: Never")
	}
	fmt.Printf("   Private key saved to: %s\n", keyPath)

	if err := rm.saveMetadata(ctx, &req.Metadata); err != nil {
		return err
	}

	signingKeys := openpgp.NewSigningKeys(keyPair, nil)
	if err := rm.createInitialIndexes(ctx, &req.Metadata, signingKeys); err != nil {
		return err
	}

	if err := rm.uploadPublicKeys(ctx, signingKeys); err != nil {
		return err
	}

	if err := rm.createSetupScript(ctx, &req.Metadata, keyPair); err != nil {
		return err
	}

	baseURL, err := rm.debBaseURL()
	if err != nil {
		return err
	}

	fmt.Println("\nRepository initialized successfully!")
	fmt.Println("\nSummary:")
	fmt.Printf("   Repository configuration: s3://%s/%s\n", bucket, metaKey)
	fmt.Println("   Public keys uploaded:")
	fmt.Printf("     - Binary format: s3://%s/%s (for setup script)\n", bucket, rm.key("pubkey.gpg"))
	fmt.Printf("     - Armored format: s3://%s/%s (for manual import)\n", bucket, rm.key("pubkey.asc"))
	fmt.Printf("   Setup script: s3://%s/%s\n", bucket, rm.key("setup.sh"))

	fmt.Println("\nQuick Start:")
	fmt.Println("   Users can setup your repository:")
	fmt.Printf("     curl -fsSL %s/setup.sh | sudo bash\n", baseURL)
	fmt.Println("\nUpload packages:")
	fmt.Printf("     hakobin deb upload <package.deb> --distribution %s --component %s --architecture %s\n",
		req.Metadata.Distributions[0],
		req.Metadata.Components[0],
		req.Metadata.Architectures[0],
	)
	fmt.Println("\nAdvanced Usage:")
	fmt.Println("   Custom key location:")
	fmt.Printf("     hakobin deb --signing-key %s upload <package.deb>\n", keyPath)
	fmt.Println("\n   CI/CD with environment variable:")
	fmt.Printf("     export GPG_PRIVATE_KEY=\"$(cat %s)\"\n", keyPath)
	fmt.Println("     hakobin deb upload <package.deb>")

	return nil
}

func (rm *RepositoryManager) Upload(ctx context.Context, req *UploadRequest) error {
	for _, f := range req.DebFiles {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", f)
		}
	}

	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}

	metadata, err := rm.loadMetadata(ctx)
	var metadataPtr *RepoMetadata
	if err == nil {
		metadataPtr = metadata
	}

	fmt.Printf("Uploading %d package(s) to S3 bucket: %s\n", len(req.DebFiles), bucket)
	fmt.Printf("Distribution: %s\n", req.Distribution)
	fmt.Printf("Component: %s\n", req.Component)

	uploaded := 0
	skipped := 0
	failed := 0
	var firstErr error

	for index, file := range req.DebFiles {
		fmt.Printf("\n[%d/%d] Processing: %s\n", index+1, len(req.DebFiles), file)
		outcome, err := rm.uploadOne(ctx, file, req, metadataPtr)
		if err != nil {
			failed++
			fmt.Printf("Failed: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			switch outcome {
			case uploadOutcomeUploaded:
				uploaded++
				fmt.Println("Uploaded successfully")
			case uploadOutcomeSkipped:
				skipped++
				fmt.Println("Skipped (already exists)")
			}
		}
	}

	fmt.Println("\n==================================================")
	fmt.Println("Upload Summary:")
	fmt.Printf("Uploaded: %d\n", uploaded)
	if skipped > 0 {
		fmt.Printf("Skipped: %d\n", skipped)
	}
	if failed > 0 {
		fmt.Printf("Failed: %d\n", failed)
		if len(req.DebFiles) > 1 && firstErr != nil {
			fmt.Printf("\nFirst error encountered: %v\n", firstErr)
			fmt.Println("Use --force flag to overwrite existing packages.")
		}
		return fmt.Errorf("%d package(s) failed to upload", failed)
	}

	fmt.Println("All packages processed successfully!")
	return nil
}

func (rm *RepositoryManager) ListPackages(ctx context.Context, distribution, component, packageName *string) error {
	metadata, err := rm.loadMetadata(ctx)
	if err != nil {
		return err
	}

	var rows []PackageRow
	for _, dist := range metadata.Distributions {
		if distribution != nil && *distribution != "" && *distribution != dist {
			continue
		}
		for _, comp := range metadata.Components {
			if component != nil && *component != "" && *component != comp {
				continue
			}
			for _, arch := range metadata.Architectures {
				if arch == "all" {
					continue
				}

				repo := apt.RepositoryPath{
					Distribution: dist,
					Component:    comp,
					Architecture: arch,
				}

				data, err := rm.store.Download(ctx, rm.key(repo.PackagesPath()))
				if err != nil {
					continue
				}

				packages, err := apt.ParsePackages(string(data))
				if err != nil {
					continue
				}

				for _, p := range packages {
					if packageName != nil && *packageName != "" {
						filter := strings.ToLower(*packageName)
						if !strings.Contains(strings.ToLower(p.Package), filter) {
							continue
						}
					}

					descLine := ""
					lines := strings.Split(p.Description, "\n")
					if len(lines) > 0 {
						descLine = lines[0]
					}

					rows = append(rows, PackageRow{
						Name:         p.Package,
						Version:      p.Version,
						Architecture: p.Architecture,
						Component:    comp,
						Distribution: dist,
						Description:  descLine,
					})
				}
			}
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		if rows[i].Version != rows[j].Version {
			return rows[i].Version < rows[j].Version
		}
		if rows[i].Architecture != rows[j].Architecture {
			return rows[i].Architecture < rows[j].Architecture
		}
		if rows[i].Component != rows[j].Component {
			return rows[i].Component < rows[j].Component
		}
		return rows[i].Distribution < rows[j].Distribution
	})

	// Deduplicate
	var uniqueRows []PackageRow
	for i, r := range rows {
		if i == 0 {
			uniqueRows = append(uniqueRows, r)
			continue
		}
		last := uniqueRows[len(uniqueRows)-1]
		if r.Name == last.Name && r.Version == last.Version && r.Architecture == last.Architecture &&
			r.Component == last.Component && r.Distribution == last.Distribution {
			continue
		}
		uniqueRows = append(uniqueRows, r)
	}

	showActiveFilters(distribution, component, packageName)

	if len(uniqueRows) == 0 {
		fmt.Println("No packages found matching the criteria.")
		return nil
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Package", "Version", "Architecture", "Component", "Distribution", "Description"})
	table.SetBorder(true)
	table.SetAutoWrapText(false)

	for _, r := range uniqueRows {
		table.Append([]string{
			r.Name,
			r.Version,
			r.Architecture,
			r.Component,
			r.Distribution,
			truncate(r.Description, 60),
		})
	}
	table.Render()
	fmt.Printf("\nTotal packages: %d\n", len(uniqueRows))

	return nil
}

func (rm *RepositoryManager) Remove(ctx context.Context, req *RemoveRequest) error {
	metadata, err := rm.loadMetadata(ctx)
	if err != nil {
		return err
	}

	repo := apt.RepositoryPath{
		Distribution: req.Distribution,
		Component:    req.Component,
		Architecture: req.Architecture,
	}

	filename := fmt.Sprintf("%s_%s_%s.deb", req.Package, req.Version, req.Architecture)
	poolPath := fmt.Sprintf("%s/%s", repo.PoolPath(req.Package), filename)

	exists, err := rm.store.Exists(ctx, rm.key(poolPath))
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("package %s version %s architecture %s not found in %s/%s",
			req.Package, req.Version, req.Architecture, req.Distribution, req.Component)
	}

	fmt.Printf("Package: %s\n", req.Package)
	fmt.Printf("Version: %s\n", req.Version)
	fmt.Printf("Architecture: %s\n", req.Architecture)
	fmt.Printf("Distribution: %s\n", req.Distribution)
	fmt.Printf("Component: %s\n", req.Component)
	fmt.Println()
	fmt.Printf("Found package at: %s\n", poolPath)

	if !req.Force {
		fmt.Print("Are you sure you want to remove this package? (y/N): ")
		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Removal cancelled.")
			return nil
		}
	}

	if err := rm.store.Delete(ctx, rm.key(poolPath)); err != nil {
		return err
	}

	packagesPath := repo.PackagesPath()
	existsPackages, err := rm.store.Exists(ctx, rm.key(packagesPath))
	if err == nil && existsPackages {
		data, err := rm.store.Download(ctx, rm.key(packagesPath))
		if err == nil {
			packages, err := apt.ParsePackages(string(data))
			if err == nil {
				var filtered []apt.PackageEntry
				for _, p := range packages {
					if p.Package == req.Package && p.Version == req.Version && p.Architecture == req.Architecture {
						continue
					}
					filtered = append(filtered, p)
				}

				updated := apt.GeneratePackagesContent(filtered)
				_ = rm.store.UploadBytes(ctx, rm.key(packagesPath), []byte(updated), "text/plain")

				gzBytes, err := apt.CompressGzip([]byte(updated))
				if err == nil {
					_ = rm.store.UploadBytes(ctx, rm.key(repo.PackagesGzPath()), gzBytes, "application/gzip")
				}
			}
		}
	}

	if err := rm.updateRelease(ctx, req.Distribution, metadata, req.SigningKeys); err != nil {
		return err
	}

	fmt.Println("Package removed successfully!")
	return nil
}

func (rm *RepositoryManager) RotateKey(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
	metadata, err := rm.loadMetadata(ctx)
	if err != nil {
		return err
	}

	if signingKeys.Active == nil {
		return fmt.Errorf("rotate-key requires an active private signing key")
	}

	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}

	fmt.Printf("Rotating APT repository signing keys in S3 bucket: %s\n", bucket)

	if err := rm.uploadPublicKeys(ctx, signingKeys); err != nil {
		return err
	}

	if err := rm.createSetupScript(ctx, metadata, signingKeys.Active); err != nil {
		return err
	}

	for _, dist := range metadata.Distributions {
		fmt.Printf("Re-signing APT distribution: %s\n", dist)
		if err := rm.updateRelease(ctx, dist, metadata, signingKeys); err != nil {
			return err
		}
	}

	fmt.Println("APT signing keys rotated successfully.")
	return nil
}

type uploadOutcome string

const (
	uploadOutcomeUploaded uploadOutcome = "uploaded"
	uploadOutcomeSkipped  uploadOutcome = "skipped"
)

func (rm *RepositoryManager) uploadOne(ctx context.Context, filePath string, req *UploadRequest, metadata *RepoMetadata) (uploadOutcome, error) {
	pkg, err := deb.FromPath(filePath)
	if err != nil {
		return "", err
	}

	architecture, err := pkg.Architecture()
	if err != nil {
		return "", err
	}
	packageName, err := pkg.Package()
	if err != nil {
		return "", err
	}
	version, err := pkg.Version()
	if err != nil {
		return "", err
	}
	filename, err := pkg.GeneratedFilename()
	if err != nil {
		return "", err
	}

	fmt.Printf("  Package: %s\n", packageName)
	fmt.Printf("  Version: %s\n", version)
	fmt.Printf("  Architecture: %s\n", architecture)
	fmt.Printf("  Generated filename: %s\n", filename)

	if metadata != nil {
		if err := rm.ensureArchitecture(ctx, metadata, architecture); err != nil {
			return "", err
		}
	}

	repo := apt.RepositoryPath{
		Distribution: req.Distribution,
		Component:    req.Component,
		Architecture: architecture,
	}

	poolPath := fmt.Sprintf("%s/%s", repo.PoolPath(packageName), filename)

	exists, err := rm.packageExists(ctx, &repo, packageName, version, architecture)
	if err != nil {
		return "", err
	}

	if exists {
		if req.Force {
			fmt.Println("  Force uploading (overwriting existing package)")
		} else {
			return uploadOutcomeSkipped, nil
		}
	}

	err = rm.store.UploadBytes(ctx, rm.key(poolPath), pkg.Raw, "application/vnd.debian.binary-package")
	if err != nil {
		return "", err
	}

	packagesPath := repo.PackagesPath()
	var existing string
	pkgExists, err := rm.store.Exists(ctx, rm.key(packagesPath))
	if err == nil && pkgExists {
		data, err := rm.store.Download(ctx, rm.key(packagesPath))
		if err == nil {
			existing = string(data)
		}
	}

	if existing != "" && !strings.HasSuffix(existing, "\n\n") {
		if strings.HasSuffix(existing, "\n") {
			existing += "\n"
		} else {
			existing += "\n\n"
		}
	}

	existing += pkg.PackageEntry(poolPath)
	if !strings.HasSuffix(existing, "\n\n") {
		existing += "\n"
	}

	err = rm.store.UploadBytes(ctx, rm.key(packagesPath), []byte(existing), "text/plain")
	if err != nil {
		return "", err
	}

	gzBytes, err := apt.CompressGzip([]byte(existing))
	if err != nil {
		return "", err
	}

	err = rm.store.UploadBytes(ctx, rm.key(repo.PackagesGzPath()), gzBytes, "application/gzip")
	if err != nil {
		return "", err
	}

	var fallbackMetadata RepoMetadata
	releaseMetadata := metadata
	if releaseMetadata == nil {
		fallbackMetadata = RepoMetadata{
			Origin:        "",
			Label:         "",
			Description:   "",
			Distributions: []string{req.Distribution},
			Components:    []string{req.Component},
			Architectures: []string{architecture},
		}
		releaseMetadata = &fallbackMetadata
	}

	err = rm.updateRelease(ctx, req.Distribution, releaseMetadata, req.SigningKeys)
	if err != nil {
		return "", err
	}

	return uploadOutcomeUploaded, nil
}

func (rm *RepositoryManager) createInitialIndexes(ctx context.Context, metadata *RepoMetadata, signingKeys *openpgp.SigningKeys) error {
	for _, dist := range metadata.Distributions {
		for _, comp := range metadata.Components {
			for _, arch := range metadata.Architectures {
				if arch == "all" {
					continue
				}
				repo := apt.RepositoryPath{
					Distribution: dist,
					Component:    comp,
					Architecture: arch,
				}

				if err := rm.store.UploadBytes(ctx, rm.key(repo.PackagesPath()), []byte(""), "text/plain"); err != nil {
					return err
				}

				gzEmpty, _ := apt.CompressGzip([]byte(""))
				if err := rm.store.UploadBytes(ctx, rm.key(repo.PackagesGzPath()), gzEmpty, "application/gzip"); err != nil {
					return err
				}
			}
		}
		if err := rm.updateRelease(ctx, dist, metadata, signingKeys); err != nil {
			return err
		}
	}
	return nil
}

func (rm *RepositoryManager) ensureArchitecture(ctx context.Context, metadata *RepoMetadata, architecture string) error {
	if architecture == "all" {
		return nil
	}

	found := false
	for _, arch := range metadata.Architectures {
		if arch == architecture {
			found = true
			break
		}
	}

	if found {
		return nil
	}

	metadata.Architectures = append(metadata.Architectures, architecture)

	for _, dist := range metadata.Distributions {
		for _, comp := range metadata.Components {
			repo := apt.RepositoryPath{
				Distribution: dist,
				Component:    comp,
				Architecture: architecture,
			}

			if err := rm.store.UploadBytes(ctx, rm.key(repo.PackagesPath()), []byte(""), "text/plain"); err != nil {
				return err
			}

			gzEmpty, _ := apt.CompressGzip([]byte(""))
			if err := rm.store.UploadBytes(ctx, rm.key(repo.PackagesGzPath()), gzEmpty, "application/gzip"); err != nil {
				return err
			}
		}
	}

	return rm.saveMetadata(ctx, metadata)
}

func (rm *RepositoryManager) updateRelease(ctx context.Context, distribution string, metadata *RepoMetadata, signingKeys *openpgp.SigningKeys) error {
	var architectures []string
	for _, arch := range metadata.Architectures {
		if arch != "all" {
			architectures = append(architectures, arch)
		}
	}

	var files []apt.ReleaseFileEntry
	for _, comp := range metadata.Components {
		for _, arch := range architectures {
			repo := apt.RepositoryPath{
				Distribution: distribution,
				Component:    comp,
				Architecture: arch,
			}

			paths := []struct {
				s3Path      string
				releasePath string
			}{
				{repo.PackagesPath(), fmt.Sprintf("%s/binary-%s/Packages", comp, arch)},
				{repo.PackagesGzPath(), fmt.Sprintf("%s/binary-%s/Packages.gz", comp, arch)},
			}

			for _, p := range paths {
				data, err := rm.store.Download(ctx, rm.key(p.s3Path))
				if err == nil {
					md5Val, sha1Val, sha256Val := apt.CalculateHashes(data)
					files = append(files, apt.ReleaseFileEntry{
						Filename: p.releasePath,
						Size:     len(data),
						MD5:      md5Val,
						SHA1:     sha1Val,
						SHA256:   sha256Val,
					})
				}
			}
		}
	}

	release := apt.ReleaseFile{
		Origin:        metadata.Origin,
		Label:         metadata.Label,
		Description:   metadata.Description,
		Distribution:  distribution,
		Components:    metadata.Components,
		Architectures: architectures,
		Date:          time.Now(),
		Files:         files,
	}

	releaseData := release.Generate()
	releasePath := fmt.Sprintf("dists/%s/Release", distribution)

	err := rm.store.UploadBytes(ctx, rm.key(releasePath), releaseData, "text/plain")
	if err != nil {
		return err
	}

	if signingKeys != nil && signingKeys.Active != nil {
		sig, err := signingKeys.Active.SignDetached(releaseData)
		if err != nil {
			return err
		}

		err = rm.store.UploadBytes(ctx, rm.key(releasePath+".gpg"), []byte(sig), "application/pgp-signature")
		if err != nil {
			return err
		}

		err = rm.uploadPublicKeys(ctx, signingKeys)
		if err != nil {
			return err
		}
	}

	invalidator, err := cdn.FromEnv()
	if err == nil && invalidator != nil {
		paths := []string{
			rm.key(releasePath),
			rm.key(releasePath + ".gpg"),
		}

		for _, comp := range metadata.Components {
			for _, arch := range architectures {
				paths = append(paths, rm.key(fmt.Sprintf("dists/%s/%s/binary-%s/Packages", distribution, comp, arch)))
				paths = append(paths, rm.key(fmt.Sprintf("dists/%s/%s/binary-%s/Packages.gz", distribution, comp, arch)))
			}
		}

		setupExists, err := rm.store.Exists(ctx, rm.key("setup.sh"))
		if err == nil && setupExists {
			paths = append(paths, rm.key("setup.sh"))
		}

		if signingKeys != nil && signingKeys.Active != nil {
			paths = append(paths, rm.key("pubkey.asc"))
			paths = append(paths, rm.key("pubkey.gpg"))
		}

		if err := invalidator.Invalidate(ctx, rm.cfg, paths); err != nil {
			fmt.Printf("Warning: CDN invalidation failed: %v\n", err)
		}
	}

	return nil
}

func (rm *RepositoryManager) uploadPublicKeys(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
	if signingKeys.Active == nil {
		return nil
	}

	ascBytes, err := signingKeys.PublicKeyArmored()
	if err != nil {
		return err
	}

	gpgBytes, err := signingKeys.PublicKeyBinary()
	if err != nil {
		return err
	}

	err = rm.store.UploadBytes(ctx, rm.key("pubkey.asc"), ascBytes, "application/pgp-keys")
	if err != nil {
		return err
	}

	err = rm.store.UploadBytes(ctx, rm.key("pubkey.gpg"), gpgBytes, "application/pgp-keys")
	if err != nil {
		return err
	}

	return nil
}

func (rm *RepositoryManager) createSetupScript(ctx context.Context, metadata *RepoMetadata, keyPair *openpgp.KeyPair) error {
	baseURL, err := rm.debBaseURL()
	if err != nil {
		return err
	}

	repoID := repositoryIdentifier(metadata, rm.cfg.S3BucketName)

	var script strings.Builder
	fmt.Fprintf(&script, `#!/bin/bash
set -e

# APT Repository Setup Script
# Generated automatically by Hakobin Package

echo "Setting up APT repository: %s"
echo "Origin: %s"
echo "Description: %s"

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "This script must be run as root (use sudo)"
    exit 1
fi

# Create keyring directory if it doesn't exist
mkdir -p /etc/apt/keyrings

# Download and install GPG key (binary format, no gpg required)
echo "Installing repository GPG key..."
curl -fsSL %s/pubkey.gpg -o /etc/apt/keyrings/%s.gpg
chmod 644 /etc/apt/keyrings/%s.gpg

# Add repository to sources list
echo "Adding repository to APT sources..."
cat > /etc/apt/sources.list.d/%s.list << 'EOF'
# %s - %s
# Repository setup: %s
`, metadata.Label, metadata.Origin, metadata.Description, baseURL, repoID, repoID, repoID, metadata.Label, metadata.Origin, baseURL)

	for _, dist := range metadata.Distributions {
		for _, comp := range metadata.Components {
			fmt.Fprintf(&script, "deb [signed-by=/etc/apt/keyrings/%s.gpg] %s %s %s\n", repoID, baseURL, dist, comp)
		}
	}

	fmt.Fprintf(&script, `EOF

# Update package lists
echo "Updating package lists..."
apt update

echo ""
echo "Repository setup completed successfully!"
echo ""
echo "You can now install packages from this repository using:"
echo "  apt install <package-name>"
echo ""
echo "Available distributions: %s"
echo "Available components: %s"
echo "Supported architectures: %s"
echo ""
echo "Repository GPG Key ID: %s"
`, strings.Join(metadata.Distributions, ", "),
		strings.Join(metadata.Components, ", "),
		strings.Join(metadata.Architectures, ", "),
		keyPair.KeyID,
	)

	if keyPair.Expiration != nil {
		fmt.Fprintf(&script, "echo \"GPG Key Expires: %s\"\n", keyPair.Expiration.Format("2006-01-02"))
	} else {
		script.WriteString("echo \"GPG Key Expires: Never\"\n")
	}
	script.WriteString("echo \"\"\n")

	return rm.store.UploadBytes(ctx, rm.key("setup.sh"), []byte(script.String()), "text/plain")
}

func (rm *RepositoryManager) packageExists(ctx context.Context, repo *apt.RepositoryPath, packageName, version, architecture string) (bool, error) {
	exists, err := rm.store.Exists(ctx, rm.key(repo.PackagesPath()))
	if err != nil || !exists {
		return false, nil
	}

	data, err := rm.store.Download(ctx, rm.key(repo.PackagesPath()))
	if err != nil {
		return false, err
	}

	packages, err := apt.ParsePackages(string(data))
	if err != nil {
		return false, err
	}

	for _, p := range packages {
		if p.Package == packageName && p.Version == version && p.Architecture == architecture {
			return true, nil
		}
	}
	return false, nil
}

func (rm *RepositoryManager) loadMetadata(ctx context.Context) (*RepoMetadata, error) {
	data, err := rm.store.Download(ctx, rm.key(MetadataPath))
	if err != nil {
		return nil, fmt.Errorf("failed to download repository metadata: %w", err)
	}

	var metadata RepoMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse repository metadata: %w", err)
	}
	return &metadata, nil
}

func (rm *RepositoryManager) saveMetadata(ctx context.Context, metadata *RepoMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return rm.store.UploadBytes(ctx, rm.key(MetadataPath), data, "application/json")
}

func (rm *RepositoryManager) key(path string) string {
	return fmt.Sprintf("%s/%s", DebPrefix, strings.TrimPrefix(path, "/"))
}

func (rm *RepositoryManager) debBaseURL() (string, error) {
	url, err := rm.cfg.RepositoryBaseURL()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s", strings.TrimSuffix(url, "/"), DebPrefix), nil
}

func repositoryIdentifier(metadata *RepoMetadata, bucket string) string {
	source := metadata.Origin
	if strings.TrimSpace(source) == "" {
		source = metadata.Label
	}
	if strings.TrimSpace(source) == "" {
		source = bucket
	}

	var out strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(source) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			out.WriteRune('-')
			lastDash = true
		}
	}

	res := strings.Trim(out.String(), "-")
	if len(res) > 50 {
		res = res[:50]
		res = strings.TrimSuffix(res, "-")
	}
	if len(res) < 2 {
		return strings.ToLower(bucket)
	}
	return res
}

func showActiveFilters(distribution, component, packageName *string) {
	var filters []string
	if distribution != nil && *distribution != "" {
		filters = append(filters, "distribution: "+*distribution)
	}
	if component != nil && *component != "" {
		filters = append(filters, "component: "+*component)
	}
	if packageName != nil && *packageName != "" {
		filters = append(filters, "package name: "+*packageName)
	}

	if len(filters) > 0 {
		fmt.Printf("Active filters: %s\n", strings.Join(filters, ", "))
	}
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-3]) + "..."
}
