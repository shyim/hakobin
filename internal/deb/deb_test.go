package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsesGzippedControlTar(t *testing.T) {
	controlData := []byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\nDescription: Demo package\n")
	debBytes := testDeb(controlData)

	pkg, err := Parse("demo.deb", debBytes)
	require.NoError(t, err)

	pName, err := pkg.Package()
	require.NoError(t, err)
	assert.Equal(t, "demo", pName)

	pVer, err := pkg.Version()
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", pVer)

	pArch, err := pkg.Architecture()
	require.NoError(t, err)
	assert.Equal(t, "amd64", pArch)

	genFilename, err := pkg.GeneratedFilename()
	require.NoError(t, err)
	assert.Equal(t, "demo_1.0.0_amd64.deb", genFilename)
}

func TestRejectsPathTraversalInControlFields(t *testing.T) {
	cases := []struct {
		name    string
		control []byte
	}{
		{"package", []byte("Package: ../../etc\nVersion: 1.0.0\nArchitecture: amd64\n")},
		{"version", []byte("Package: demo\nVersion: 1.0/../../evil\nArchitecture: amd64\n")},
		{"architecture", []byte("Package: demo\nVersion: 1.0.0\nArchitecture: ../all\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkg, err := Parse("demo.deb", testDeb(tc.control))
			require.NoError(t, err)
			_, err = pkg.GeneratedFilename()
			require.Error(t, err)
		})
	}
}

func TestPackageEntryFoldsInjectedNewlines(t *testing.T) {
	// A crafted control value with a bare newline must not be able to forge a
	// new field or a package stanza boundary in the Packages index.
	control := []byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\n" +
		"Maintainer: evil\nFilename: pool/hacked.deb\n")
	pkg, err := Parse("demo.deb", testDeb(control))
	require.NoError(t, err)

	// Inject a malicious multi-line value directly into the control map to
	// simulate a value the parser folded; every continuation must stay folded.
	pkg.Control["Maintainer"] = "evil\nFilename: pool/hacked.deb"

	entry := pkg.PackageEntry("pool/m/demo/demo_1.0.0_amd64.deb")

	// Exactly one real (unfolded, start-of-line) Filename field: the pool path.
	unfoldedFilename := 0
	for _, line := range strings.Split(entry, "\n") {
		if strings.HasPrefix(line, "Filename: ") {
			unfoldedFilename++
		}
	}
	assert.Equal(t, 1, unfoldedFilename)
	assert.Contains(t, entry, "Filename: pool/m/demo/demo_1.0.0_amd64.deb")
	// The injected line must have been folded into a continuation (leading space).
	assert.Contains(t, entry, "\n Filename: pool/hacked.deb")
}

func TestPackageEntryFiltersReservedFieldsCaseInsensitively(t *testing.T) {
	pkg, err := Parse("demo.deb", testDeb([]byte("Package: demo\nVersion: 1.0.0\nArchitecture: amd64\n")))
	require.NoError(t, err)

	// A crafted control file using a differently-cased reserved field name must
	// not smuggle a second semantic field (e.g. a forged filename/checksum)
	// before the repository-computed values.
	pkg.Control["filename"] = "pool/hacked.deb"
	pkg.Control["Sha256"] = "deadbeef"

	entry := pkg.PackageEntry("pool/d/demo/demo_1.0.0_amd64.deb")

	filenameLines := 0
	sha256Lines := 0
	for _, line := range strings.Split(entry, "\n") {
		if strings.HasPrefix(strings.ToLower(line), "filename:") {
			filenameLines++
		}
		if strings.HasPrefix(strings.ToLower(line), "sha256:") {
			sha256Lines++
		}
	}
	assert.Equal(t, 1, filenameLines, "only the computed Filename must appear:\n%s", entry)
	assert.Equal(t, 1, sha256Lines, "only the computed SHA256 must appear:\n%s", entry)
	assert.Contains(t, entry, "Filename: pool/d/demo/demo_1.0.0_amd64.deb")
	assert.NotContains(t, entry, "pool/hacked.deb")
	assert.NotContains(t, entry, "deadbeef")
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
