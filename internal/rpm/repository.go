package rpm

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/olekukonko/tablewriter"

	"hakobin/internal/cdn"
	"hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/storage"
)

const (
	// rpmLockPath is the lock object guarding RPM metadata mutations.
	rpmLockPath = "rpm/.hakobin.lock"
	// rpmLockTTLSeconds bounds how long a crashed operation can block others.
	rpmLockTTLSeconds = 300
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
	if err := validateRepoArch(req.Repo, req.Arch); err != nil {
		return err
	}
	return rm.withLock(ctx, func() error {
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
	})
}

func (rm *RpmRepositoryManager) Upload(ctx context.Context, req *RpmUploadRequest) error {
	if err := validateRepoArch(req.Repo, req.Arch); err != nil {
		return err
	}
	for _, f := range req.RpmFiles {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", f)
		}
	}
	return rm.withLock(ctx, func() error {
		return rm.upload(ctx, req)
	})
}

func (rm *RpmRepositoryManager) upload(ctx context.Context, req *RpmUploadRequest) error {
	if req.SigningKeys != nil {
		if err := req.SigningKeys.EnsureActiveUsable(); err != nil {
			return err
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
	var uploadedKeys []string

	for index, file := range req.RpmFiles {
		fmt.Printf("\n[%d/%d] Processing: %s\n", index+1, len(req.RpmFiles), file)
		outcome, key, err := rm.uploadOne(ctx, file, req)
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
				uploadedKeys = append(uploadedKeys, key)
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

		// Invalidate the (possibly overwritten) package blobs alongside metadata.
		if err := rm.writeMetadata(ctx, req.Repo, req.Arch, packages, req.SigningKeys, uploadedKeys...); err != nil {
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
	if err := validateRepoArch(req.Repo, req.Arch); err != nil {
		return err
	}
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
	if err := validateRepoArch(req.Repo, req.RepoArch); err != nil {
		return err
	}
	return rm.withLock(ctx, func() error {
		return rm.remove(ctx, req)
	})
}

func (rm *RpmRepositoryManager) remove(ctx context.Context, req *RpmRemoveRequest) error {
	if req.SigningKeys != nil {
		if err := req.SigningKeys.EnsureActiveUsable(); err != nil {
			return err
		}
	}

	packages, err := rm.loadPackages(ctx, req.Repo, req.RepoArch)
	if err != nil {
		return err
	}

	// Match on NEVR first, then prefer an exact architecture match; fall back to
	// a noarch package with the same NEVR so a user need not know whether the
	// package was arch-specific or arch-independent to remove it.
	var target *RpmPackage
	var noarchFallback *RpmPackage
	for i := range packages {
		p := packages[i]
		if p.Name != req.Package || p.Epoch != req.Epoch || p.Version != req.Version || p.Release != req.Release {
			continue
		}
		if p.Arch == req.Arch {
			target = &packages[i]
			break
		}
		if p.Arch == "noarch" {
			noarchFallback = &packages[i]
		}
	}
	if target == nil {
		target = noarchFallback
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

	removedBlob := rm.key(req.Repo, req.RepoArch, target.Location)

	// Rewrite metadata from the in-memory package set with the target filtered
	// out BEFORE deleting the blob. If metadata regeneration, signing, or upload
	// fails, the blob is still present, so the repo never advertises metadata
	// pointing at a missing RPM. The blob is deleted last.
	var remaining []RpmPackage
	for i := range packages {
		if &packages[i] == target {
			continue
		}
		remaining = append(remaining, packages[i])
	}

	if err := rm.writeMetadata(ctx, req.Repo, req.RepoArch, remaining, req.SigningKeys, removedBlob); err != nil {
		return err
	}

	if err := rm.store.Delete(ctx, removedBlob); err != nil {
		return err
	}

	fmt.Println("RPM package removed successfully!")
	return nil
}

func (rm *RpmRepositoryManager) RotateKey(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
	if signingKeys.Active == nil {
		return fmt.Errorf("rotate-key requires an active private signing key")
	}
	if err := signingKeys.EnsureActiveUsable(); err != nil {
		return err
	}
	return rm.withLock(ctx, func() error {
		return rm.rotateKey(ctx, signingKeys)
	})
}

func (rm *RpmRepositoryManager) rotateKey(ctx context.Context, signingKeys *openpgp.SigningKeys) error {
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

func (rm *RpmRepositoryManager) uploadOne(ctx context.Context, filePath string, req *RpmUploadRequest) (uploadOutcome, string, error) {
	pkg, err := FromPath(filePath)
	if err != nil {
		return "", "", err
	}

	if pkg.Arch != req.Arch && pkg.Arch != "noarch" {
		return "", "", fmt.Errorf("package architecture %s does not match repository architecture %s", pkg.Arch, req.Arch)
	}

	fmt.Printf("  Package: %s\n", pkg.Name)
	fmt.Printf("  Epoch: %s\n", pkg.Epoch)
	fmt.Printf("  Version: %s\n", pkg.Version)
	fmt.Printf("  Release: %s\n", pkg.Release)
	fmt.Printf("  Architecture: %s\n", pkg.Arch)
	fmt.Printf("  Generated filename: %s\n", pkg.Filename())

	key := rm.key(req.Repo, req.Arch, pkg.Location)
	exists, err := rm.store.Exists(ctx, key)
	if err != nil {
		return "", "", err
	}
	if exists {
		if req.Force {
			fmt.Println("  Force uploading (overwriting existing package)")
		} else {
			return uploadOutcomeSkipped, "", nil
		}
	}

	signedPkg, err := pkg.Signed(req.SigningKeys)
	if err != nil {
		return "", "", fmt.Errorf("failed to sign RPM package %s: %w", filePath, err)
	}

	err = rm.store.UploadBytes(ctx, key, signedPkg.Raw, "application/x-rpm")
	if err != nil {
		return "", "", err
	}

	return uploadOutcomeUploaded, key, nil
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

func (rm *RpmRepositoryManager) writeMetadata(ctx context.Context, repo, arch string, packages []RpmPackage, signingKeys *openpgp.SigningKeys, extraInvalidationPaths ...string) error {
	metadata, err := GenerateRepositoryMetadata(packages)
	if err != nil {
		return err
	}

	repodataPrefix := rm.key(repo, arch, "repodata/")
	pubKeyPath := rm.key(repo, arch, RpmPublicKeyName)

	// Capture the files Hakobin owned in the PREVIOUS repomd.xml before we
	// overwrite it, so pruning only removes our own superseded primary/filelists/
	// other files and never third-party repodata (sqlite *_db, updateinfo,
	// modules, comps, …) that another producer may have added.
	prevOwned, err := rm.previouslyOwnedRepodata(ctx, repodataPrefix)
	if err != nil {
		return err
	}

	// Sign first so a signing failure aborts before anything is published.
	repomdBytes := []byte(metadata.Repomd)
	sigBytes, pubKeyBytes, err := SignRpmMetadata(repomdBytes, signingKeys)
	if err != nil {
		return err
	}
	sigPath := repodataPrefix + "repomd.xml.asc"

	// Publish the checksum-named data files first: they are what repomd.xml will
	// point at, so they must exist before repomd.xml becomes visible.
	files := []*MetadataFile{&metadata.Primary, &metadata.Filelists, &metadata.Other}
	for _, file := range files {
		err = rm.store.UploadBytes(ctx, repodataPrefix+file.Filename, file.Data, "application/gzip")
		if err != nil {
			return err
		}
	}

	// Publish (or clear) the signature and public key before repomd.xml so a
	// client that reads the new repomd.xml never sees it paired with a stale or
	// missing signature.
	if sigBytes != nil {
		if err := rm.store.UploadBytes(ctx, pubKeyPath, pubKeyBytes, "application/pgp-keys"); err != nil {
			return err
		}
		if err := rm.store.UploadBytes(ctx, sigPath, sigBytes, "application/pgp-signature"); err != nil {
			return err
		}
	} else {
		// Repository is now unsigned: remove any stale signature/public key from
		// a previous signed run so clients don't verify a stale signature (or an
		// old key) against the freshly written metadata.
		if err := rm.store.Delete(ctx, sigPath); err != nil {
			return err
		}
		if err := rm.store.Delete(ctx, pubKeyPath); err != nil {
			return err
		}
	}

	// repomd.xml is the canonical pointer; publish it last.
	if err := rm.store.UploadBytes(ctx, repodataPrefix+"repomd.xml", repomdBytes, "application/xml"); err != nil {
		return err
	}

	// Garbage-collect ONLY the checksum-named primary/filelists/other files
	// Hakobin previously published (they accumulate on every regeneration).
	// Anything else under repodata/ — including third-party metadata — is left
	// untouched, and the just-written files are never deleted.
	current := map[string]bool{
		repodataPrefix + metadata.Primary.Filename:   true,
		repodataPrefix + metadata.Filelists.Filename: true,
		repodataPrefix + metadata.Other.Filename:     true,
	}
	for _, old := range prevOwned {
		if current[old] {
			continue
		}
		if err := rm.store.Delete(ctx, old); err != nil {
			return err
		}
	}

	paths := []string{
		repodataPrefix + "repomd.xml",
		repodataPrefix + metadata.Primary.Filename,
		repodataPrefix + metadata.Filelists.Filename,
		repodataPrefix + metadata.Other.Filename,
	}

	if signingKeys != nil && signingKeys.Active != nil {
		paths = append(paths, sigPath)
		paths = append(paths, pubKeyPath)
	}
	paths = append(paths, extraInvalidationPaths...)

	rm.invalidateCDN(ctx, paths)

	return nil
}

// previouslyOwnedRepodata reads the current repomd.xml (if any) and returns the
// full keys of the primary/filelists/other data files it references. Those are
// the only files Hakobin considers its own and is allowed to prune when it
// regenerates metadata; unknown data types are preserved.
func (rm *RpmRepositoryManager) previouslyOwnedRepodata(ctx context.Context, repodataPrefix string) ([]string, error) {
	repomdKey := repodataPrefix + "repomd.xml"
	exists, err := rm.store.Exists(ctx, repomdKey)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	data, err := rm.store.Download(ctx, repomdKey)
	if err != nil {
		return nil, err
	}
	hrefs, err := hakobinOwnedRepomdHrefs(data)
	if err != nil {
		// A repomd.xml we cannot parse is treated as owning nothing rather than
		// as a reason to fail the whole operation; we simply won't prune it.
		return nil, nil
	}

	var owned []string
	for _, href := range hrefs {
		// href is repodata-relative like "repodata/<sum>-primary.xml.gz".
		trimmed := strings.TrimPrefix(href, "repodata/")
		owned = append(owned, repodataPrefix+trimmed)
	}
	return owned, nil
}

// hakobinOwnedRepomdHrefs extracts the <location href> values for the data
// types Hakobin manages (primary, filelists, other) from a repomd.xml.
func hakobinOwnedRepomdHrefs(repomd []byte) ([]string, error) {
	type location struct {
		Href string `xml:"href,attr"`
	}
	type dataEntry struct {
		Type     string   `xml:"type,attr"`
		Location location `xml:"location"`
	}
	type repomdDoc struct {
		Data []dataEntry `xml:"data"`
	}
	var doc repomdDoc
	if err := xml.Unmarshal(repomd, &doc); err != nil {
		return nil, err
	}
	managed := map[string]bool{"primary": true, "filelists": true, "other": true}
	var hrefs []string
	for _, d := range doc.Data {
		if managed[d.Type] && d.Location.Href != "" {
			hrefs = append(hrefs, d.Location.Href)
		}
	}
	return hrefs, nil
}

// invalidateCDN purges the given paths from the configured CDN, surfacing a
// misconfiguration as a loud warning rather than silently skipping.
func (rm *RpmRepositoryManager) invalidateCDN(ctx context.Context, paths []string) {
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

func (rm *RpmRepositoryManager) repoPrefix(repo, arch string) string {
	return fmt.Sprintf("%s/%s/%s", RpmPrefix, repo, arch)
}

func (rm *RpmRepositoryManager) key(repo, arch, path string) string {
	return fmt.Sprintf("%s/%s", rm.repoPrefix(repo, arch), strings.TrimPrefix(path, "/"))
}

// withLock runs fn while holding the RPM repository lock, serializing metadata
// mutations so concurrent uploads/removes cannot corrupt repodata.
func (rm *RpmRepositoryManager) withLock(ctx context.Context, fn func() error) error {
	owner := uuid.NewString()
	lock, err := rm.store.AcquireLock(ctx, rpmLockPath, owner, rpmLockTTLSeconds)
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

// segmentRe matches a single safe path segment for --repo/--arch: no slashes,
// no dots-only, no path traversal.
var segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// validateRepoArch rejects --repo/--arch values that could escape the
// rpm/<repo>/<arch>/ prefix and overwrite other repositories' objects.
func validateRepoArch(repo, arch string) error {
	for _, s := range []struct{ name, val string }{{"repo", repo}, {"arch", arch}} {
		if s.val == "" {
			return fmt.Errorf("%s must not be empty", s.name)
		}
		if s.val == "." || s.val == ".." || !segmentRe.MatchString(s.val) {
			return fmt.Errorf("invalid %s %q: must be a single path segment", s.name, s.val)
		}
	}
	return nil
}

func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
