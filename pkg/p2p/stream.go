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
	VideoTimestamp  uint64
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

// GetVideoCodec sniffs the codec of a raw video frame by scanning its NAL
// unit headers. Cameras emit Annex-B framed NALs; H.264 uses 1-byte NAL
// headers while H.265/HEVC uses 2-byte headers. The two are distinguished by
// the NAL types found:
//
//	H.264: 1 (non-IDR), 5 (IDR), 7 (SPS), 8 (PPS), 9 (AUD)
//	H.265: 32 (VPS), 33 (SPS), 34 (PPS), 35 (AUD), 19/20 (IDR), 21 (CRA)
//
// Newer eufy cameras (SoloCam S340/E42, Indoor Pan&Tilt, dual-lens doorbells)
// default to H.265, whose leading VPS NAL (start code + 0x40 0x01) was
// previously undetectable, causing HEVC streams to be muxed into
// H.264-declared MPEG-TS and rendering as unplayable.
func GetVideoCodec(data []byte) VideoCodec {
	foundH264 := false
	foundH265 := false

	for i := 0; i+4 < len(data); {
		// Locate a start code (3-byte 00 00 01 or 4-byte 00 00 00 01).
		if data[i] != 0x00 || data[i+1] != 0x00 {
			i++
			continue
		}
		var nalStart int
		if data[i+2] == 0x01 {
			nalStart = i + 3
		} else if data[i+2] == 0x00 && i+4 < len(data) && data[i+3] == 0x01 {
			nalStart = i + 4
		} else {
			i++
			continue
		}
		if nalStart+1 >= len(data) {
			break
		}

		b0 := data[nalStart]
		if b0&0x80 == 0 { // forbidden_zero_bit must be 0 on a valid NAL
			// H.264: 1-byte header, nal type = b0 & 0x1F
			switch b0 & 0x1F {
			case 1, 5, 7, 8, 9:
				foundH264 = true
			}
			// H.265: 2-byte header, nal type = (b0 >> 1) & 0x3F
			switch (b0 >> 1) & 0x3F {
			case 32, 33, 34, 35, 19, 20, 21:
				foundH265 = true
			}
		}

		if foundH264 && !foundH265 {
			return VideoCodecH264
		}
		if foundH265 && !foundH264 {
			return VideoCodecH265
		}

		// Skip past this NAL header and keep scanning.
		i = nalStart + 2
	}

	if foundH264 {
		return VideoCodecH264
	}
	if foundH265 {
		return VideoCodecH265
	}
	return VideoCodecUnknown
}
