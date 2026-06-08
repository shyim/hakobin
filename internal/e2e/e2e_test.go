package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/rpmpack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testcontainers/testcontainers-go/network"

	hconfig "hakobin/internal/config"
	"hakobin/internal/openpgp"
	"hakobin/internal/repository"
	"hakobin/internal/rpm"
	"hakobin/internal/storage"
)

func TestE2EWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	ctx := context.Background()

	// Ensure private key file is removed at the very end of E2E test
	defer os.Remove("signing-key.gpg")

	// Determine client native architectures
	nativeDebArch := "amd64"
	nativeRpmArch := "x86_64"
	if runtime.GOARCH == "arm64" {
		nativeDebArch = "arm64"
		nativeRpmArch = "aarch64"
	}

	// 1. Create Docker network
	nw, err := network.New(ctx)
	require.NoError(t, err)
	defer func() {
		_ = nw.Remove(ctx)
	}()

	// 2. Start MinIO test container on the network
	minioContainer, err := minio.Run(ctx, "minio/minio:latest",
		network.WithNetwork([]string{"minio-s3"}, nw),
	)
	require.NoError(t, err)
	defer func() {
		_ = minioContainer.Terminate(ctx)
	}()

	// Host connection endpoint (using localhost mapping)
	hostEndpoint, err := minioContainer.ConnectionString(ctx)
	require.NoError(t, err)
	if !strings.HasPrefix(hostEndpoint, "http://") && !strings.HasPrefix(hostEndpoint, "https://") {
		hostEndpoint = "http://" + hostEndpoint
	}

	bucketName := "hakobin-test-bucket"

	// Container-internal public URL base for the repository (must include the bucket name for path-style access)
	containerPublicURL := "http://minio-s3:9000/" + bucketName

	accessKey := minioContainer.Username
	secretKey := minioContainer.Password

	// 3. Create the test bucket and configure it as public-read in MinIO
	err = createMinioBucket(ctx, hostEndpoint, accessKey, secretKey, bucketName)
	require.NoError(t, err)

	// 4. Construct host config (for uploading packages)
	hostCfg := &hconfig.Config{
		S3Endpoint:        hostEndpoint,
		S3AccessKeyID:     accessKey,
		S3SecretAccessKey: secretKey,
		S3BucketName:      bucketName,
		S3Region:          "us-east-1",
		S3UsePathStyle:    true,
		PublicURL:         containerPublicURL, // Generated Setup script and Release indexes will point here
	}

	store, err := storage.NewS3Store(ctx, hostCfg)
	require.NoError(t, err)

	// 5. Spin up client test containers on the same network
	ubuntuClient, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    "ubuntu:24.04",
			Cmd:      []string{"tail", "-f", "/dev/null"},
			Networks: []string{nw.Name},
		},
		Started: true,
	})
	require.NoError(t, err)
	defer func() {
		_ = ubuntuClient.Terminate(ctx)
	}()

	almaClient, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    "almalinux:9",
			Cmd:      []string{"tail", "-f", "/dev/null"},
			Networks: []string{nw.Name},
		},
		Started: true,
	})
	require.NoError(t, err)
	defer func() {
		_ = almaClient.Terminate(ctx)
	}()

	// --- DEB / APT Repository E2E Workflow ---
	t.Run("DEB APT Workflow", func(t *testing.T) {
		manager := repository.NewRepositoryManager(hostCfg, store)

		// 1. Init
		initReq := &repository.InitRequest{
			Metadata: repository.RepoMetadata{
				Origin:        "Hakobin E2E",
				Label:         "Hakobin E2E Repo",
				Description:   "E2E Testing",
				Distributions: []string{"stable"},
				Components:    []string{"main"},
				Architectures: []string{nativeDebArch, "all"},
			},
			KeyName:            "E2E Key",
			KeyEmail:           "e2e@example.com",
			KeyExpirationYears: 1,
		}

		err := manager.Init(ctx, initReq)
		require.NoError(t, err)

		signingKeys, err := openpgp.LoadSigningKeys([]string{"signing-key.gpg"}, nil)
		require.NoError(t, err)
		require.NotNil(t, signingKeys.Active)

		// 2. Generate package with file content & Upload
		controlData := []byte(fmt.Sprintf("Package: e2e-demo\nVersion: 2.1.0\nArchitecture: %s\nDescription: E2E demo package\n", nativeDebArch))
		controlTar := gzippedControlTar(controlData)
		dataTar := gzippedDataTar("./usr/bin/e2e-demo", []byte("#!/bin/sh\necho \"hello e2e\"\n"))
		debBytes := testDeb(controlTar, dataTar)

		tempDeb, err := os.CreateTemp("", "e2e-*.deb")
		require.NoError(t, err)
		defer os.Remove(tempDeb.Name())
		_, err = tempDeb.Write(debBytes)
		require.NoError(t, err)
		tempDeb.Close()

		uploadReq := &repository.UploadRequest{
			DebFiles:     []string{tempDeb.Name()},
			Distribution: "stable",
			Component:    "main",
			Force:        false,
			SigningKeys:  signingKeys,
		}

		err = manager.Upload(ctx, uploadReq)
		require.NoError(t, err)

		// 3. Native install validation inside Ubuntu container
		runExec(t, ctx, ubuntuClient, []string{"apt-get", "update"})
		runExec(t, ctx, ubuntuClient, []string{"apt-get", "install", "-y", "curl", "gnupg"})

		// Download and run the generated setup script (using bucket prefix)
		setupScriptURL := fmt.Sprintf("%s/deb/setup.sh", containerPublicURL)
		runExec(t, ctx, ubuntuClient, []string{"curl", "-fsSL", setupScriptURL, "-o", "/tmp/setup.sh"})
		runExec(t, ctx, ubuntuClient, []string{"bash", "/tmp/setup.sh"})

		// Install our uploaded package native
		runExec(t, ctx, ubuntuClient, []string{"apt-get", "install", "-y", "e2e-demo"})

		// Verify executed script output
		output, exitCode, err := execCommand(ctx, ubuntuClient, []string{"e2e-demo"})
		require.NoError(t, err)
		assert.Equal(t, 0, exitCode)
		assert.Contains(t, output, "hello e2e")

		// Remove Package from repository
		removeReq := &repository.RemoveRequest{
			Package:      "e2e-demo",
			Version:      "2.1.0",
			Architecture: nativeDebArch,
			Distribution: "stable",
			Component:    "main",
			Force:        true,
			SigningKeys:  signingKeys,
		}
		err = manager.Remove(ctx, removeReq)
		require.NoError(t, err)
	})

	// --- RPM / YUM Repository E2E Workflow ---
	t.Run("RPM YUM Workflow", func(t *testing.T) {
		manager := rpm.NewRpmRepositoryManager(hostCfg, store)

		// 1. Init
		initReq := &rpm.RpmInitRequest{
			Repo: "stable",
			Arch: nativeRpmArch,
		}

		err := manager.Init(ctx, initReq)
		require.NoError(t, err)

		// Create a dedicated signing key for the RPM workflow so this subtest is
		// self-contained and does not depend on the DEB subtest having generated
		// ./signing-key.gpg (running the RPM subtest in isolation must work).
		rpmKeyDir, err := os.MkdirTemp("", "rpm-e2e-key")
		require.NoError(t, err)
		defer os.RemoveAll(rpmKeyDir)
		rpmKeyPath := filepath.Join(rpmKeyDir, "signing-key.gpg")
		rpmKeyPair, err := openpgp.GenerateKeyPair("E2E RPM Key", "rpm-e2e@example.com", "RPM Signing Key", 1)
		require.NoError(t, err)
		require.NoError(t, rpmKeyPair.SavePrivateKey(rpmKeyPath))

		signingKeys, err := openpgp.LoadSigningKeys([]string{rpmKeyPath}, nil)
		require.NoError(t, err)
		require.NotNil(t, signingKeys.Active)

		// 2. Generate package & Upload
		rpmBytes, err := testRpmPackage(nativeRpmArch)
		require.NoError(t, err)

		tempDir, err := os.MkdirTemp("", "rpm-e2e")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		tempRpmPath := filepath.Join(tempDir, fmt.Sprintf("demo-1.0.0-1.el9.%s.rpm", nativeRpmArch))
		err = os.WriteFile(tempRpmPath, rpmBytes, 0644)
		require.NoError(t, err)

		uploadReq := &rpm.RpmUploadRequest{
			RpmFiles:    []string{tempRpmPath},
			Repo:        "stable",
			Arch:        nativeRpmArch,
			Force:       false,
			SigningKeys: signingKeys,
		}

		err = manager.Upload(ctx, uploadReq)
		require.NoError(t, err)

		// 3. Native install validation inside AlmaLinux container
		// Write the YUM repo config file (using bucket prefix)
		repoConfig := fmt.Sprintf(`[hakobin]
name=Hakobin E2E
baseurl=%s/rpm/stable/%s
gpgcheck=1
gpgkey=%s/rpm/stable/%s/RPM-GPG-KEY-hakobin.asc
enabled=1
`, containerPublicURL, nativeRpmArch, containerPublicURL, nativeRpmArch)

		writeRepoCmd := fmt.Sprintf("cat << 'EOF' > /etc/yum.repos.d/hakobin.repo\n%sEOF\n", repoConfig)
		runExec(t, ctx, almaClient, []string{"sh", "-c", writeRepoCmd})

		// Install our package
		runExec(t, ctx, almaClient, []string{"dnf", "install", "-y", "demo"})

		// Execute installed file
		output, exitCode, err := execCommand(ctx, almaClient, []string{"demo"})
		require.NoError(t, err)
		assert.Equal(t, 0, exitCode)
		assert.Contains(t, output, "hello rpm e2e")

		// Clean up repository
		removeReq := &rpm.RpmRemoveRequest{
			Package:     "demo",
			Epoch:       "0",
			Version:     "1.0.0",
			Release:     "1.el9",
			Arch:        nativeRpmArch,
			Repo:        "stable",
			RepoArch:    nativeRpmArch,
			Force:       true,
			SigningKeys: signingKeys,
		}

		err = manager.Remove(ctx, removeReq)
		require.NoError(t, err)
	})
}

