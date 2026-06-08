package repository

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/storage"
)

func TestAptWorkflowUploadRemoveAndRotateKeyUsesMemoryStore(t *testing.T) {
	cfg := &config.Config{
		S3AccessKeyID:     "test-access-key",
		S3SecretAccessKey: "test-secret-key",
		S3BucketName:      "test-bucket",
		S3Region:          "us-east-1",
		S3UsePathStyle:    true,
		PublicURL:         "https://packages.example.test",
	}

	store := storage.NewMemoryStore()
	manager := NewRepositoryManager(cfg, store)

	oldKey, err := openpgp.GenerateKeyPair("Hakobin Old", "gpg@example.com", "test", 0)
	require.NoError(t, err)
	oldKeys := openpgp.NewSigningKeys(oldKey, nil)

	ctx := context.Background()
	req := &InitRequest{
		Metadata: RepoMetadata{
			Origin:        "Acme Corp",
			Label:         "Acme APT Repo",
			Description:   "Acme's custom packages",
			Distributions: []string{"stable"},
			Components:    []string{"main"},
			Architectures: []string{"amd64", "all"},
		},
		KeyName:            "GPG Key",
		KeyEmail:           "gpg@example.com",
		KeyExpirationYears: 0,
	}

	err = manager.Init(ctx, req)
	require.NoError(t, err)

	// Verify init files exist in storage
	metaData, ok := store.Body("deb/apt-repo.json")
	require.True(t, ok)

	var meta RepoMetadata
	err = json.Unmarshal(metaData, &meta)
	require.NoError(t, err)
	assert.Equal(t, "Acme Corp", meta.Origin)

	// Create a temp deb file
	controlData := []byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nDescription: Demo package\n")
	debBytes := testDeb(controlData)

	tempDeb, err := os.CreateTemp("", "demo-*.deb")
	require.NoError(t, err)
	defer os.Remove(tempDeb.Name())
	_, err = tempDeb.Write(debBytes)
	require.NoError(t, err)
	tempDeb.Close()

	// Upload
	uploadReq := &UploadRequest{
		DebFiles:     []string{tempDeb.Name()},
		Distribution: "stable",
		Component:    "main",
		Force:        false,
		SigningKeys:  oldKeys,
	}

	err = manager.Upload(ctx, uploadReq)
	require.NoError(t, err)

	packageKey := "deb/pool/main/d/demo/demo_1.0.0_amd64.deb"
	_, ok = store.Body(packageKey)
	assert.True(t, ok)

	// Rotate key
	newKey, err := openpgp.GenerateKeyPair("Hakobin New", "gpg@example.com", "test", 0)
	require.NoError(t, err)

	oldCert, err := oldKey.PublicKeyCert()
	require.NoError(t, err)
	rotatedKeys := openpgp.NewSigningKeys(newKey, []*openpgp.PublicKeyCert{oldCert})

	err = manager.RotateKey(ctx, rotatedKeys)
	require.NoError(t, err)

	// Remove package
	removeReq := &RemoveRequest{
		Package:      "demo",
		Version:      "1.0.0",
		Architecture: "amd64",
		Distribution: "stable",
		Component:    "main",
		Force:        true,
		SigningKeys:  rotatedKeys,
	}

	err = manager.Remove(ctx, removeReq)
	require.NoError(t, err)

	_, ok = store.Body(packageKey)
	assert.False(t, ok)
}

