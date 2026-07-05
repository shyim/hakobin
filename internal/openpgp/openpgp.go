package openpgp

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

type KeyPair struct {
	Entity      *openpgp.Entity
	PublicKey   string
	KeyID       string
	Fingerprint string
	Expiration  *time.Time
}

type PublicKeyCert struct {
	Entity      *openpgp.Entity
	PublicKey   string
	KeyID       string
	Fingerprint string
	Expiration  *time.Time
}

type SigningKeys struct {
	Active            *KeyPair
	TrustedPublicKeys []*PublicKeyCert
}

func NewKeyPair(entity *openpgp.Entity) (*KeyPair, error) {
	// Strip private key to get public key armored representation
	var pubBuf bytes.Buffer
	w, err := armor.Encode(&pubBuf, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := entity.Serialize(w); err != nil {
		return nil, err
	}
	w.Close()

	keyID := fmt.Sprintf("%016x", entity.PrimaryKey.KeyId)
	fp := hex.EncodeToString(entity.PrimaryKey.Fingerprint[:])

	var expiration *time.Time
	for _, ident := range entity.Identities {
		if ident.SelfSignature != nil && ident.SelfSignature.KeyLifetimeSecs != nil {
			lifetime := *ident.SelfSignature.KeyLifetimeSecs
			if lifetime > 0 {
				exp := entity.PrimaryKey.CreationTime.Add(time.Duration(lifetime) * time.Second)
				expiration = &exp
			}
		}
		break
	}

	return &KeyPair{
		Entity:      entity,
		PublicKey:   pubBuf.String(),
		KeyID:       keyID,
		Fingerprint: fp,
		Expiration:  expiration,
	}, nil
}

func NewPublicKeyCert(entity *openpgp.Entity) (*PublicKeyCert, error) {
	var pubBuf bytes.Buffer
	w, err := armor.Encode(&pubBuf, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := entity.Serialize(w); err != nil {
		return nil, err
	}
	w.Close()

	keyID := fmt.Sprintf("%016x", entity.PrimaryKey.KeyId)
	fp := hex.EncodeToString(entity.PrimaryKey.Fingerprint[:])

	var expiration *time.Time
	for _, ident := range entity.Identities {
		if ident.SelfSignature != nil && ident.SelfSignature.KeyLifetimeSecs != nil {
			lifetime := *ident.SelfSignature.KeyLifetimeSecs
			if lifetime > 0 {
				exp := entity.PrimaryKey.CreationTime.Add(time.Duration(lifetime) * time.Second)
				expiration = &exp
			}
		}
		break
	}

	return &PublicKeyCert{
		Entity:      entity,
		PublicKey:   pubBuf.String(),
		KeyID:       keyID,
		Fingerprint: fp,
		Expiration:  expiration,
	}, nil
}

func GenerateKeyPair(name, email, comment string, expirationYears uint32) (*KeyPair, error) {
	var lifetimeSecs uint32
	if expirationYears > 0 {
		lifetimeSecs = expirationYears * 365 * 24 * 60 * 60
	}

	config := &packet.Config{
		Algorithm:       packet.PubKeyAlgoRSA,
		RSABits:         4096,
		KeyLifetimeSecs: lifetimeSecs,
	}

	entity, err := openpgp.NewEntity(name, comment, email, config)
	if err != nil {
		return nil, fmt.Errorf("failed to generate entity: %w", err)
	}

	return NewKeyPair(entity)
}

func LoadKeyPairFromPath(path string) (*KeyPair, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadKeyPairFromArmored(string(data))
}

func LoadKeyPairFromArmored(data string) (*KeyPair, error) {
	if !strings.Contains(data, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		return nil, fmt.Errorf("invalid private key format: expected ASCII armored PGP private key")
	}
	entityList, err := openpgp.ReadArmoredKeyRing(strings.NewReader(data))
	if err != nil {
		return nil, err
	}
	if len(entityList) == 0 {
		return nil, fmt.Errorf("no keys found in private key block")
	}

	entity := entityList[0]
	if err := decryptPrivateKey(entity); err != nil {
		return nil, err
	}

	return NewKeyPair(entity)
}

// decryptPrivateKey decrypts an encrypted (passphrase-protected) private key
// in place using the GPG_PASSPHRASE environment variable. An encrypted key that
// is left encrypted cannot sign, so this fails loudly rather than producing
// unsigned or corrupt artifacts later.
func decryptPrivateKey(entity *openpgp.Entity) error {
	if entity.PrivateKey == nil {
		return fmt.Errorf("key does not contain a private key")
	}

	if !entity.PrivateKey.Encrypted {
		return nil
	}

	passphrase := os.Getenv("GPG_PASSPHRASE")
	if passphrase == "" {
		return fmt.Errorf("signing key is passphrase-protected; set GPG_PASSPHRASE to unlock it")
	}

	pw := []byte(passphrase)
	if err := entity.PrivateKey.Decrypt(pw); err != nil {
		return fmt.Errorf("failed to decrypt signing key with GPG_PASSPHRASE: %w", err)
	}
	for _, sub := range entity.Subkeys {
		if sub.PrivateKey != nil && sub.PrivateKey.Encrypted {
			if err := sub.PrivateKey.Decrypt(pw); err != nil {
				return fmt.Errorf("failed to decrypt signing subkey with GPG_PASSPHRASE: %w", err)
			}
		}
	}

	return nil
}

func (k *KeyPair) SavePrivateKey(path string) error {
	armored, err := k.PrivateKeyArmored()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	err = os.WriteFile(path, []byte(armored), 0600)
	if err != nil {
		return err
	}

	return nil
}

func (k *KeyPair) PrivateKeyArmored() (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", err
	}
	err = k.Entity.SerializePrivate(w, nil)
	if err != nil {
		return "", err
	}
	w.Close()
	return buf.String(), nil
}

func (k *KeyPair) PublicKeyBinary() ([]byte, error) {
	var buf bytes.Buffer
	err := k.Entity.Serialize(&buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (k *KeyPair) PublicKeyCert() (*PublicKeyCert, error) {
	return NewPublicKeyCert(k.Entity)
}

func (k *KeyPair) SignDetached(data []byte) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.SignatureType, nil)
	if err != nil {
		return "", err
	}
	err = openpgp.DetachSign(w, k.Entity, bytes.NewReader(data), nil)
	if err != nil {
		return "", err
	}
	w.Close()
	return buf.String(), nil
}

// ClearSign produces an inline clearsigned message (used for apt's InRelease).
func (k *KeyPair) ClearSign(data []byte) (string, error) {
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, k.Entity.PrivateKey, nil)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func LoadPublicKeyCerts(path string) ([]*PublicKeyCert, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePublicKeyCerts(data)
}

func ParsePublicKeyCerts(data []byte) ([]*PublicKeyCert, error) {
	var entityList openpgp.EntityList
	isArmored := bytes.Contains(data, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) ||
		bytes.Contains(data, []byte("-----BEGIN PGP PRIVATE KEY BLOCK-----"))
	if isArmored {
		// Split the input into individual armored blocks and decode each with
		// its own reader. Decoding sequentially from a single shared reader is
		// unreliable when multiple concatenated blocks are present.
		for _, blockBytes := range splitArmoredBlocks(data) {
			block, err := armor.Decode(bytes.NewReader(blockBytes))
			if err == io.EOF {
				continue
			}
			if err != nil {
				return nil, err
			}
			// Accept both public and private key blocks. A --signing-key file
			// used as a trusted rotation key contains a private block; we only
			// publish its public cert, so a private block must not be dropped.
			if block.Type != openpgp.PublicKeyType && block.Type != openpgp.PrivateKeyType {
				continue
			}
			subList, err := openpgp.ReadKeyRing(block.Body)
			if err != nil {
				return nil, err
			}
			entityList = append(entityList, subList...)
		}
	} else {
		var err error
		entityList, err = openpgp.ReadKeyRing(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
	}

	var certs []*PublicKeyCert
	for _, entity := range entityList {
		cert, err := NewPublicKeyCert(entity)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

// splitArmoredBlocks extracts each individual ASCII-armored block (from its
// "-----BEGIN" line through the matching "-----END" line) so each can be
// decoded independently.
func splitArmoredBlocks(data []byte) [][]byte {
	lines := strings.Split(string(data), "\n")
	var blocks [][]byte
	var current []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-----BEGIN ") {
			inBlock = true
			current = []string{line}
			continue
		}
		if !inBlock {
			continue
		}
		current = append(current, line)
		if strings.HasPrefix(trimmed, "-----END ") {
			blocks = append(blocks, []byte(strings.Join(current, "\n")+"\n"))
			inBlock = false
			current = nil
		}
	}
	return blocks
}

func (p *PublicKeyCert) ToBinary() ([]byte, error) {
	var buf bytes.Buffer
	err := p.Entity.Serialize(&buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func NewSigningKeys(active *KeyPair, trusted []*PublicKeyCert) *SigningKeys {
	seen := make(map[string]bool)
	if active != nil {
		seen[active.Fingerprint] = true
	}

	var deduped []*PublicKeyCert
	for _, cert := range trusted {
		if !seen[cert.Fingerprint] {
			seen[cert.Fingerprint] = true
			deduped = append(deduped, cert)
		}
	}

	return &SigningKeys{
		Active:            active,
		TrustedPublicKeys: deduped,
	}
}

func (s *SigningKeys) PublicCerts() ([]*PublicKeyCert, error) {
	var certs []*PublicKeyCert
	if s.Active != nil {
		cert, err := s.Active.PublicKeyCert()
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	certs = append(certs, s.TrustedPublicKeys...)
	return certs, nil
}

func (s *SigningKeys) PublicKeyArmored() ([]byte, error) {
	certs, err := s.PublicCerts()
	if err != nil {
		return nil, err
	}

	var out strings.Builder
	for i, cert := range certs {
		block := strings.TrimRight(cert.PublicKey, "\n")
		if i > 0 {
			// Separate consecutive armored blocks with a blank line so parsers
			// (gpg --import, apt-key) reliably see distinct keys.
			out.WriteString("\n\n")
		}
		out.WriteString(block)
	}
	if out.Len() > 0 {
		out.WriteString("\n")
	}

	bundle := []byte(out.String())

	// Guard against a malformed concatenation: the emitted bundle must parse
	// back to exactly the certs we put in.
	if len(certs) > 0 {
		parsed, err := ParsePublicKeyCerts(bundle)
		if err != nil {
			return nil, fmt.Errorf("public key bundle failed to round-trip: %w", err)
		}
		if len(parsed) != len(certs) {
			return nil, fmt.Errorf("public key bundle round-trip mismatch: wrote %d keys, parsed %d", len(certs), len(parsed))
		}
	}

	return bundle, nil
}

func (s *SigningKeys) PublicKeyBinary() ([]byte, error) {
	certs, err := s.PublicCerts()
	if err != nil {
		return nil, err
	}

	var out []byte
	for _, cert := range certs {
		bin, err := cert.ToBinary()
		if err != nil {
			return nil, err
		}
		out = append(out, bin...)
	}
	return out, nil
}
