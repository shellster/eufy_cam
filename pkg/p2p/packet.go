package p2p

import (
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	MagicWord            = "XZYH"
	SendHeaderBytes      = 10
	RecvDataHeaderOffset = 20
	MaxPacketBytes       = 1024
	MaxPayloadBytes      = 1028
)

type P2PMessageType uint16

const (
	// Request message types
	MessageTypeSTUN                 P2PMessageType = 0xf100
	MessageTypeLOOKUP               P2PMessageType = 0xf120
	MessageTypeLOOKUP_WITH_KEY      P2PMessageType = 0xf126
	MessageTypeLOCAL_LOOKUP         P2PMessageType = 0xf130
	MessageTypeCHECK_CAM            P2PMessageType = 0xf141
	MessageTypeLOOKUP_WITH_KEY2     P2PMessageType = 0xf16a
	MessageTypeTURN_LOOKUP_WITH_KEY P2PMessageType = 0xf180
	MessageTypeCHECK_CAM2           P2PMessageType = 0xf183
	MessageTypeTURN_SERVER_INIT     P2PMessageType = 0xf170
	MessageTypeTURN_CLIENT_OK       P2PMessageType = 0xf172
	MessageTypePING                 P2PMessageType = 0xf1e0
	MessageTypePONG                 P2PMessageType = 0xf1e1
	MessageTypeDATA                 P2PMessageType = 0xf1d0
	MessageTypeACK                  P2PMessageType = 0xf1d1
	MessageTypeEND                  P2PMessageType = 0xf1f0

	// Response message types
	MessageTypeLOOKUP_ADDR          P2PMessageType = 0xf140
	MessageTypeCAM_ID               P2PMessageType = 0xf142
	MessageTypeCAM_ID2              P2PMessageType = 0xf143
	MessageTypeLOOKUP_ADDR2         P2PMessageType = 0xf182
	MessageTypeTURN_SERVER_CAM_ID   P2PMessageType = 0xf184
	MessageTypeTURN_SERVER_LIST     P2PMessageType = 0xf169
	MessageTypeTURN_SERVER_OK       P2PMessageType = 0xf171
	MessageTypeTURN_SERVER_TOKEN    P2PMessageType = 0xf173
	MessageTypeTURN_SERVER_LOOKUP_OK P2PMessageType = 0xf181
	MessageTypeSTUN_RESP            P2PMessageType = 0xf101
	MessageTypeLOOKUP_RESP          P2PMessageType = 0xf121
	MessageTypeLOCAL_LOOKUP_RESP    P2PMessageType = 0xf141
)

type P2PDataType byte

const (
	DataTypeDATA    P2PDataType = 0
	DataTypeVIDEO   P2PDataType = 1
	DataTypeCONTROL P2PDataType = 2
	DataTypeBINARY  P2PDataType = 3
)

var DataTypeHeader = map[P2PDataType][]byte{
	DataTypeDATA:    {0xd1, byte(DataTypeDATA)},
	DataTypeVIDEO:   {0xd1, byte(DataTypeVIDEO)},
	DataTypeCONTROL: {0xd1, byte(DataTypeCONTROL)},
	DataTypeBINARY:  {0xd1, byte(DataTypeBINARY)},
}

// p2pDidToBuffer converts a P2P DID string (e.g. "USPRAMB-498538-UEBKX") to a 20-byte buffer:
// [8-byte padded prefix][4-byte BE integer][8-byte padded suffix]
func p2pDidToBuffer(p2pDid string) []byte {
	parts := strings.Split(p2pDid, "-")
	if len(parts) != 3 {
		return make([]byte, 20)
	}

	buf := make([]byte, 20)
	// Prefix - 8 bytes null-padded
	copy(buf[0:8], []byte(parts[0]))
	// Number - 4 bytes big-endian
	num, _ := strconv.Atoi(parts[1])
	binary.BigEndian.PutUint32(buf[8:12], uint32(num))
	// Suffix - 8 bytes null-padded
	copy(buf[12:20], []byte(parts[2]))

	return buf
}

