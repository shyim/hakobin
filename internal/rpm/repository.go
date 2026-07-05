package rpm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/olekukonko/tablewriter"

	"hakobin/internal/cdn"
	"hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/storage"
)

type RpmRepositoryManager struct {
	cfg   *config.Config
	store storage.Store
}

func NewRpmRepositoryManager(cfg *config.Config, store storage.Store) *RpmRepositoryManager {
	return &RpmRepositoryManager{
		cfg:   cfg,
		store: store,
	}
}

type RpmInitRequest struct {
	Repo        string
	Arch        string
	SigningKeys *openpgp.SigningKeys
}

type RpmUploadRequest struct {
	RpmFiles    []string
	Repo        string
	Arch        string
	Force       bool
	SigningKeys *openpgp.SigningKeys
}

type RpmListRequest struct {
	Repo        string
	Arch        string
	PackageName *string
}

type RpmRemoveRequest struct {
	Package     string
	Epoch       string
	Version     string
	Release     string
	Arch        string
	Repo        string
	RepoArch    string
	Force       bool
	SigningKeys *openpgp.SigningKeys
}

func (rm *RpmRepositoryManager) Init(ctx context.Context, req *RpmInitRequest) error {
	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}
	fmt.Printf("Initializing RPM repository '%s' for %s in S3 bucket: %s\n", req.Repo, req.Arch, bucket)

	if err := rm.writeMetadata(ctx, req.Repo, req.Arch, nil, req.SigningKeys); err != nil {
		return err
	}

	baseURL, err := rm.cfg.RepositoryBaseURL()
	if err != nil {
		return err
	}

	fmt.Println("RPM repository initialized successfully.")
	fmt.Printf("Repository base URL: %s/%s\n", strings.TrimSuffix(baseURL, "/"), rm.repoPrefix(req.Repo, req.Arch))
	return nil
}

func (rm *RpmRepositoryManager) Upload(ctx context.Context, req *RpmUploadRequest) error {
	for _, f := range req.RpmFiles {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", f)
		}
	}

	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}

	fmt.Printf("Uploading %d RPM package(s) to S3 bucket: %s\n", len(req.RpmFiles), bucket)
	fmt.Printf("Repository: %s\n", req.Repo)
	fmt.Printf("Repository architecture: %s\n", req.Arch)

	uploaded := 0
	skipped := 0
	failed := 0
	var firstErr error

	for index, file := range req.RpmFiles {
		fmt.Printf("\n[%d/%d] Processing: %s\n", index+1, len(req.RpmFiles), file)
		outcome, err := rm.uploadOne(ctx, file, req)
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

	if uploaded > 0 {
		packages, err := rm.loadPackages(ctx, req.Repo, req.Arch)
		if err != nil {
			return err
		}

		if err := rm.writeMetadata(ctx, req.Repo, req.Arch, packages, req.SigningKeys); err != nil {
			return err
		}
	}

	fmt.Println("\n==================================================")
	fmt.Println("RPM Upload Summary:")
	fmt.Printf("Uploaded: %d\n", uploaded)
	if skipped > 0 {
		fmt.Printf("Skipped: %d\n", skipped)
	}
	if failed > 0 {
		fmt.Printf("Failed: %d\n", failed)
		if firstErr != nil {
			fmt.Printf("\nFirst error encountered: %v\n", firstErr)
		}
		fmt.Println("Use --force flag to overwrite existing packages.")
		return fmt.Errorf("%d RPM package(s) failed to upload", failed)
	}

	fmt.Println("All RPM packages processed successfully!")
	return nil
}

func (rm *RpmRepositoryManager) List(ctx context.Context, req *RpmListRequest) error {
	packages, err := rm.loadPackages(ctx, req.Repo, req.Arch)
	if err != nil {
		return err
	}

	var filtered []RpmPackage
	for _, p := range packages {
		if req.PackageName != nil && *req.PackageName != "" {
			filter := strings.ToLower(*req.PackageName)
			if !strings.Contains(strings.ToLower(p.Name), filter) {
				continue
			}
		}
		filtered = append(filtered, p)
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Name != filtered[j].Name {
			return filtered[i].Name < filtered[j].Name
		}
		if filtered[i].Version != filtered[j].Version {
			return filtered[i].Version < filtered[j].Version
		}
		if filtered[i].Release != filtered[j].Release {
			return filtered[i].Release < filtered[j].Release
		}
		return filtered[i].Arch < filtered[j].Arch
	})

	if len(filtered) == 0 {
		fmt.Println("No RPM packages found matching the criteria.")
		return nil
	}

	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithHeader([]string{"Package", "Epoch", "Version", "Release", "Arch", "Summary"}),
	)

	for _, p := range filtered {
		_ = table.Append([]string{
			p.Name,
			p.Epoch,
			p.Version,
			p.Release,
			p.Arch,
			truncate(p.Summary, 60),
		})
	}
	_ = table.Render()
	fmt.Printf("\nTotal RPM packages: %d\n", len(filtered))

	return nil
}

