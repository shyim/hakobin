package rpm

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/rpmpack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shyim/hakobin/internal/config"
	"github.com/shyim/hakobin/internal/openpgp"
	"github.com/shyim/hakobin/internal/storage"
)

func TestSplitEVR(t *testing.T) {
	epoch, ver, rel := splitEVR("1:2.3.4-5.el9")
	assert.Equal(t, "1", epoch)
	assert.Equal(t, "2.3.4", ver)
	assert.Equal(t, "5.el9", rel)

	epoch, ver, rel = splitEVR("2.3.4")
	assert.Equal(t, "0", epoch)
	assert.Equal(t, "2.3.4", ver)
	assert.Equal(t, "", rel)

	// Epoch only, no release.
	epoch, ver, rel = splitEVR("2:1.0")
	assert.Equal(t, "2", epoch)
	assert.Equal(t, "1.0", ver)
	assert.Equal(t, "", rel)

	// Empty string.
	epoch, ver, rel = splitEVR("")
	assert.Equal(t, "0", epoch)
	assert.Equal(t, "", ver)
	assert.Equal(t, "", rel)
}

func TestFormatFlags(t *testing.T) {
	// createrepo-compatible symbolic flag names (not operator glyphs), so
	// YUM/DNF clients can resolve versioned dependencies.
	assert.Equal(t, "LT", formatFlags(0x02))
	assert.Equal(t, "GT", formatFlags(0x04))
	assert.Equal(t, "EQ", formatFlags(0x08))
	assert.Equal(t, "LE", formatFlags(0x0a))
	assert.Equal(t, "GE", formatFlags(0x0c))
	// Non-comparison flags yield no operator.
	assert.Equal(t, "", formatFlags(0x00))
	assert.Equal(t, "", formatFlags(0x10))
	// The comparison bits are masked out of higher bits.
	assert.Equal(t, "EQ", formatFlags(0x08|0x100))
}

func TestDiscoversRpmRepositoryFromMetadataAndPackageKeys(t *testing.T) {
	id := RpmRepositoryFromKey("rpm/stable/x86_64/repodata/repomd.xml")
	require.NotNil(t, id)
	assert.Equal(t, "stable", id.Repo)
	assert.Equal(t, "x86_64", id.Arch)

	id = RpmRepositoryFromKey("rpm/testing/aarch64/Packages/demo-1.0.0-1.aarch64.rpm")
	require.NotNil(t, id)
	assert.Equal(t, "testing", id.Repo)
	assert.Equal(t, "aarch64", id.Arch)

	id = RpmRepositoryFromKey("rpm/stable/x86_64/RPM-GPG-KEY-hakobin.asc")
	assert.Nil(t, id)

	id = RpmRepositoryFromKey("deb/dists/stable/Release")
	assert.Nil(t, id)
}

func TestMetadataUsesCompressedChecksumsInRepomd(t *testing.T) {
	pkg := RpmPackage{
		Name:        "demo",
		Epoch:       "0",
		Version:     "1.0.0",
		Release:     "1",
		Arch:        "x86_64",
		Summary:     "Demo",
		Description: "Demo package",
		License:     "MIT",
		Checksum:    "abc123",
		Location:    "Packages/demo-1.0.0-1.x86_64.rpm",
		Files:       []RpmFile{{Path: "/usr/bin/demo", Kind: "file"}},
	}

	meta, err := GenerateRepositoryMetadata([]RpmPackage{pkg})
	require.NoError(t, err)

	assert.Contains(t, meta.Repomd, "repodata/")
	assert.Contains(t, meta.Repomd, "primary")
	assert.Contains(t, meta.Repomd, "filelists")
	assert.Contains(t, meta.Repomd, "other")
	assert.True(t, strings.HasSuffix(meta.Primary.Filename, "-primary.xml.gz"))
}

