package openpgp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