func execCommand(ctx context.Context, container testcontainers.Container, cmd []string) (string, int, error) {
	exitCode, reader, err := container.Exec(ctx, cmd)
	if err != nil {
		return "", 0, err
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, reader)
	return buf.String(), exitCode, nil
}

func runExec(t *testing.T, ctx context.Context, container testcontainers.Container, cmd []string) {
	output, exitCode, err := execCommand(ctx, container, cmd)
	require.NoError(t, err)
	if exitCode != 0 {
		t.Fatalf("command failed with exit code %d: %v. Output:\n%s", exitCode, cmd, output)
	}
}

func createMinioBucket(ctx context.Context, endpoint, accessKey, secretKey, bucket string) error {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return err
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return err
	}

	// Apply public-read bucket policy so that clients can fetch objects anonymously
	policy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "PublicRead",
				"Effect": "Allow",
				"Principal": "*",
				"Action": ["s3:GetObject"],
				"Resource": ["arn:aws:s3:::%s/*"]
			}
		]
	}`, bucket)

	_, err = client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(policy),
	})
	return err
}

func testDeb(controlTar, dataTar []byte) []byte {
	var arBuf bytes.Buffer
	arBuf.Write([]byte("!<arch>\n"))
	appendArMember(&arBuf, "debian-binary", []byte("2.0\n"))
	appendArMember(&arBuf, "control.tar.gz", controlTar)
	appendArMember(&arBuf, "data.tar.gz", dataTar)

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

func gzippedDataTar(path string, content []byte) []byte {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	hdr := &tar.Header{
		Name: path,
		Mode: 0755,
		Size: int64(len(content)),
	}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write(content)
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

func testRpmPackage(arch string) ([]byte, error) {
	rpm, err := rpmpack.NewRPM(rpmpack.RPMMetaData{
		Name:        "demo",
		Version:     "1.0.0",
		Release:     "1.el9",
		Arch:        arch,
		Summary:     "Demo",
		Description: "Demo package",
		Licence:     "MIT",
	})
	if err != nil {
		return nil, err
	}

	rpm.AddFile(rpmpack.RPMFile{
		Name:  "/usr/bin/demo",
		Body:  []byte("#!/bin/sh\necho \"hello rpm e2e\"\n"),
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
