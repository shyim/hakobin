package repository

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
