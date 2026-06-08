package repository

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
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
	// LockPath is the repository-scoped lock object guarding metadata mutations.
	LockPath = ".hakobin.lock"
	// lockTTLSeconds bounds how long a crashed uploader can block others.
	lockTTLSeconds = 300
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

// nameSegmentRe matches a safe distribution / component / architecture name: a
// single path segment with no separators or shell metacharacters. These values
// are interpolated into S3 key paths, APT source lines, and the generated setup
// script, so they must be tightly constrained.
var nameSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validateName(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if value == "." || value == ".." || !nameSegmentRe.MatchString(value) {
		return fmt.Errorf("invalid %s %q: must be a single path segment of letters, digits, '.', '_' or '-'", kind, value)
	}
	return nil
}

// shQuote renders a string as a POSIX single-quoted shell word so it is safe to
// embed anywhere a shell will evaluate the surrounding script. Single quotes
// suppress all expansion; the only character that must be handled specially is a
// literal single quote, which is closed, escaped, and reopened ('\”).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func validateNames(kind string, values []string) error {
	for _, v := range values {
		if err := validateName(kind, v); err != nil {
			return err
		}
	}
	return nil
}

// validateMetadata rejects distribution/component/architecture names that could
// escape a key path or inject content into generated APT source lines and setup
// scripts. Origin/Label/Description are free-form text that is shell-quoted
// wherever it is emitted, so they are not constrained here.
func validateMetadata(m *RepoMetadata) error {
	if err := validateNames("distribution", m.Distributions); err != nil {
		return err
	}
	if err := validateNames("component", m.Components); err != nil {
		return err
	}
	if err := validateNames("architecture", m.Architectures); err != nil {
		return err
	}
	return nil
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
	if err := validateMetadata(&req.Metadata); err != nil {
		return err
	}

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
	if err := validateName("distribution", req.Distribution); err != nil {
		return err
	}
	if err := validateName("component", req.Component); err != nil {
		return err
	}
	for _, f := range req.DebFiles {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", f)
		}
	}
	return rm.withLock(ctx, func() error {
		return rm.upload(ctx, req)
	})
}

func (rm *RepositoryManager) upload(ctx context.Context, req *UploadRequest) error {
	if req.SigningKeys != nil {
		if err := req.SigningKeys.EnsureActiveUsable(); err != nil {
			return err
		}
	}

	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}

	// Distinguish "repository not initialized yet" (fall back to synthesized
	// metadata) from a real download/parse error (abort, so we never rewrite
	// Release with a degraded metadata set on a transient S3 failure).
	var metadataPtr *RepoMetadata
	metaExists, err := rm.store.Exists(ctx, rm.key(MetadataPath))
	if err != nil {
		return err
	}
	if metaExists {
		metadata, err := rm.loadMetadata(ctx)
		if err != nil {
			return err
		}
		metadataPtr = metadata

		if err := rm.requireKeyForSignedRepo(ctx, metadata, req.SigningKeys); err != nil {
			return err
		}
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

	// A batch can add new architectures partway through (via ensureArchitecture),
	// so earlier per-file Release writes may not reference them yet. Regenerate
	// Release once for every declared distribution against the final metadata.
	if uploaded > 0 && metadataPtr != nil {
		final, err := rm.loadMetadata(ctx)
		if err != nil {
			return err
		}
		for _, dist := range final.Distributions {
			if err := rm.updateRelease(ctx, dist, final, req.SigningKeys); err != nil {
				return err
			}
		}
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

	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithHeader([]string{"Package", "Version", "Architecture", "Component", "Distribution", "Description"}),
	)

	for _, r := range uniqueRows {
		_ = table.Append([]string{
			r.Name,
			r.Version,
			r.Architecture,
			r.Component,
			r.Distribution,
			truncate(r.Description, 60),
		})
	}
	_ = table.Render()
	fmt.Printf("\nTotal packages: %d\n", len(uniqueRows))

	return nil
}

func (rm *RepositoryManager) Remove(ctx context.Context, req *RemoveRequest) error {
	if err := validateName("distribution", req.Distribution); err != nil {
		return err
	}
	if err := validateName("component", req.Component); err != nil {
		return err
	}
	if err := validateName("architecture", req.Architecture); err != nil {
		return err
	}
	return rm.withLock(ctx, func() error {
		return rm.remove(ctx, req)
	})
}

