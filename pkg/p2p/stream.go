package p2p

import (
	"crypto/aes"
	"crypto/rsa"
	"encoding/hex"
	"fmt"
)

type VideoCodec int

const (
	VideoCodecUnknown VideoCodec = -1
	VideoCodecH264    VideoCodec = 0
	VideoCodecH265    VideoCodec = 1
)

type VideoFrameMetadata struct {
	VideoDataLength uint32
	IsKeyFrame      bool
	StreamType      byte
	VideoSeqNo      uint16
	VideoFPS        uint16
	VideoWidth      uint16
	VideoHeight     uint16
	VideoTimestamp   uint64
	AESKey          string
}

func ParseVideoFrameWithDecryption(data []byte, rsaKey *rsa.PrivateKey, signCode byte) (VideoFrameMetadata, []byte, error) {
	if len(data) < 22 {
		return VideoFrameMetadata{}, nil, fmt.Errorf("video frame too short")
	}

	dataLength := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24

	metadata := VideoFrameMetadata{
		VideoDataLength: dataLength,
		IsKeyFrame:      data[4] == 1,
		StreamType:      data[5],
		VideoSeqNo:      uint16(data[6]) | uint16(data[7])<<8,
		VideoFPS:        uint16(data[8]) | uint16(data[9])<<8,
		VideoWidth:      uint16(data[10]) | uint16(data[11])<<8,
		VideoHeight:     uint16(data[12]) | uint16(data[13])<<8,
		VideoTimestamp:  uint64(data[14]) | uint64(data[15])<<8 | uint64(data[16])<<16 | uint64(data[17])<<24 | uint64(data[18])<<32 | uint64(data[19])<<40,
	}

	payloadStart := 22
	var videoData []byte

	if signCode > 0 && dataLength >= 128 && rsaKey != nil {
		if len(data) < 150 {
			return metadata, nil, fmt.Errorf("video frame too short for encrypted key")
		}

		encryptedAESKey := data[22:150]
		aesKeyHex, err := rsa.DecryptPKCS1v15(nil, rsaKey, encryptedAESKey)
		if err != nil {
			return metadata, nil, fmt.Errorf("failed to decrypt AES key: %w", err)
		}
		metadata.AESKey = hex.EncodeToString(aesKeyHex)

		payloadStart = 151

		if len(data) >= payloadStart+128 {
			encryptedData := data[payloadStart : payloadStart+128]
			decryptedData := decryptAESDataECB(metadata.AESKey, encryptedData)

			unencryptedLen := int(dataLength) - 128
			if unencryptedLen < 0 {
				unencryptedLen = 0
			}
			if payloadStart+128+unencryptedLen > len(data) {
				unencryptedLen = len(data) - payloadStart - 128
			}
			unencryptedData := data[payloadStart+128 : payloadStart+128+unencryptedLen]
			videoData = append(decryptedData, unencryptedData...)
		} else {
			videoLen := int(dataLength)
			if payloadStart+videoLen > len(data) {
				videoLen = len(data) - payloadStart
			}
			videoData = data[payloadStart : payloadStart+videoLen]
		}
	} else {
		videoLen := int(dataLength)
		if payloadStart+videoLen > len(data) {
			videoLen = len(data) - payloadStart
		}
		if videoLen < 0 {
			videoLen = 0
		}
		videoData = data[payloadStart : payloadStart+videoLen]
	}

	return metadata, videoData, nil
}

func decryptAESDataECB(hexKey string, data []byte) []byte {
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) == 0 {
		return data
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return data
	}

	decrypted := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		if i+aes.BlockSize <= len(data) {
			block.Decrypt(decrypted[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
		}
	}
	return decrypted
}

func GetVideoCodec(data []byte) VideoCodec {
	if len(data) < 5 {
		return VideoCodecUnknown
	}

	h264StartCodes := []byte{0x67, 0x41, 0x49, 0x26, 0x01}
	h265StartCodes := []byte{0x26, 0x02, 0x38, 0x64, 0x65}

	// Check for H.264 start codes at position 3-4
	isH264 := false
	for _, code := range h264StartCodes {
		if len(data) > 4 && data[3] == code {
			isH264 = true
			break
		}
	}

	if isH264 {
		return VideoCodecH264
	}

	// Check for H.265 start codes
	for _, code := range h265StartCodes {
		if len(data) > 4 && data[3] == code {
			return VideoCodecH265
		}
	}

	// Check deeper in data for NAL unit type header
	if len(data) >= 5 {
		nalType := data[4]
		if nalType == 0x67 || nalType == 0x41 || nalType == 0x49 || nalType == 0x26 {
			return VideoCodecH264
		}
		if nalType == 0x26 && len(data) > 5 && (data[5] == 0x02 || data[5] == 0x01) {
			return VideoCodecH265
		}
	}

	return VideoCodecUnknown
}

func IsIFrame(data []byte, isKeyFrame bool) bool {
	// If the metadata says it's a keyframe, trust it
	if isKeyFrame {
		return true
	}
	if len(data) < 5 {
		return false
	}

	nalType := data[4]

	// H.264 I-frame indicators (NAL type 5 or keyframe indicator)
	if nalType == 0x05 || nalType == 0x67 || nalType == 0x68 {
		return true
	}

	// Check for NAL unit header with keyframe flag
	if len(data) >= 5 && data[4] == 0x01 {
		// Look for reference picture ID field in NAL unit header
		if data[0] == 0x67 || data[0] == 0x41 || data[0] == 0x49 {
			// H.264 SPS or PPS, check for IDR
			return true
		}
	}

	return false
}

func FindStartCode(data []byte) int {
	for i := 0; i < len(data)-3; i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			return i
		}
	}
	return -1
}
