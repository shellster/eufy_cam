package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"
)

func EncryptCBC(plaintext []byte, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}

	padded := PKCS7Pad(plaintext, aes.BlockSize)
	iv := key[:aes.BlockSize]
	stream := cipher.NewCBCEncrypter(block, iv)

	ciphertext := make([]byte, len(padded))
	stream.CryptBlocks(ciphertext, padded)

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func DecryptCBCRaw(ciphertext []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	if len(key) < aes.BlockSize {
		return nil, fmt.Errorf("key too short for IV: %d bytes", len(key))
	}

	iv := key[:aes.BlockSize]
	stream := cipher.NewCBCDecrypter(block, iv)

	plaintext := make([]byte, len(ciphertext))
	stream.CryptBlocks(plaintext, ciphertext)

	plaintext = RemovePKCS7Padding(plaintext)

	return plaintext, nil
}

func DecryptCBC(ciphertext string, key []byte) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	return DecryptCBCRaw(data, key)
}

func PKCS7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padded := make([]byte, len(data)+padding)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	return padded
}

func PadToBlockSize(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	if padding == blockSize {
		return data
	}
	padded := make([]byte, len(data)+padding)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	return padded
}

func RemovePKCS7Padding(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	padding := int(data[len(data)-1])
	if padding < 1 || padding > aes.BlockSize {
		return data
	}

	expectedPaddingLen := aes.BlockSize - len(data)%aes.BlockSize
	if expectedPaddingLen == 0 {
		return data
	}

	if padding != expectedPaddingLen {
		return data
	}

	for i := len(data) - padding; i < len(data); i++ {
		if data[i] != byte(padding) {
			return data
		}
	}

	return data[:len(data)-padding]
}

func EncryptECB(plaintext []byte, key []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("AES-128-ECB requires 16-byte key, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	padded := PadToBlockSize(plaintext, aes.BlockSize)
	encrypted := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(encrypted[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}

	return encrypted, nil
}

func DecryptECB(ciphertext []byte, key []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("AES-128-ECB requires 16-byte key, got %d", len(key))
	}

	if len(ciphertext) == 0 {
		return ciphertext, nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	decrypted := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(decrypted[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}

	// Remove PKCS7 padding
	return RemovePKCS7Padding(decrypted), nil
}

func DecryptPKCS7Padding(data []byte, blockSize int) []byte {
	if len(data) == 0 {
		return data
	}

	padding := int(data[len(data)-1])
	if padding < 1 || padding > blockSize {
		return data
	}

	expectedPaddingLen := blockSize - len(data)%blockSize
	if expectedPaddingLen == 0 {
		return data
	}

	if padding != expectedPaddingLen {
		return data
	}

	return data[:len(data)-padding]
}

func StringToNullTerminated(s string, chunkLen int) []byte {
	buf := []byte(s)
	bufLen := len(buf)

	if bufLen >= chunkLen {
		result := make([]byte, bufLen)
		copy(result, buf)
		return result
	}

	result := make([]byte, chunkLen)
	copy(result, buf)
	return result
}

func ReadNullTerminatedString(data []byte) string {
	nullIndex := bytes.IndexByte(data, 0)
	if nullIndex == -1 {
		return string(data)
	}
	return string(data[:nullIndex])
}

func DecryptAPIData(base64Data string, key []byte) ([]byte, error) {
	if !strings.HasPrefix(base64Data, "data:") {
		data, err := base64.StdEncoding.DecodeString(base64Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode base64: %w", err)
		}
		return DecryptCBCRaw(data, key)
	}

	dataPart := strings.TrimPrefix(base64Data, "data:")
	data, err := base64.StdEncoding.DecodeString(dataPart)
	if err != nil {
		return nil, fmt.Errorf("failed to decode data: prefix: %w", err)
	}
	return DecryptCBC(string(data), key)
}
