package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
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
