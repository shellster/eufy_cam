package crypto

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"fmt"
)

const (
	rsaBits = 2048
)

func GeneratePrivateKey() (*rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}
	return privateKey, nil
}

func DecryptPKCS1v15(encryptedKey []byte, privateKey *rsa.PrivateKey) ([]byte, error) {
	if len(encryptedKey) != 128 {
		return nil, fmt.Errorf("encrypted key must be 128 bytes, got %d", len(encryptedKey))
	}

	plaintext, err := rsa.DecryptPKCS1v15(rand.Reader, privateKey, encryptedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt RSA: %w", err)
	}

	return plaintext, nil
}

func DecryptOAEP(encryptedKey []byte, privateKey *rsa.PrivateKey) ([]byte, error) {
	hash := sha256.New()
	plaintext, err := rsa.DecryptOAEP(hash, rand.Reader, privateKey, encryptedKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt RSA OAEP: %w", err)
	}
	return plaintext, nil
}
