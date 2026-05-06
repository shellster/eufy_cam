package crypto

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const (
	ServerPublicKey = "04c5c00c4f8d1197cc7c3167c52bf7acb054d722f0ef08dcd7e0883236e0d72a3868d9750cb47fa4619248f3d83f0f662671dadc6e2d31c2f41db0161651c7c076"
)

type KeyPair struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

func GenerateKeyPair() (*KeyPair, error) {
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ECDH key pair: %w", err)
	}

	publicKeyBytes := privateKey.PublicKey().Bytes()
	publicKeyHex := hex.EncodeToString(publicKeyBytes)

	privateKeyBytes := privateKey.Bytes()
	privateKeyHex := hex.EncodeToString(privateKeyBytes)

	return &KeyPair{
		PrivateKey: privateKeyHex,
		PublicKey:  publicKeyHex,
	}, nil
}

func ComputeSecret(privateKeyHex, publicKeyHex string) ([]byte, error) {
	privateKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode private key: %w", err)
	}

	privateKey, err := ecdh.P256().NewPrivateKey(privateKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create ECDH key: %w", err)
	}

	publicKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode public key: %w", err)
	}

	publicKey, err := ecdh.P256().NewPublicKey(publicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal public key: %w", err)
	}

	secret, err := privateKey.ECDH(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to compute shared secret: %w", err)
	}

	return secret, nil
}

func ParseServerPublicKey() (*ecdh.PublicKey, error) {
	publicKeyBytes, err := hex.DecodeString(ServerPublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode server public key: %w", err)
	}

	return ecdh.P256().NewPublicKey(publicKeyBytes)
}