func TestUploadAllArchitecturePackageIsVisibleInConcreteArch(t *testing.T) {
	cfg := &config.Config{
		S3AccessKeyID:     "test-access-key",
		S3SecretAccessKey: "test-secret-key",
		S3BucketName:      "test-bucket",
		S3Region:          "us-east-1",
		S3UsePathStyle:    true,
		PublicURL:         "https://packages.example.test",
	}

	store := storage.NewMemoryStore()
	manager := NewRepositoryManager(cfg, store)

	key, err := openpgp.GenerateKeyPair("Hakobin", "gpg@example.com", "test", 0)
	require.NoError(t, err)
	keys := openpgp.NewSigningKeys(key, nil)

	ctx := context.Background()
	require.NoError(t, manager.Init(ctx, &InitRequest{
		Metadata: RepoMetadata{
			Distributions: []string{"stable"},
			Components:    []string{"main"},
			Architectures: []string{"amd64", "arm64", "all"},
		},
		KeyName:  "GPG Key",
		KeyEmail: "gpg@example.com",
	}))

	controlData := []byte("Package: docs\nVersion: 2.0.0\nArchitecture: all\nDescription: Arch-independent package\n")
	tempDeb, err := os.CreateTemp("", "docs-*.deb")
	require.NoError(t, err)
	defer os.Remove(tempDeb.Name())
	_, err = tempDeb.Write(testDeb(controlData))
	require.NoError(t, err)
	tempDeb.Close()

	require.NoError(t, manager.Upload(ctx, &UploadRequest{
		DebFiles:     []string{tempDeb.Name()},
		Distribution: "stable",
		Component:    "main",
		SigningKeys:  keys,
	}))

	// The all package must appear in every concrete architecture's index so
	// apt clients configured for a specific arch can see it.
	for _, arch := range []string{"amd64", "arm64"} {
		body, ok := store.Body(fmt.Sprintf("deb/dists/stable/main/binary-%s/Packages", arch))
		require.True(t, ok, "missing Packages index for %s", arch)
		assert.Contains(t, string(body), "Package: docs", "docs (all) not visible in binary-%s", arch)
	}

	// The Release file must reference the concrete-arch Packages indexes.
	release, ok := store.Body("deb/dists/stable/Release")
	require.True(t, ok)
	assert.Contains(t, string(release), "main/binary-amd64/Packages")
	assert.Contains(t, string(release), "main/binary-arm64/Packages")

	// A clearsigned InRelease must be published for modern apt.
	inRelease, ok := store.Body("deb/dists/stable/InRelease")
	require.True(t, ok)
	assert.Contains(t, string(inRelease), "-----BEGIN PGP SIGNED MESSAGE-----")
	assert.Contains(t, string(inRelease), "main/binary-amd64/Packages")

	// Removal must clear it from all concrete indexes.
	require.NoError(t, manager.Remove(ctx, &RemoveRequest{
		Package:      "docs",
		Version:      "2.0.0",
		Architecture: "all",
		Distribution: "stable",
		Component:    "main",
		Force:        true,
		SigningKeys:  keys,
	}))

	for _, arch := range []string{"amd64", "arm64"} {
		body, ok := store.Body(fmt.Sprintf("deb/dists/stable/main/binary-%s/Packages", arch))
		require.True(t, ok)
		assert.NotContains(t, string(body), "Package: docs", "docs still present in binary-%s after remove", arch)
	}
}

func TestListPackages(t *testing.T) {
	cfg := &config.Config{
		S3BucketName: "test-bucket", S3Region: "us-east-1", S3UsePathStyle: true,
		S3AccessKeyID: "k", S3SecretAccessKey: "s",
		PublicURL: "https://packages.example.test",
	}
	store := storage.NewMemoryStore()
	manager := NewRepositoryManager(cfg, store)
	ctx := context.Background()

	key, err := openpgp.GenerateKeyPair("Hakobin", "gpg@example.com", "test", 0)
	require.NoError(t, err)
	keys := openpgp.NewSigningKeys(key, nil)

	require.NoError(t, manager.Init(ctx, &InitRequest{
		Metadata: RepoMetadata{
			Distributions: []string{"stable"},
			Components:    []string{"main"},
			Architectures: []string{"amd64"},
		},
		KeyName: "GPG Key", KeyEmail: "gpg@example.com",
	}))

	control := []byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nDescription: Demo package\n")
	tempDeb, err := os.CreateTemp("", "demo-*.deb")
	require.NoError(t, err)
	defer os.Remove(tempDeb.Name())
	_, err = tempDeb.Write(testDeb(control))
	require.NoError(t, err)
	tempDeb.Close()

	require.NoError(t, manager.Upload(ctx, &UploadRequest{
		DebFiles: []string{tempDeb.Name()}, Distribution: "stable", Component: "main", SigningKeys: keys,
	}))

	// List with no filter, with a matching name filter, and with a non-matching
	// filter must all complete without error.
	require.NoError(t, manager.ListPackages(ctx, nil, nil, nil))

	name := "demo"
	require.NoError(t, manager.ListPackages(ctx, nil, nil, &name))

	missing := "nonexistent"
	require.NoError(t, manager.ListPackages(ctx, nil, nil, &missing))

	dist := "stable"
	comp := "main"
	require.NoError(t, manager.ListPackages(ctx, &dist, &comp, nil))
}