// stringWithLength pads/truncates a string to exactly n bytes
func stringWithLength(s string, n int) []byte {
	buf := make([]byte, n)
	copy(buf, []byte(s))
	return buf
}

// BuildLookupWithKeyPayload2 builds the simplified payload for LOOKUP_WITH_KEY2.
// Format: [p2pDID 20 bytes][dskKey][0x00 0x00 0x00 0x00]
func BuildLookupWithKeyPayload2(p2pDid string, dskKey string) []byte {
	p2pDidBuf := p2pDidToBuffer(p2pDid)
	dskKeyBuf := []byte(dskKey)
	fourEmpty := []byte{0x00, 0x00, 0x00, 0x00}

	result := make([]byte, 0)
	result = append(result, p2pDidBuf...)
	result = append(result, dskKeyBuf...)
	result = append(result, fourEmpty...)
	return result
}

// BuildLookupWithKeyPayload builds the payload for LOOKUP_WITH_KEY cloud lookup.
// Format: [p2pDID 20 bytes][0x00 0x02][port 2 bytes LE][ip 4 bytes reversed][magic 12 bytes][dskKey][0x00 0x00 0x00 0x00]
func BuildLookupWithKeyPayload(p2pDid string, dskKey string, localPort int, localIP string) []byte {
	p2pDidBuf := p2pDidToBuffer(p2pDid)
	splitter := []byte{0x00, 0x02}

	portBuf := make([]byte, 2)
	binary.LittleEndian.PutUint16(portBuf, uint16(localPort))

	// Reverse IP bytes
	ipParts := strings.Split(localIP, ".")
	ipBuf := make([]byte, 4)
	for i, part := range ipParts {
		if i < 4 {
			v, _ := strconv.Atoi(part)
			ipBuf[3-i] = byte(v)
		}
	}

	magic := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x04, 0x00, 0x00}
	dskKeyBuf := []byte(dskKey)
	fourEmpty := []byte{0x00, 0x00, 0x00, 0x00}

	result := make([]byte, 0)
	result = append(result, p2pDidBuf...)
	result = append(result, splitter...)
	result = append(result, portBuf...)
	result = append(result, ipBuf...)
	result = append(result, magic...)
	result = append(result, dskKeyBuf...)
	result = append(result, fourEmpty...)

	return result
}

// BuildCheckCamPayload2 builds the payload for CHECK_CAM2 with binary data.
// Format: [data][p2pDID 20 bytes][0x00 0x00 0x00 0x00]
func BuildCheckCamPayload2(p2pDid string, data []byte) []byte {
	p2pDidBuf := p2pDidToBuffer(p2pDid)
	fourEmpty := []byte{0x00, 0x00, 0x00, 0x00}

	result := make([]byte, 0)
	result = append(result, data...)
	result = append(result, p2pDidBuf...)
	result = append(result, fourEmpty...)
	return result
}

// BuildCheckCamPayload builds the payload for CHECK_CAM.
// Format: [p2pDID 20 bytes][0x00 0x00 0x00]
func BuildCheckCamPayload(p2pDid string) []byte {
	p2pDidBuf := p2pDidToBuffer(p2pDid)
	magic := []byte{0x00, 0x00, 0x00}
	return append(p2pDidBuf, magic...)
}

// BuildCommandHeader builds the 10-byte SEND command header.
// This is the header prepended to outbound commands.
// Format: [dataTypeHeader 2][seqNo 2 BE][magic "XZYH" 4][commandType 2 LE]
// NOTE: This is 10 bytes for SENDING. The RECEIVE inner header is 16 bytes (P2PDataHeaderBytes).
func BuildCommandHeader(seq uint16, commandType uint16, dataType P2PDataType) []byte {
	header := DataTypeHeader[dataType]
	seqBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(seqBuf, seq)
	magic := []byte(MagicWord)
	cmdBuf := make([]byte, 2)
	binary.LittleEndian.PutUint16(cmdBuf, commandType)

	result := make([]byte, 0, SendHeaderBytes)
	result = append(result, header...)
	result = append(result, seqBuf...)
	result = append(result, magic...)
	result = append(result, cmdBuf...)
	return result
}