func TestRpmMetadataSignatureIsAsciiArmored(t *testing.T) {
	active, err := openpgp.GenerateKeyPair("Hakobin Test", "hakobin@example.com", "RPM metadata signing", 0)
	require.NoError(t, err)

	trustedKP, err := openpgp.GenerateKeyPair("Hakobin Old", "hakobin-old@example.com", "RPM metadata signing", 0)
	require.NoError(t, err)

	trustedCert, err := trustedKP.PublicKeyCert()
	require.NoError(t, err)

	signingKeys := openpgp.NewSigningKeys(active, []*openpgp.PublicKeyCert{trustedCert})

	sig, pubKey, err := SignRpmMetadata([]byte("<repomd/>"), signingKeys)
	require.NoError(t, err)

	sigStr := string(sig)
	pubKeyStr := string(pubKey)

	assert.True(t, strings.HasPrefix(sigStr, "-----BEGIN PGP SIGNATURE-----"))
	count := strings.Count(pubKeyStr, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	assert.Equal(t, 2, count)
}

func TestParsesGeneratedRpmPackage(t *testing.T) {
	raw, err := testRpmPackage()
	require.NoError(t, err)

	parsed, err := FromBytes("Packages/demo-1.0.0-1.el9.x86_64.rpm", raw)
	require.NoError(t, err)
	assert.Equal(t, "demo", parsed.Name)
	assert.Equal(t, "1.0.0", parsed.Version)
	assert.Equal(t, "1.el9", parsed.Release)
	assert.Equal(t, "x86_64", parsed.Arch)
	assert.Len(t, parsed.Files, 1)
	assert.Equal(t, "/usr/bin/demo", parsed.Files[0].Path)

	// header-range must reflect the real header offsets, not 0/0.
	assert.Greater(t, parsed.HeaderStart, 0)
	assert.Greater(t, parsed.HeaderEnd, parsed.HeaderStart)
	// file time is deterministic (derived from build time), not wall-clock.
	assert.Equal(t, parsed.BuildTime, parsed.FileTime)

	primary := generatePrimaryXML([]RpmPackage{*parsed})
	assert.Contains(t, primary, fmt.Sprintf("<rpm:header-range start=\"%d\" end=\"%d\"/>", parsed.HeaderStart, parsed.HeaderEnd))
	assert.NotContains(t, primary, "header-range start=\"0\" end=\"0\"")
}

func TestRpmPackageSigningLeavesBytesUnchangedWithoutActiveKey(t *testing.T) {
	raw, err := testRpmPackage()
	require.NoError(t, err)

	signed, err := SignRpmPackage(raw, openpgp.NewSigningKeys(nil, nil))
	require.NoError(t, err)
	assert.Equal(t, raw, signed)
}

func TestRpmPackageSigningUsesActiveKey(t *testing.T) {
	active, err := openpgp.GenerateKeyPair("Hakobin RPM Package", "hakobin@example.com", "test", 0)
	require.NoError(t, err)

	raw, err := testRpmPackage()
	require.NoError(t, err)

	signed, err := SignRpmPackage(raw, openpgp.NewSigningKeys(active, nil))
	require.NoError(t, err)
	assert.NotEqual(t, raw, signed)

	parsed, err := FromBytes("Packages/demo-1.0.0-1.el9.x86_64.rpm", signed)
	require.NoError(t, err)
	assert.Equal(t, "demo", parsed.Name)
}

func TestWriteMetadataCleansStaleSignatureAndOrphanedRepodata(t *testing.T) {
	store := storage.NewMemoryStore()
	cfg := &config.Config{
		S3BucketName: "b", S3Region: "us-east-1", S3AccessKeyID: "k", S3SecretAccessKey: "s",
		PublicURL: "https://packages.example.test",
	}
	rm := NewRpmRepositoryManager(cfg, store)
	ctx := context.Background()

	raw, err := testRpmPackage()
	require.NoError(t, err)
	pkg, err := FromBytes("Packages/demo-1.0.0-1.el9.x86_64.rpm", raw)
	require.NoError(t, err)

	key, err := openpgp.GenerateKeyPair("Hakobin RPM", "hakobin@example.com", "test", 0)
	require.NoError(t, err)
	signed := openpgp.NewSigningKeys(key, nil)

	// First write: signed.
	require.NoError(t, rm.writeMetadata(ctx, "stable", "x86_64", []RpmPackage{*pkg}, signed))
	_, ok := store.Body("rpm/stable/x86_64/repodata/repomd.xml.asc")
	require.True(t, ok, "signed run must produce repomd.xml.asc")
	_, ok = store.Body("rpm/stable/x86_64/" + RpmPublicKeyName)
	require.True(t, ok, "signed run must produce the public key")

	firstPrimary := "rpm/stable/x86_64/repodata/" + primaryFilename(t, store)

	// Second write: unsigned. Stale .asc/pubkey must be gone, and the previous
	// checksum-named primary must be garbage-collected.
	require.NoError(t, rm.writeMetadata(ctx, "stable", "x86_64", []RpmPackage{*pkg}, openpgp.NewSigningKeys(nil, nil)))

	_, ok = store.Body("rpm/stable/x86_64/repodata/repomd.xml.asc")
	assert.False(t, ok, "stale repomd.xml.asc must be deleted on unsigned run")
	_, ok = store.Body("rpm/stable/x86_64/" + RpmPublicKeyName)
	assert.False(t, ok, "stale public key must be deleted on unsigned run")

	// The repodata dir must not accumulate old checksum-named primary files.
	primaries := 0
	for _, k := range store.Keys() {
		if strings.HasPrefix(k, "rpm/stable/x86_64/repodata/") && strings.Contains(k, "-primary.xml.gz") {
			primaries++
		}
	}
	assert.Equal(t, 1, primaries, "old primary.xml.gz should be garbage-collected")
	_ = firstPrimary
}

func primaryFilename(t *testing.T, store *storage.MemoryStore) string {
	t.Helper()
	for _, k := range store.Keys() {
		if strings.HasPrefix(k, "rpm/stable/x86_64/repodata/") && strings.Contains(k, "-primary.xml.gz") {
			return strings.TrimPrefix(k, "rpm/stable/x86_64/repodata/")
		}
	}
	return ""
}

func testRpmPackage() ([]byte, error) {
	rpm, err := rpmpack.NewRPM(rpmpack.RPMMetaData{
		Name:        "demo",
		Version:     "1.0.0",
		Release:     "1.el9",
		Arch:        "x86_64",
		Summary:     "Demo",
		Description: "Demo package",
		Licence:     "MIT",
	})
	if err != nil {
		return nil, err
	}

	rpm.AddFile(rpmpack.RPMFile{
		Name:  "/usr/bin/demo",
		Body:  []byte("hello"),
		Mode:  0100755,
		Owner: "root",
		Group: "root",
	})

	var buf bytes.Buffer
	if err := rpm.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