func (rm *RepositoryManager) remove(ctx context.Context, req *RemoveRequest) error {
	if req.SigningKeys != nil {
		if err := req.SigningKeys.EnsureActiveUsable(); err != nil {
			return err
		}
	}

	metadata, err := rm.loadMetadata(ctx)
	if err != nil {
		return err
	}

	if err := rm.requireKeyForSignedRepo(ctx, metadata, req.SigningKeys); err != nil {
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

	// An "all" package lives in every concrete architecture's index; a concrete
	// package lives only in its own. Rewrite the indexes BEFORE deleting the
	// pool blob so a failure never leaves a dangling reference (Packages listing
	// a .deb that no longer exists → apt 404).
	targetArches, err := rm.targetArchitectures(req.Architecture, metadata)
	if err != nil {
		return err
	}

	for _, targetArch := range targetArches {
		indexRepo := apt.RepositoryPath{
			Distribution: req.Distribution,
			Component:    req.Component,
			Architecture: targetArch,
		}
		if err := rm.removeFromPackagesIndex(ctx, indexRepo, req); err != nil {
			return err
		}
	}

	if err := rm.updateRelease(ctx, req.Distribution, metadata, req.SigningKeys, rm.key(poolPath)); err != nil {
		return err
	}

	if err := rm.store.Delete(ctx, rm.key(poolPath)); err != nil {
		return err
	}

	fmt.Println("Package removed successfully!")
	return nil
}

// removeFromPackagesIndex rewrites a single binary-<arch> Packages index with
// the matching package stanza filtered out. Unlike the previous implementation,
// upload errors are propagated rather than swallowed.
func (rm *RepositoryManager) removeFromPackagesIndex(ctx context.Context, repo apt.RepositoryPath, req *RemoveRequest) error {
	packagesPath := repo.PackagesPath()
	existsPackages, err := rm.store.Exists(ctx, rm.key(packagesPath))
	if err != nil {
		return err
	}
	if !existsPackages {
		return nil
	}

	data, err := rm.store.Download(ctx, rm.key(packagesPath))
	if err != nil {
		return err
	}

	packages, err := apt.ParsePackages(string(data))
	if err != nil {
		return err
	}

	var filtered []apt.PackageEntry
	for _, p := range packages {
		if p.Package == req.Package && p.Version == req.Version && p.Architecture == req.Architecture {
			continue
		}
		filtered = append(filtered, p)
	}

	updated := apt.GeneratePackagesContent(filtered)
	if err := rm.store.UploadBytes(ctx, rm.key(packagesPath), []byte(updated), "text/plain"); err != nil {
		return err
	}

	gzBytes, err := apt.CompressGzip([]byte(updated))
	if err != nil {
		return err
	}
	return rm.store.UploadBytes(ctx, rm.key(repo.PackagesGzPath()), gzBytes, "application/gzip")
}

func (rm *RepositoryManager) RotateKey(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
	return rm.withLock(ctx, func() error {
		return rm.rotateKey(ctx, signingKeys)
	})
}

func (rm *RepositoryManager) rotateKey(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
	metadata, err := rm.loadMetadata(ctx)
	if err != nil {
		return err
	}

	if signingKeys.Active == nil {
		return fmt.Errorf("rotate-key requires an active private signing key")
	}
	if err := signingKeys.EnsureActiveUsable(); err != nil {
		return err
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

	// The pool path is architecture-independent (keyed by component + package).
	poolRepo := apt.RepositoryPath{
		Distribution: req.Distribution,
		Component:    req.Component,
		Architecture: architecture,
	}
	poolPath := fmt.Sprintf("%s/%s", poolRepo.PoolPath(packageName), filename)

	// Determine which binary-<arch> indexes this package belongs in. A concrete
	// architecture goes into its own index; an "all" package must appear in
	// every concrete architecture's index so apt clients configured for a
	// specific arch can see it.
	var fallbackMetadata RepoMetadata
	releaseMetadata := metadata
	if releaseMetadata == nil {
		fallbackMetadata = RepoMetadata{
			Distributions: []string{req.Distribution},
			Components:    []string{req.Component},
			Architectures: []string{architecture},
		}
		releaseMetadata = &fallbackMetadata
	}

	targetArches, err := rm.targetArchitectures(architecture, releaseMetadata)
	if err != nil {
		return "", err
	}

	// Existence is checked against the first target index; if the package is
	// already present there it is present in all of them (they are written
	// together), so a single check preserves the previous skip/force semantics.
	probeRepo := apt.RepositoryPath{
		Distribution: req.Distribution,
		Component:    req.Component,
		Architecture: targetArches[0],
	}
	exists, err := rm.packageExists(ctx, &probeRepo, packageName, version, architecture)
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

	entry := pkg.PackageEntry(poolPath)
	for _, targetArch := range targetArches {
		indexRepo := apt.RepositoryPath{
			Distribution: req.Distribution,
			Component:    req.Component,
			Architecture: targetArch,
		}
		if err := rm.appendToPackagesIndex(ctx, indexRepo, entry); err != nil {
			return "", err
		}
	}

	// Invalidate the pool blob too: on a force-overwrite the object bytes
	// changed under an unchanged key, so the CDN would otherwise serve stale
	// package contents while Packages advertises the new hash.
	err = rm.updateRelease(ctx, req.Distribution, releaseMetadata, req.SigningKeys, rm.key(poolPath))
	if err != nil {
		return "", err
	}

	return uploadOutcomeUploaded, nil
}

// targetArchitectures returns the concrete architectures whose Packages index a
// package should be written to. Concrete architectures map to themselves; "all"
// maps to every concrete architecture the repository declares.
func (rm *RepositoryManager) targetArchitectures(architecture string, metadata *RepoMetadata) ([]string, error) {
	if architecture != "all" {
		return []string{architecture}, nil
	}

	var concrete []string
	for _, arch := range metadata.Architectures {
		if arch != "all" {
			concrete = append(concrete, arch)
		}
	}
	if len(concrete) == 0 {
		return nil, fmt.Errorf("cannot upload architecture-independent (all) package: repository has no concrete architectures yet; upload an arch-specific package or run 'deb init' with architectures first")
	}
	return concrete, nil
}

// appendToPackagesIndex downloads the existing Packages index for a repository
// path, replaces any stanza with the same Package/Version/Architecture identity
// (so a --force re-upload does not leave a stale duplicate with old hashes),
// adds the new stanza, and writes both the plain and gzip variants.
//
// Storage errors are treated as hard failures: a transient download failure must
// never cause the index to be silently rewritten with only the new package,
// which would make every other package disappear from clients.
func (rm *RepositoryManager) appendToPackagesIndex(ctx context.Context, repo apt.RepositoryPath, entryText string) error {
	packagesPath := repo.PackagesPath()

	newEntries, err := apt.ParsePackages(entryText)
	if err != nil {
		return err
	}
	if len(newEntries) != 1 {
		return fmt.Errorf("expected exactly one new package stanza, got %d", len(newEntries))
	}
	newEntry := newEntries[0]

	var existing []apt.PackageEntry
	pkgExists, err := rm.store.Exists(ctx, rm.key(packagesPath))
	if err != nil {
		return err
	}
	if pkgExists {
		data, err := rm.store.Download(ctx, rm.key(packagesPath))
		if err != nil {
			return err
		}
		existing, err = apt.ParsePackages(string(data))
		if err != nil {
			return err
		}
	}

	var merged []apt.PackageEntry
	for _, p := range existing {
		if p.Package == newEntry.Package && p.Version == newEntry.Version && p.Architecture == newEntry.Architecture {
			continue
		}
		merged = append(merged, p)
	}
	merged = append(merged, newEntry)

	content := apt.GeneratePackagesContent(merged)
	if err := rm.store.UploadBytes(ctx, rm.key(packagesPath), []byte(content), "text/plain"); err != nil {
		return err
	}

	gzBytes, err := apt.CompressGzip([]byte(content))
	if err != nil {
		return err
	}

	return rm.store.UploadBytes(ctx, rm.key(repo.PackagesGzPath()), gzBytes, "application/gzip")
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

// requireKeyForSignedRepo refuses to mutate a repository that is currently
// signed (has an InRelease for any distribution) unless an active signing key is
// available. Rewriting a signed repo's Release without re-signing would leave a
// stale InRelease / Release.gpg that no longer matches, breaking apt clients.
func (rm *RepositoryManager) requireKeyForSignedRepo(ctx context.Context, metadata *RepoMetadata, signingKeys *openpgp.SigningKeys) error {
	if signingKeys != nil && signingKeys.Active != nil {
		return nil
	}
	for _, dist := range metadata.Distributions {
		inReleaseKey := rm.key(fmt.Sprintf("dists/%s/InRelease", dist))
		signed, err := rm.store.Exists(ctx, inReleaseKey)
		if err != nil {
			return err
		}
		if signed {
			return fmt.Errorf("repository %q is signed but no active signing key was provided; supply GPG_PRIVATE_KEY, --signing-key, or ./signing-key.gpg to re-sign it", dist)
		}
	}
	return nil
}

func (rm *RepositoryManager) updateRelease(ctx context.Context, distribution string, metadata *RepoMetadata, signingKeys *openpgp.SigningKeys, extraInvalidationPaths ...string) error {
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
				// An index that genuinely does not exist yet (e.g. a brand-new
				// arch mid-batch) is skipped, but any other storage error is a
				// hard failure: silently omitting an index would sign a reduced
				// Release and make those packages disappear from clients.
				exists, err := rm.store.Exists(ctx, rm.key(p.s3Path))
				if err != nil {
					return err
				}
				if !exists {
					continue
				}
				data, err := rm.store.Download(ctx, rm.key(p.s3Path))
				if err != nil {
					return err
				}
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
	inReleasePath := fmt.Sprintf("dists/%s/InRelease", distribution)

	if signingKeys != nil && signingKeys.Active != nil {
		// Compute both signatures up front so a signing failure aborts before any
		// object is published. Then publish in an order where the signature
		// artifacts and the public key are visible before the metadata that
		// points at them, and the client-preferred InRelease is published last:
		//   pubkeys -> Release.gpg -> Release -> InRelease
		// This avoids a window where a client fetches new metadata whose
		// signature or key has not landed yet.
		sig, err := signingKeys.Active.SignDetached(releaseData)
		if err != nil {
			return err
		}
		inRelease, err := signingKeys.Active.ClearSign(releaseData)
		if err != nil {
			return err
		}

		if err := rm.uploadPublicKeys(ctx, signingKeys); err != nil {
			return err
		}
		if err := rm.store.UploadBytes(ctx, rm.key(releasePath+".gpg"), []byte(sig), "application/pgp-signature"); err != nil {
			return err
		}
		if err := rm.store.UploadBytes(ctx, rm.key(releasePath), releaseData, "text/plain"); err != nil {
			return err
		}
		// InRelease is the inline clearsigned Release that modern apt prefers; it
		// is the canonical pointer, published last.
		if err := rm.store.UploadBytes(ctx, rm.key(inReleasePath), []byte(inRelease), "text/plain"); err != nil {
			return err
		}
	} else {
		// Unsigned: publish the plain Release, then remove any stale signature
		// artifacts left over from a previously-signed repository so clients do
		// not verify an old InRelease / Release.gpg against the new Release.
		if err := rm.store.UploadBytes(ctx, rm.key(releasePath), releaseData, "text/plain"); err != nil {
			return err
		}
		if err := rm.store.Delete(ctx, rm.key(releasePath+".gpg")); err != nil {
			return err
		}
		if err := rm.store.Delete(ctx, rm.key(inReleasePath)); err != nil {
			return err
		}
	}

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
		paths = append(paths, rm.key(inReleasePath))
		paths = append(paths, rm.key("pubkey.asc"))
		paths = append(paths, rm.key("pubkey.gpg"))
	}

	// Pool blobs written/removed by the caller (e.g. force-overwrite or remove)
	// must be invalidated too, or the CDN serves stale package bytes.
	paths = append(paths, extraInvalidationPaths...)

	rm.invalidateCDN(ctx, paths)

	return nil
}

// invalidateCDN purges the given paths from the configured CDN. A CDN
// misconfiguration (e.g. an unknown HAKOBIN_CDN_PURGE_TYPE) is surfaced as a
// loud warning rather than being silently ignored, but never fails the
// underlying repository operation.
func (rm *RepositoryManager) invalidateCDN(ctx context.Context, paths []string) {
	invalidator, err := cdn.FromEnv()
	if err != nil {
		fmt.Printf("Warning: CDN not configured correctly, skipping invalidation: %v\n", err)
		return
	}
	if invalidator == nil {
		return
	}
	if err := invalidator.Invalidate(ctx, rm.cfg, paths); err != nil {
		fmt.Printf("Warning: CDN invalidation failed: %v\n", err)
	}
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

	// Every value derived from repository metadata is emitted through a
	// single-quoted shell string so that a crafted Origin/Label/Description (or
	// any other field) cannot inject command substitution ($(...), backticks) or
	// break out of the echo when a user runs this script as root.
	var script strings.Builder
	fmt.Fprintf(&script, `#!/bin/bash
set -e

# APT Repository Setup Script
# Generated automatically by Hakobin Package

echo "Setting up APT repository: "%s
echo "Origin: "%s
echo "Description: "%s

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
`, shQuote(metadata.Label), shQuote(metadata.Origin), shQuote(metadata.Description), baseURL, repoID, repoID, repoID)

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
echo "Available distributions: "%s
echo "Available components: "%s
echo "Supported architectures: "%s
echo ""
echo "Repository GPG Key ID: %s"
`, shQuote(strings.Join(metadata.Distributions, ", ")),
		shQuote(strings.Join(metadata.Components, ", ")),
		shQuote(strings.Join(metadata.Architectures, ", ")),
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
	if err != nil {
		return false, err
	}
	if !exists {
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

// withLock runs fn while holding the repository-scoped lock, serializing
// metadata mutations so concurrent uploads/removes cannot lose updates.
func (rm *RepositoryManager) withLock(ctx context.Context, fn func() error) error {
	owner := uuid.NewString()
	lock, err := rm.store.AcquireLock(ctx, rm.key(LockPath), owner, lockTTLSeconds)
	if err != nil {
		return err
	}
	defer func() {
		if relErr := rm.store.ReleaseLock(ctx, lock); relErr != nil {
			fmt.Printf("Warning: failed to release repository lock: %v\n", relErr)
		}
	}()
	return fn()
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