// BuildIntCommandPayload builds a command payload with an integer value.
func BuildIntCommandPayload(encryptionType int, encryptionKey []byte, commandType uint16, value uint32, strValue string, channel byte) []byte {
	emptyBuf := []byte{0x00, 0x00}
	magicBuf := []byte{0x01, 0x00}

	encrypted := isP2PCommandEncrypted(CommandType(commandType)) && encryptionType != 0 && len(encryptionKey) == 16
	encryptionFlag := byte(0)
	if encrypted {
		encryptionFlag = byte(encryptionType)
	}

	channelBuf := []byte{channel, encryptionFlag}

	valueBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(valueBuf, value)

	strValueBuf := []byte{}
	if strValue != "" {
		strValueBuf = stringWithLength(strValue, 128)
	}

	dataBuf := append(valueBuf, strValueBuf...)

	if encrypted {
		dataBuf = padP2PData(dataBuf, aes.BlockSize)
		dataBuf = encryptECB(dataBuf, encryptionKey)
	}

	headerBuf := make([]byte, 2)
	binary.LittleEndian.PutUint16(headerBuf, uint16(len(dataBuf)))

	result := make([]byte, 0)
	result = append(result, headerBuf...)
	result = append(result, emptyBuf...)
	result = append(result, magicBuf...)
	result = append(result, channelBuf...)
	result = append(result, emptyBuf...)
	result = append(result, dataBuf...)

	return result
}

// BuildVoidCommandPayload builds a void command payload.
// Format: [0x00 0x00][0x00 0x00][0x01 0x00][channel 0x00][0x00 0x00]
func BuildVoidCommandPayload(channel byte) []byte {
	return []byte{0x00, 0x00, 0x00, 0x00, 0x01, 0x00, channel, 0x00, 0x00, 0x00}
}

// BuildCommandWithStringTypePayload builds the inner payload for string-type commands.
// This is the payload that goes AFTER the 10-byte send header.
// Format: [dataLen 2 LE][0x00 0x00][0x01 0x00][channel 1][encFlag 1][0x00 0x00][data...]
// When encrypted: data is zero-padded to 16-byte boundary, then AES-128-ECB encrypted.
// encFlag: 0=plaintext, 1=LEVEL_1, 2=LEVEL_2.
// Node.js reference: buildCommandWithStringTypePayload in utils.ts
func BuildCommandWithStringTypePayload(encryptionType int, encryptionKey []byte, commandType uint16, value string, channel byte) []byte {
	emptyBuf := []byte{0x00, 0x00}
	magicBuf := []byte{0x01, 0x00}

	encrypted := isP2PCommandEncrypted(CommandType(commandType)) && encryptionType != 0 && len(encryptionKey) == 16

	encFlag := byte(0)
	if encrypted {
		encFlag = byte(encryptionType)
	}

	channelBuf := []byte{channel, encFlag}

	var dataBuffer []byte
	if encrypted {
		dataBuffer = padP2PData([]byte(value), aes.BlockSize)
		dataBuffer = encryptECB(dataBuffer, encryptionKey)
	} else {
		dataBuffer = []byte(value)
	}

	headerBuf := make([]byte, 2)
	binary.LittleEndian.PutUint16(headerBuf, uint16(len(dataBuffer)))

	result := make([]byte, 0)
	result = append(result, headerBuf...)
	result = append(result, emptyBuf...)
	result = append(result, magicBuf...)
	result = append(result, channelBuf...)
	result = append(result, emptyBuf...)
	result = append(result, dataBuffer...)

	return result
}

