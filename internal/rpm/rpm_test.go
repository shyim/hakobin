package rpm

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/rpmpack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"hakobin/internal/openpgp"
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
