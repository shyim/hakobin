package openpgp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeKeyFile(t *testing.T, dir, name, user string) string {
	t.Helper()
	kp, err := GenerateKeyPair(user, user+"@example.com", "test", 0)
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, kp.SavePrivateKey(path))
	return path
}

func TestLoadSigningKeysFromEnv(t *testing.T) {
	kp, err := GenerateKeyPair("Env Key", "env@example.com", "test", 0)
	require.NoError(t, err)
	armored, err := kp.PrivateKeyArmored()
	require.NoError(t, err)

	t.Setenv("GPG_PRIVATE_KEY", armored)

	keys, err := LoadSigningKeys(nil, nil)
	require.NoError(t, err)
	require.NotNil(t, keys.Active)
	assert.Equal(t, kp.KeyID, keys.Active.KeyID)
}

func TestLoadSigningKeysFromFile(t *testing.T) {
	t.Setenv("GPG_PRIVATE_KEY", "")
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "signing-key.gpg", "File Key")

	keys, err := LoadSigningKeys([]string{path}, nil)
	require.NoError(t, err)
	require.NotNil(t, keys.Active)
}

func TestLoadSigningKeysMissingExplicitKeyIsError(t *testing.T) {
	t.Setenv("GPG_PRIVATE_KEY", "")
	keys, err := LoadSigningKeys([]string{"/does/not/exist.gpg"}, nil)
	require.Error(t, err)
	assert.Nil(t, keys)
}

func TestLoadSigningKeysBadEnvIsError(t *testing.T) {
	t.Setenv("GPG_PRIVATE_KEY", "-----BEGIN PGP PRIVATE KEY BLOCK-----\ngarbage\n-----END PGP PRIVATE KEY BLOCK-----")
	_, err := LoadSigningKeys(nil, nil)
	require.Error(t, err)
}

func TestLoadSigningKeysTrustedKeys(t *testing.T) {
	t.Setenv("GPG_PRIVATE_KEY", "")
	dir := t.TempDir()
	active := writeKeyFile(t, dir, "signing-key.gpg", "Active")
	old := writeKeyFile(t, dir, "old.gpg", "Old")

	// Second --signing-key and --trusted-key both become trusted certs.
	keys, err := LoadSigningKeys([]string{active, old}, nil)
	require.NoError(t, err)
	require.NotNil(t, keys.Active)
	assert.Len(t, keys.TrustedPublicKeys, 1)
}

func TestLoadKeyPairFromPath(t *testing.T) {
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "k.gpg", "Path Key")

	kp, err := LoadKeyPairFromPath(path)
	require.NoError(t, err)
	assert.NotEmpty(t, kp.KeyID)

	_, err = LoadKeyPairFromPath(filepath.Join(dir, "missing.gpg"))
	require.Error(t, err)
}

func TestLoadPublicKeyCertsFromFile(t *testing.T) {
	kp, err := GenerateKeyPair("Pub", "pub@example.com", "test", 0)
	require.NoError(t, err)
	cert, err := kp.PublicKeyCert()
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "pub.asc")
	require.NoError(t, os.WriteFile(path, []byte(cert.PublicKey), 0644))

	certs, err := LoadPublicKeyCerts(path)
	require.NoError(t, err)
	require.Len(t, certs, 1)
	assert.Equal(t, kp.Fingerprint, certs[0].Fingerprint)
}

func TestClearSignProducesInlineSignature(t *testing.T) {
	kp, err := GenerateKeyPair("Clear", "clear@example.com", "test", 0)
	require.NoError(t, err)

	out, err := kp.ClearSign([]byte("Release contents\n"))
	require.NoError(t, err)
	assert.Contains(t, out, "-----BEGIN PGP SIGNED MESSAGE-----")
	assert.Contains(t, out, "Release contents")
	assert.Contains(t, out, "-----BEGIN PGP SIGNATURE-----")
}

func TestExpirationStatus(t *testing.T) {
	assert.Equal(t, "", ExpirationStatus(nil))

	past := time.Now().Add(-24 * time.Hour)
	assert.Contains(t, ExpirationStatus(&past), "EXPIRED")

	future := time.Now().Add(24 * time.Hour)
	assert.Contains(t, ExpirationStatus(&future), "expires")
}