func isP2PCommandEncrypted(cmd CommandType) bool {
	encryptedCmds := map[CommandType]bool{
		1001: true, 1002: true, 1004: true, 1005: true, 1006: true, 1007: true,
		1008: true, 1009: true, 1010: true, 1011: true, 1015: true, 1017: true,
		1019: true, 1035: true, 1045: true, 1056: true, 1145: true, 1146: true,
		1152: true, 1200: true, 1207: true, 1210: true, 1213: true, 1214: true,
		1226: true, 1227: true, 1229: true, 1230: true, 1233: true, 1236: true,
		1240: true, 1241: true, 1243: true, 1246: true, 1272: true, 1273: true,
		1275: true, 1350: true, 1400: true, 1401: true, 1402: true, 1403: true,
		1408: true, 1409: true, 1410: true, 1412: true, 1413: true, 1506: true,
		1507: true, 1607: true, 1609: true, 1610: true, 1611: true, 1700: true,
		1702: true, 1703: true, 1704: true, 1705: true, 1706: true, 1707: true,
		1708: true, 1709: true,
	}
	return encryptedCmds[cmd]
}

func padP2PData(data []byte, blockSize int) []byte {
	if len(data) < blockSize {
		padded := make([]byte, blockSize)
		copy(padded, data)
		return padded
	}
	roundUp := ((len(data) + blockSize - 1) / blockSize) * blockSize
	padded := make([]byte, roundUp)
	copy(padded, data)
	return padded
}

func encryptECB(data, key []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		return data
	}
	encrypted := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Encrypt(encrypted[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return encrypted
}

func decryptECB(data, key []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		return data
	}
	decrypted := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(decrypted[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return decrypted
}

func DecryptECBPayload(data, key []byte) []byte {
	return decryptECB(data, key)
}

// BuildAck builds an ACK packet payload.
func BuildAck(seq uint16, dataType P2PDataType) []byte {
	header := DataTypeHeader[dataType]
	numPending := make([]byte, 2)
	binary.BigEndian.PutUint16(numPending, 1)
	seqBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(seqBuf, seq)

	result := make([]byte, 0)
	result = append(result, header...)
	result = append(result, numPending...)
	result = append(result, seqBuf...)
	return result
}

func NewUDPAddr() (*net.UDPAddr, error) {
	return net.ResolveUDPAddr("udp", "0.0.0.0:0")
}

// ParseLookupAddrResponse extracts IP and port from a LOOKUP_ADDR response payload.
// Node.js reads msg[6:8] for port and msg[8:12] for IP from the raw buffer.
// Our payload starts at msg[4:], so Node's msg[6:8] = our payload[2:4], etc.
func ParseLookupAddrResponse(payload []byte) (string, int, error) {
	if len(payload) < 8 {
		return "", 0, fmt.Errorf("payload too short: %d bytes", len(payload))
	}

	port := int(binary.LittleEndian.Uint16(payload[2:4]))
	ip := fmt.Sprintf("%d.%d.%d.%d", payload[7], payload[6], payload[5], payload[4])

	return ip, port, nil
}

// BuildLookupWithKeyPayload3 builds the payload for TURN_LOOKUP_WITH_KEY.
// Format: [p2pDID 20 bytes][splitter 0x00 0x02][port 2 bytes LE][IP 4 bytes reversed][8 empty bytes][data]
func BuildLookupWithKeyPayload3(p2pDid string, host string, port int, data []byte) []byte {
	p2pDidBuf := p2pDidToBuffer(p2pDid)

	splitter := []byte{0x00, 0x02}

	portBuf := make([]byte, 2)
	binary.LittleEndian.PutUint16(portBuf, uint16(port))

	ipParts := strings.Split(host, ".")
	ipBuf := make([]byte, 4)
	for i, part := range ipParts {
		if i < 4 {
			v, _ := strconv.Atoi(part)
			ipBuf[3-i] = byte(v)
		}
	}

	eightEmpty := make([]byte, 8)

	result := make([]byte, 0)
	result = append(result, p2pDidBuf...)
	result = append(result, splitter...)
	result = append(result, portBuf...)
	result = append(result, ipBuf...)
	result = append(result, eightEmpty...)
	result = append(result, data...)
	return result
}
