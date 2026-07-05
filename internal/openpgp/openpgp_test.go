package openpgp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serializeEncryptedPrivateKey armors an entity whose private key is already
// encrypted, mirroring what a passphrase-protected key file on disk looks like.
// SerializePrivateWithoutSigning is used because re-signing user IDs would
// require the (locked) private key.
func serializeEncryptedPrivateKey(t *testing.T, entity *openpgp.Entity) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", err
	}
	if err := entity.SerializePrivateWithoutSigning(w, nil); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func TestPublicKeyBundleIncludesActiveAndTrustedKeys(t *testing.T) {
	active, err := GenerateKeyPair("Hakobin Active", "hakobin@example.com", "test", 0)
	require.NoError(t, err)

	trustedKP, err := GenerateKeyPair("Hakobin Old", "hakobin@example.com", "test", 0)
	require.NoError(t, err)

	trustedCert, err := trustedKP.PublicKeyCert()
	require.NoError(t, err)

	signingKeys := NewSigningKeys(active, []*PublicKeyCert{trustedCert})

	armored, err := signingKeys.PublicKeyArmored()
	require.NoError(t, err)

	binary, err := signingKeys.PublicKeyBinary()
	require.NoError(t, err)

	parsed, err := ParsePublicKeyCerts(armored)
	require.NoError(t, err)
	assert.Len(t, parsed, 2)

	armoredStr := string(armored)
	count := strings.Count(armoredStr, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	assert.Equal(t, 2, count)
	assert.NotEmpty(t, binary)
}

func TestPublicKeyBundleDeduplicatesActiveKey(t *testing.T) {
	active, err := GenerateKeyPair("Hakobin Active", "hakobin@example.com", "test", 0)
	require.NoError(t, err)

	duplicate, err := active.PublicKeyCert()
	require.NoError(t, err)

	signingKeys := NewSigningKeys(active, []*PublicKeyCert{duplicate})

	armored, err := signingKeys.PublicKeyArmored()
	require.NoError(t, err)

	armoredStr := string(armored)
	count := strings.Count(armoredStr, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	assert.Equal(t, 1, count)
}

func TestDetachedSigningAndVerification(t *testing.T) {
	active, err := GenerateKeyPair("Hakobin Active", "hakobin@example.com", "test", 0)
	require.NoError(t, err)

	data := []byte("hello world")
	sig, err := active.SignDetached(data)
	require.NoError(t, err)
	assert.Contains(t, sig, "-----BEGIN PGP SIGNATURE-----")
}

func TestParsePublicKeyCertsAcceptsPrivateKeyBlock(t *testing.T) {
	// A --signing-key rotation file contains a private key block; only its
	// public cert should be published, and it must not be silently dropped.
	kp, err := GenerateKeyPair("Hakobin Rotation", "hakobin@example.com", "test", 0)
	require.NoError(t, err)

	armored, err := kp.PrivateKeyArmored()
	require.NoError(t, err)
	require.Contains(t, armored, "-----BEGIN PGP PRIVATE KEY BLOCK-----")

	certs, err := ParsePublicKeyCerts([]byte(armored))
	require.NoError(t, err)
	require.Len(t, certs, 1)
	assert.Equal(t, kp.Fingerprint, certs[0].Fingerprint)
	// The published cert must be public-only, never a private key block.
	assert.Contains(t, certs[0].PublicKey, "-----BEGIN PGP PUBLIC KEY BLOCK-----")
	assert.NotContains(t, certs[0].PublicKey, "PRIVATE KEY")
}

func TestLoadKeyPairRejectsEncryptedKeyWithoutPassphrase(t *testing.T) {
	t.Setenv("GPG_PASSPHRASE", "")

	kp, err := GenerateKeyPair("Hakobin Locked", "hakobin@example.com", "test", 0)
	require.NoError(t, err)
	require.NoError(t, kp.Entity.PrivateKey.Encrypt([]byte("s3cret")))

	armored, err := serializeEncryptedPrivateKey(t, kp.Entity)
	require.NoError(t, err)

	_, err = LoadKeyPairFromArmored(armored)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GPG_PASSPHRASE")
}

func TestLoadKeyPairDecryptsEncryptedKeyWithPassphrase(t *testing.T) {
	t.Setenv("GPG_PASSPHRASE", "s3cret")

	kp, err := GenerateKeyPair("Hakobin Locked", "hakobin@example.com", "test", 0)
	require.NoError(t, err)
	require.NoError(t, kp.Entity.PrivateKey.Encrypt([]byte("s3cret")))

	armored, err := serializeEncryptedPrivateKey(t, kp.Entity)
	require.NoError(t, err)

	loaded, err := LoadKeyPairFromArmored(armored)
	require.NoError(t, err)

	// The unlocked key must be usable for signing.
	sig, err := loaded.SignDetached([]byte("payload"))
	require.NoError(t, err)
	assert.Contains(t, sig, "-----BEGIN PGP SIGNATURE-----")
}
