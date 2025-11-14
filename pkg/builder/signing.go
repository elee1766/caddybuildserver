package builder

import (
	"bytes"
	"fmt"
	"os"

	"golang.org/x/crypto/openpgp"
)

// Signer handles PGP signing of built binaries
type Signer struct {
	entity *openpgp.Entity
}

// NewSigner creates a new Signer from an ASCII-armored keyring file
// Returns an error if the key file cannot be loaded or decrypted
func NewSigner(keyFile, password string) (*Signer, error) {
	// Open the signing key file
	file, err := os.Open(keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open signing key file: %w", err)
	}
	defer file.Close()

	// Read ASCII-armored key ring
	entities, err := openpgp.ReadArmoredKeyRing(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read armored keyring: %w", err)
	}

	if len(entities) == 0 {
		return nil, fmt.Errorf("no signing entities found in key file")
	}

	entity := entities[0]

	// Decrypt private key if it's encrypted
	if entity.PrivateKey != nil && entity.PrivateKey.Encrypted {
		if password == "" {
			return nil, fmt.Errorf("signing key is encrypted but no password provided")
		}
		err = entity.PrivateKey.Decrypt([]byte(password))
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt private key: %w", err)
		}
	}

	return &Signer{
		entity: entity,
	}, nil
}

// SignFile creates a detached ASCII-armored signature for the given file
func (s *Signer) SignFile(filePath string) ([]byte, error) {
	if s == nil || s.entity == nil {
		return nil, fmt.Errorf("signer not initialized")
	}

	// Open the file to sign
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file for signing: %w", err)
	}
	defer file.Close()

	// Create buffer for the signature
	var sigBuf bytes.Buffer

	// Create detached ASCII-armored signature
	err = openpgp.ArmoredDetachSign(&sigBuf, s.entity, file, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to sign file: %w", err)
	}

	return sigBuf.Bytes(), nil
}
