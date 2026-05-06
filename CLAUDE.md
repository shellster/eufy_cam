# Eufy Camera P2P Protocol - Reference Guide

## Wire Format (UDP)

All P2P messages use this outer frame:

```
Outer Frame: [msgType 2 BE] [payloadLen 2 BE] [payload N bytes]
```

- `msg[0:2]` = message type (big-endian uint16), e.g. 0xf1d0 = DATA, 0xf1d1 = ACK
- `msg[2:4]` = payload length (big-endian uint16)
- `msg[4:]` = payload bytes

When we parse: `payload = msg[4:]`

## DATA Message Payload Format

DATA messages (msgType 0xf1d0) have this payload structure:

```
DATA Payload: [dataTypeHeader 2] [seqNo 2 BE] [data bytes...]
```

- `payload[0:2]` = dataTypeHeader, e.g. `[0xd1, 0x00]` = DATA, `[0xd1, 0x01]` = VIDEO
- `payload[2:4]` = sequence number (big-endian uint16)
- `payload[4:]` = actual data bytes (NOT payload[6:])

**CRITICAL**: The data portion starts at `payload[4:]`, NOT `payload[6:]`. There is NO extra length field between seqNo and data. The outer `payloadLen` (msg[2:4]) gives the total payload size.

## Sending Commands

### Send Header (10 bytes)

```
Send Header: [dataTypeHeader 2] [seqNo 2 BE] [magic "XZYH" 4] [commandType 2 LE]
```

- Bytes 0-1: dataTypeHeader (e.g. `[0xd1, 0x00]` for DATA)
- Bytes 2-3: sequence number (big-endian)
- Bytes 4-7: magic word "XZYH" = `[0x58, 0x5a, 0x59, 0x48]`
- Bytes 8-9: command type (little-endian uint16)

### Send Payload (after 10-byte header)

For string-type commands (like CMD_SET_PAYLOAD):

```
Send Payload: [dataLen 2 LE] [0x00 0x00] [0x01 0x00] [channel 1] [encFlag 1] [0x00 0x00] [data...]
```

- `dataLen` = length of data (after padding/encryption)
- `encFlag` = 0 for plaintext, 1 for LEVEL_1, 2 for LEVEL_2
- If encrypted: data is zero-padded to 16-byte boundary, then AES-128-ECB encrypted with p2pKey

## Receiving Data (Inner P2P Data Header)

After outer frame + data type + seqNo are stripped, the inner data starts with:

```
Inner Data (first packet): [magic "XZYH" 4] [commandID 2 LE] [bytesToRead 4 LE] [? 2] [channel 1] [signCode 1] [type 1]
```

- Bytes 0-3: "XZYH" magic word (identifies first packet of a message)
- Bytes 4-5: command ID (little-endian uint16)
- Bytes 6-9: bytes to read - total payload size (little-endian uint32)
- Bytes 10-11: unknown
- Byte 12: channel
- Byte 13: signCode (>0 means encrypted)
- Byte 14: type (1 = result message)

Total inner header = 16 bytes (P2PDataHeaderBytes).

After the 16-byte header, the actual command data follows. Large messages are split across multiple UDP packets and must be reassembled.

## Encryption Flow

1. After CAM_ID received, send CMD_GATEWAYINFO with void payload on channel 255
2. Station responds with GATEWAYINFO data containing:
   - `data[0:2]` = cipherID (little-endian uint16)
   - `data[4:]` = RSA-encrypted key (null-terminated)
3. Call API `v2/app/cipher/get_ciphers` with cipherID and adminUserID
4. If cipher found: RSA-PKCS1v15 decrypt encryptedKey → p2pKey (LEVEL_2)
5. If cipher not found: derive key = `SN[-7:] + p2pDid[firstDashPos:firstDashPos+9]` (LEVEL_1)
6. After encryption setup, signal ready and send queued commands

**LEVEL_1 key example**: SN="T8420N1234567890", p2pDid="ABCDEF-123456-XYZ"
  → key = "4567890" + "-123456-X" = "4567890-123456-X" (16 chars)

## Encrypted Commands

Commands in the encrypted list (includes CMD_SET_PAYLOAD=1350) must have their payload encrypted with AES-128-ECB when encryption is active. The `encFlag` in the send payload signals the encryption type to the station.

## Encrypted Responses

DATA responses with `signCode > 0` are encrypted with AES-128-ECB using p2pKey. Decrypt before parsing return codes.

## Video Frames

Video frames (CMD_VIDEO_FRAME=1300) have this structure:

```
Video Frame: [dataLength 4 LE] [isKeyFrame 1] [streamType 1] [seqNo 2 LE] [fps 2 LE] [width 2 LE] [height 2 LE] [timestamp 6 LE] [? 2] [payload...]
```

When `signCode > 0` and `dataLength >= 128`:
- Bytes 22-150: RSA-PKCS1v15 encrypted AES key (128 bytes)
- Byte 151+: video data, first 128 bytes AES-ECB encrypted with the decrypted key

## ACK Handling

ACK messages (msgType 0xf1d1) payload:

```
ACK Payload: [dataTypeHeader 2] [numPending 2 BE] [seqNo 2 BE]
```

Command flow:
1. Send command, track in messageStates with seqNo
2. Wait for ACK to confirm receipt
3. Wait for DATA response with matching seqNo for result
4. Only send next queued command after previous is acknowledged

## Key Command Types

| Command | ID | Description |
|---------|-----|-------------|
| CMD_GATEWAYINFO | 1100 | Initial handshake, triggers encryption setup |
| CMD_PING | 1139 | Keepalive ping |
| CMD_VIDEO_FRAME | 1300 | Video frame data |
| CMD_SET_PAYLOAD | 1350 | JSON command wrapper (encrypted) |
| CMD_START_REALTIME_MEDIA | 1003 | Start livestream (sent inside CMD_SET_PAYLOAD) |
| CMD_STOP_REALTIME_MEDIA | 1004 | Stop livestream |

## Common Pitfalls

1. **DO NOT** add extra fields between seqNo and data in DATA payload parsing
2. **DO NOT** confuse send header (10 bytes) with receive inner header (16 bytes)
3. **DO NOT** forget to encrypt CMD_SET_PAYLOAD payloads when encryption is active
4. **DO NOT** forget to decrypt DATA responses when signCode > 0
5. **DO NOT** forget the GATEWAYINFO encryption handshake - station will reject commands with error -148 without it
6. LEVEL_1 key INCLUDES the dash from p2pDid (substring from dash index, 9 chars)
7. The outer payload length is at `msg[2:4]`, NOT inside the data payload