func TestForceUploadReplacesStanzaInsteadOfDuplicating(t *testing.T) {
	cfg := &config.Config{
		S3BucketName: "test-bucket", S3Region: "us-east-1", S3UsePathStyle: true,
		S3AccessKeyID: "k", S3SecretAccessKey: "s",
		PublicURL: "https://packages.example.test",
	}
	store := storage.NewMemoryStore()
	manager := NewRepositoryManager(cfg, store)
	ctx := context.Background()

	key, err := openpgp.GenerateKeyPair("Hakobin", "gpg@example.com", "test", 0)
	require.NoError(t, err)
	keys := openpgp.NewSigningKeys(key, nil)

	require.NoError(t, manager.Init(ctx, &InitRequest{
		Metadata: RepoMetadata{
			Distributions: []string{"stable"},
			Components:    []string{"main"},
			Architectures: []string{"amd64"},
		},
		KeyName: "GPG Key", KeyEmail: "gpg@example.com",
	}))

	writeDeb := func(control []byte) string {
		f, err := os.CreateTemp("", "demo-*.deb")
		require.NoError(t, err)
		t.Cleanup(func() { os.Remove(f.Name()) })
		_, err = f.Write(testDeb(control))
		require.NoError(t, err)
		f.Close()
		return f.Name()
	}

	// First upload of demo 1.0.0/amd64.
	first := writeDeb([]byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nDescription: First build\n"))
	require.NoError(t, manager.Upload(ctx, &UploadRequest{
		DebFiles: []string{first}, Distribution: "stable", Component: "main", SigningKeys: keys,
	}))

	// Force re-upload of the SAME identity with different bytes (different
	// Description → different control tar → different hashes).
	second := writeDeb([]byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nDescription: Second build changed bytes\n"))
	require.NoError(t, manager.Upload(ctx, &UploadRequest{
		DebFiles: []string{second}, Distribution: "stable", Component: "main", Force: true, SigningKeys: keys,
	}))

	body, ok := store.Body("deb/dists/stable/main/binary-amd64/Packages")
	require.True(t, ok)
	got := string(body)

	// Exactly one stanza for demo must remain, and it must be the new build.
	assert.Equal(t, 1, bytes.Count([]byte(got), []byte("Package: demo\n")),
		"force re-upload must replace the stanza, not duplicate it:\n%s", got)
	assert.Contains(t, got, "Second build changed bytes")
	assert.NotContains(t, got, "First build")
}

func TestSetupScriptNeutralizesShellInjectionInMetadata(t *testing.T) {
	cfg := &config.Config{
		S3BucketName: "test-bucket", S3Region: "us-east-1", S3UsePathStyle: true,
		S3AccessKeyID: "k", S3SecretAccessKey: "s",
		PublicURL: "https://packages.example.test",
	}
	store := storage.NewMemoryStore()
	manager := NewRepositoryManager(cfg, store)
	ctx := context.Background()

	// Origin/Label/Description are free-form and end up inside the generated
	// setup script that users run as root. A command substitution in them must
	// NOT survive as executable shell.
	require.NoError(t, manager.Init(ctx, &InitRequest{
		Metadata: RepoMetadata{
			Origin:        "Acme $(touch /tmp/pwned)",
			Label:         "Repo `id`",
			Description:   "end'; rm -rf / #",
			Distributions: []string{"stable"},
			Components:    []string{"main"},
			Architectures: []string{"amd64"},
		},
		KeyName: "GPG Key", KeyEmail: "gpg@example.com",
	}))

	body, ok := store.Body("deb/setup.sh")
	require.True(t, ok)
	script := string(body)

	// The dangerous values must appear only inside single-quoted shell words,
	// where no expansion happens. Their presence in that exact quoted form proves
	// they are inert.
	assert.Contains(t, script, `'Acme $(touch /tmp/pwned)'`)
	assert.Contains(t, script, "'Repo `id`'")
	// The single-quote in the description must be escaped via the '\'' idiom so
	// it cannot terminate the quoted string early.
	assert.Contains(t, script, `end'\''; rm -rf / #`)

	// Sanity: run the script through bash -n to confirm it is syntactically valid
	// shell (an injection that broke out would usually produce a syntax error or
	// change structure). We only parse, never execute.
	requireValidBash(t, script)
}

func requireValidBash(t *testing.T, script string) {
	t.Helper()
	f, err := os.CreateTemp("", "setup-*.sh")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	_, err = f.WriteString(script)
	require.NoError(t, err)
	f.Close()

	out, err := exec.Command("bash", "-n", f.Name()).CombinedOutput()
	require.NoError(t, err, "generated setup.sh is not valid bash:\n%s", out)
}

func TestUploadRejectsInvalidNames(t *testing.T) {
	cfg := &config.Config{
		S3BucketName: "test-bucket", S3Region: "us-east-1", S3UsePathStyle: true,
		S3AccessKeyID: "k", S3SecretAccessKey: "s",
		PublicURL: "https://packages.example.test",
	}
	manager := NewRepositoryManager(cfg, storage.NewMemoryStore())
	ctx := context.Background()

	err := manager.Upload(ctx, &UploadRequest{
		DebFiles: []string{"/nonexistent.deb"}, Distribution: "../evil", Component: "main",
	})
	require.Error(t, err)

	err = manager.Upload(ctx, &UploadRequest{
		DebFiles: []string{"/nonexistent.deb"}, Distribution: "stable", Component: "ma in",
	})
	require.Error(t, err)

	err = manager.Init(ctx, &InitRequest{
		Metadata: RepoMetadata{
			Distributions: []string{"stable"},
			Components:    []string{"main/../etc"},
			Architectures: []string{"amd64"},
		},
		KeyName: "k", KeyEmail: "e@example.com",
	})
	require.Error(t, err)
}

func testDeb(control []byte) []byte {
	controlTar := gzippedControlTar(control)
	emptyTar := gzippedControlTar([]byte("Package: ignored\n"))

	var arBuf bytes.Buffer
	arBuf.Write([]byte("!<arch>\n"))
	appendArMember(&arBuf, "debian-binary", []byte("2.0\n"))
	appendArMember(&arBuf, "control.tar.gz", controlTar)
	appendArMember(&arBuf, "data.tar.gz", emptyTar)

	return arBuf.Bytes()
}

func gzippedControlTar(control []byte) []byte {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	hdr := &tar.Header{
		Name: "./control",
		Mode: 0644,
		Size: int64(len(control)),
	}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write(control)
	_ = tw.Close()

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(tarBuf.Bytes())
	_ = gw.Close()

	return gzBuf.Bytes()
}

func appendArMember(buf *bytes.Buffer, name string, data []byte) {
	header := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n",
		name+"/",
		0, 0, 0, 0100644, len(data))
	buf.Write([]byte(header))
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}