func (rm *RpmRepositoryManager) Remove(ctx context.Context, req *RpmRemoveRequest) error {
	packages, err := rm.loadPackages(ctx, req.Repo, req.RepoArch)
	if err != nil {
		return err
	}

	var target *RpmPackage
	for _, p := range packages {
		if p.Name == req.Package && p.Epoch == req.Epoch && p.Version == req.Version && p.Release == req.Release && p.Arch == req.Arch {
			target = &p
			break
		}
	}

	if target == nil {
		return fmt.Errorf("package %s epoch %s version %s release %s architecture %s not found in %s/%s",
			req.Package, req.Epoch, req.Version, req.Release, req.Arch, req.Repo, req.RepoArch)
	}

	fmt.Printf("Package: %s\n", target.Name)
	fmt.Printf("Epoch: %s\n", target.Epoch)
	fmt.Printf("Version: %s\n", target.Version)
	fmt.Printf("Release: %s\n", target.Release)
	fmt.Printf("Architecture: %s\n", target.Arch)
	fmt.Printf("Repository: %s\n", req.Repo)
	fmt.Printf("Repository architecture: %s\n", req.RepoArch)
	fmt.Printf("Found package at: %s\n", target.Location)

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

	if err := rm.store.Delete(ctx, rm.key(req.Repo, req.RepoArch, target.Location)); err != nil {
		return err
	}

	// Reload packages to generate metadata
	packages, err = rm.loadPackages(ctx, req.Repo, req.RepoArch)
	if err != nil {
		return err
	}

	if err := rm.writeMetadata(ctx, req.Repo, req.RepoArch, packages, req.SigningKeys); err != nil {
		return err
	}

	fmt.Println("RPM package removed successfully!")
	return nil
}

func (rm *RpmRepositoryManager) RotateKey(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
	if signingKeys.Active == nil {
		return fmt.Errorf("rotate-key requires an active private signing key")
	}

	repos, err := rm.discoverRepositories(ctx)
	if err != nil {
		return err
	}

	if len(repos) == 0 {
		return fmt.Errorf("no RPM repositories found under %s/", RpmPrefix)
	}

	bucket, err := rm.cfg.Bucket()
	if err != nil {
		return err
	}

	fmt.Printf("Rotating signing keys for %d RPM repository target(s) in S3 bucket: %s\n", len(repos), bucket)

	for _, repo := range repos {
		fmt.Printf("Re-signing RPM repository: %s/%s\n", repo.Repo, repo.Arch)
		packages, err := rm.loadPackages(ctx, repo.Repo, repo.Arch)
		if err != nil {
			return err
		}

		var signedPackages []RpmPackage
		for _, p := range packages {
			signed, err := p.Signed(signingKeys)
			if err != nil {
				return fmt.Errorf("failed to sign RPM package %s: %w", p.Location, err)
			}
			signedPackages = append(signedPackages, *signed)
		}

		for _, p := range signedPackages {
			err = rm.store.UploadBytes(ctx, rm.key(repo.Repo, repo.Arch, p.Location), p.Raw, "application/x-rpm")
			if err != nil {
				return err
			}
		}

		err = rm.writeMetadata(ctx, repo.Repo, repo.Arch, signedPackages, signingKeys)
		if err != nil {
			return err
		}
	}

	fmt.Println("RPM signing keys rotated successfully.")
	return nil
}

type uploadOutcome string

const (
	uploadOutcomeUploaded uploadOutcome = "uploaded"
	uploadOutcomeSkipped  uploadOutcome = "skipped"
)

func (rm *RpmRepositoryManager) uploadOne(ctx context.Context, filePath string, req *RpmUploadRequest) (uploadOutcome, error) {
	pkg, err := FromPath(filePath)
	if err != nil {
		return "", err
	}

	if pkg.Arch != req.Arch && pkg.Arch != "noarch" {
		return "", fmt.Errorf("package architecture %s does not match repository architecture %s", pkg.Arch, req.Arch)
	}

	fmt.Printf("  Package: %s\n", pkg.Name)
	fmt.Printf("  Epoch: %s\n", pkg.Epoch)
	fmt.Printf("  Version: %s\n", pkg.Version)
	fmt.Printf("  Release: %s\n", pkg.Release)
	fmt.Printf("  Architecture: %s\n", pkg.Arch)
	fmt.Printf("  Generated filename: %s\n", pkg.Filename())

	key := rm.key(req.Repo, req.Arch, pkg.Location)
	exists, err := rm.store.Exists(ctx, key)
	if err == nil && exists {
		if req.Force {
			fmt.Println("  Force uploading (overwriting existing package)")
		} else {
			return uploadOutcomeSkipped, nil
		}
	}

	signedPkg, err := pkg.Signed(req.SigningKeys)
	if err != nil {
		return "", fmt.Errorf("failed to sign RPM package %s: %w", filePath, err)
	}

	err = rm.store.UploadBytes(ctx, key, signedPkg.Raw, "application/x-rpm")
	if err != nil {
		return "", err
	}

	return uploadOutcomeUploaded, nil
}

