package crypto

import (
	"bytes"
	"testing"
)

func TestDecryptECBRawRejectsMisalignedInput(t *testing.T) {
	key := []byte("0123456789abcdef")
	bad := []byte("not-16-aligned!") // 15 bytes
	if _, err := DecryptECBRaw(bad, key); err == nil {
		t.Fatal("misaligned ciphertext must return an error, not panic")
	}
	if _, err := DecryptECBRaw(nil, key); err != nil {
		t.Fatalf("empty input should be a no-op: %v", err)
	}
	if _, err := DecryptECBRaw(make([]byte, 32), []byte("short")); err == nil {
		t.Fatal("short key must return an error")
	}
}

func TestDecryptCBCRawRejectsMisalignedInput(t *testing.T) {
	key := []byte("0123456789abcdef")
	if _, err := DecryptCBCRaw([]byte("15-bytes-bad!!"), key); err == nil {
		t.Fatal("misaligned ciphertext must return an error, not panic")
	}
	if _, err := DecryptCBCRaw(make([]byte, 16), []byte("short")); err == nil {
		t.Fatal("short key must return an error")
	}
}

func TestEncryptCBCRejectsShortKey(t *testing.T) {
	if _, err := EncryptCBC([]byte("secret"), []byte("short")); err == nil {
		t.Fatal("short key must return an error, not panic")
	}
}

func TestECBRoundTripNoPaddingStrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	// 20 bytes -> padded to 32; ECB raw decrypt must NOT strip trailing zeros
	plaintext := []byte("twenty bytes of d")
	enc, err := EncryptECB(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(enc) != 32 {
		t.Fatalf("encrypted length = %d, want 32", len(enc))
	}
	dec, err := DecryptECBRaw(enc, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.HasPrefix(dec, plaintext) {
		t.Fatal("decrypted data does not start with plaintext")
	}
	if len(dec) != 32 {
		t.Fatalf("raw decrypt must preserve zero padding, got %d bytes", len(dec))
	}
}
