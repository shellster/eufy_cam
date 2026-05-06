package crypto

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
)

func MD5(data []byte) string {
	hash := md5.Sum(data)
	return hex.EncodeToString(hash[:])
}

func SHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func SHA256Bytes(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
}

func MD5Bytes(data []byte) []byte {
	hash := md5.Sum(data)
	return hash[:]
}