func (rm *RpmRepositoryManager) loadPackages(ctx context.Context, repo, arch string) ([]RpmPackage, error) {
	prefix := rm.key(repo, arch, "Packages/")
	keys, err := rm.store.ListKeys(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var packages []RpmPackage
	for _, key := range keys {
		if !strings.HasSuffix(key, ".rpm") {
			continue
		}
		data, err := rm.store.Download(ctx, key)
		if err != nil {
			return nil, err
		}

		location := strings.TrimPrefix(key, rm.repoPrefix(repo, arch)+"/")
		pkg, err := FromBytes(location, data)
		if err != nil {
			return nil, err
		}
		packages = append(packages, *pkg)
	}

	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Location < packages[j].Location
	})

	return packages, nil
}

func (rm *RpmRepositoryManager) discoverRepositories(ctx context.Context) ([]RpmRepositoryId, error) {
	prefix := fmt.Sprintf("%s/", RpmPrefix)
	keys, err := rm.store.ListKeys(ctx, prefix)
	if err != nil {
		return nil, err
	}

	seen := make(map[RpmRepositoryId]bool)
	var repos []RpmRepositoryId
	for _, key := range keys {
		if id := RpmRepositoryFromKey(key); id != nil {
			if !seen[*id] {
				seen[*id] = true
				repos = append(repos, *id)
			}
		}
	}
	return repos, nil
}

func (rm *RpmRepositoryManager) writeMetadata(ctx context.Context, repo, arch string, packages []RpmPackage, signingKeys *openpgp.SigningKeys) error {
	metadata, err := GenerateRepositoryMetadata(packages)
	if err != nil {
		return err
	}

	repodataPrefix := rm.key(repo, arch, "repodata/")
	pubKeyPath := rm.key(repo, arch, RpmPublicKeyName)

	files := []*MetadataFile{&metadata.Primary, &metadata.Filelists, &metadata.Other}
	for _, file := range files {
		err = rm.store.UploadBytes(ctx, repodataPrefix+file.Filename, file.Data, "application/gzip")
		if err != nil {
			return err
		}
	}

	repomdBytes := []byte(metadata.Repomd)
	err = rm.store.UploadBytes(ctx, repodataPrefix+"repomd.xml", repomdBytes, "application/xml")
	if err != nil {
		return err
	}

	sigBytes, pubKeyBytes, err := SignRpmMetadata(repomdBytes, signingKeys)
	if err != nil {
		return err
	}

	if sigBytes != nil {
		err = rm.store.UploadBytes(ctx, repodataPrefix+"repomd.xml.asc", sigBytes, "application/pgp-signature")
		if err != nil {
			return err
		}
		err = rm.store.UploadBytes(ctx, pubKeyPath, pubKeyBytes, "application/pgp-keys")
		if err != nil {
			return err
		}
	}

	invalidator, err := cdn.FromEnv()
	if err == nil && invalidator != nil {
		paths := []string{
			repodataPrefix + "repomd.xml",
			repodataPrefix + metadata.Primary.Filename,
			repodataPrefix + metadata.Filelists.Filename,
			repodataPrefix + metadata.Other.Filename,
		}

		if signingKeys != nil && signingKeys.Active != nil {
			paths = append(paths, repodataPrefix+"repomd.xml.asc")
			paths = append(paths, pubKeyPath)
		}

		if err := invalidator.Invalidate(ctx, rm.cfg, paths); err != nil {
			fmt.Printf("Warning: CDN invalidation failed: %v\n", err)
		}
	}

	return nil
}

func (rm *RpmRepositoryManager) repoPrefix(repo, arch string) string {
	return fmt.Sprintf("%s/%s/%s", RpmPrefix, repo, arch)
}

func (rm *RpmRepositoryManager) key(repo, arch, path string) string {
	return fmt.Sprintf("%s/%s", rm.repoPrefix(repo, arch), strings.TrimPrefix(path, "/"))
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-3]) + "..."
}
