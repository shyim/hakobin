package openpgp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func LoadSigningKeys(signingPaths, trustedPaths []string) *SigningKeys {
	envKey := strings.TrimSpace(os.Getenv("GPG_PRIVATE_KEY"))
	envKeyPresent := envKey != ""
	var activeSource string
	var active *KeyPair

	if envKeyPresent {
		kp, err := LoadKeyPairFromArmored(envKey)
		if err != nil {
			fmt.Printf("Warning: Failed to load signing key: %v\n", err)
		} else {
			activeSource = "environment variable"
			active = kp
		}
	} else {
		var activePath string
		if len(signingPaths) > 0 {
			activePath = signingPaths[0]
		} else {
			cwd, err := os.Getwd()
			if err == nil {
				activePath = filepath.Join(cwd, "signing-key.gpg")
			}
		}

		if activePath != "" {
			if _, err := os.Stat(activePath); err == nil {
				kp, err := LoadKeyPairFromPath(activePath)
				if err != nil {
					fmt.Printf("Warning: Failed to load signing key: %v\n", err)
				} else {
					activeSource = fmt.Sprintf("file %s", activePath)
					active = kp
				}
			}
		}
	}

	var trusted []*PublicKeyCert
	var extraSigningKeys []string
	if envKeyPresent {
		extraSigningKeys = signingPaths
	} else {
		if len(signingPaths) > 1 {
			extraSigningKeys = signingPaths[1:]
		}
	}

	allTrustedPaths := append(extraSigningKeys, trustedPaths...)
	for _, path := range allTrustedPaths {
		certs, err := LoadPublicKeyCerts(path)
		if err != nil {
			fmt.Printf("Warning: Failed to load trusted signing key %s: %v\n", path, err)
		} else {
			trusted = append(trusted, certs...)
		}
	}

	signingKeys := NewSigningKeys(active, trusted)
	PrintSigningKeyStatus(signingKeys, activeSource)
	return signingKeys
}

func PrintSigningKeyStatus(signingKeys *SigningKeys, activeSource string) {
	if signingKeys.Active != nil {
		source := activeSource
		if source == "" {
			source = "file"
		}
		fmt.Printf("Using GPG signing key: %s (loaded from %s)%s\n",
			signingKeys.Active.KeyID,
			source,
			ExpirationStatus(signingKeys.Active.Expiration),
		)
	}

	if len(signingKeys.TrustedPublicKeys) > 0 {
		if signingKeys.Active == nil {
			fmt.Println("Trusted public signing keys loaded, but no active signing key is available:")
		} else {
			fmt.Println("Trusted public signing keys:")
		}
		for _, key := range signingKeys.TrustedPublicKeys {
			fmt.Printf("  - %s%s\n", key.KeyID, ExpirationStatus(key.Expiration))
		}
	}
}

func ExpirationStatus(expiration *time.Time) string {
	if expiration == nil {
		return ""
	}
	if expiration.Before(time.Now()) {
		return " [EXPIRED]"
	}
	return fmt.Sprintf(" [expires %s]", expiration.Format("2006-01-02"))
}
