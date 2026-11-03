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

### streamType IS the codec (0=H.264, 1=H.265)

`streamType` in the frame header directly encodes the codec, matching the
VideoCodec enum (H264=0, H265=1). Verified live:
- eufyCam 3C (T8134): streamType=0 (H264)
- SoloCam S340 / E42 pan-tilt: streamType=1 (**H.265** — these cameras
  default to H.265; muxing HEVC into an H.264-declared TS makes the stream
  unplayable, which is the classic "camera streams but shows nothing" bug)
- Battery Doorbell Plus E340: streamType=0 (H264)

Detect the codec from `metadata.StreamType` FIRST (authoritative), falling
back to NAL-header sniffing (`GetVideoCodec`) which must handle both 1-byte
H.264 NAL headers and 2-byte HEVC NAL headers (VPS=32/SPS=33/PPS=34/AUD=35).

### The isKeyFrame header byte is NOT reliable

Pan-tilt cameras (SoloCam S340/E42) never set the `isKeyFrame` flag in the
frame header, even for real IDR frames. Keyframe detection must scan the
AU's NALs for IDR/CRA (HEVC nal types 19/20/21, H.264 type 5) — and it must
walk ALL NALs: keyframe AUs lead with VPS/SPS/PPS/SEI before the IDR slice,
so checking only the first NAL sees the VPS and misses every keyframe.
Without this, keyframe-gated serving starves and clients join mid-GOP.

## Multi-Lens Devices

HomeBase channel slots are DEVICE slots, not lens slots. This was verified
with packet-level content fingerprinting: requesting `device_channel + 1`
delivers the NEXT DEVICE's feed (96/100 TS packets matched the neighbor
camera), not a second lens.

Multi-lens cameras deliver ALL lenses composited in ONE stream on the
device's own channel — verified on the Battery Doorbell Plus E340 (T8214):
a single portrait 1600x2200 stream = main lens with the package lens view
stacked below. NEVER add +1 channel starts for a second lens.

The P2P layer routes frames per channel (`videoRoutes[channel]`), so the
stream callback carries the channel and sessions are keyed `deviceSN#channel`.

## Battery/Solar Camera Wake-Up + H.265 Web Playback

- Battery/solar cameras (SoloCam S340/E42, battery doorbells) SLEEP until
  commanded to stream and routinely take 10-30s to wake and deliver the first
  frame. The stream monitor MUST NOT declare such streams stale during this
  window (`WarmUpStaleThreshold` = 40s for sessions that never produced a
  frame, vs 8s for established feeds) or it churn-restarts the P2P session in
  a loop and the camera never finishes waking - the web app then never shows
  video.
- These cameras stream H.265 (`frame streamType=1`). Browsers cannot decode
  HEVC via MSE, so the web player uses the `/api/raw/{SN}` endpoint (framed
  Annex-B AUs) with the vendored hevc.js WASM decoder rendering to a canvas —
  works in every browser, no transcode step and no ffmpeg anywhere.
- Rapid successive stream requests can put battery cameras in a temporary
  refuse state: if a camera stops responding to stream requests, stop the
  stream, wait ~60s, and re-request.

## Sequence Gap Handling (CRITICAL — verified against live logs)

Video packets are sequenced per dataType. The station runs a go-back-N
retransmit: whenever a packet's per-packet ACK is missing (packet lost OR the
ACK itself lost), it re-bursts its whole un-ACKed window. That means:

- **Duplicates are normal and healthy.** A re-burst delivers 10+ consecutive
  duplicates of already-received packets while the ONE genuinely missing
  packet is still in flight. Measured on the relayed path: the retransmit of
  the missing seq arrives up to ~2.7s (81 packets) after the gap opened.
- **NEVER skip a gap on duplicate streaks.** Skipping forward discards the
  in-flight retransmit when it lands (observed in logs: resync skipped seq N,
  then `dropping duplicate video packet seq=N` arrived 81 packets later).
  Every skip manufactures a permanent hole: the AU reassembles with missing
  bytes, the HEVC reference chain breaks ("Could not find ref with POC"),
  and the picture decodes as gray skip-blocks with only motion fragments
  visible until the next keyframe. THIS WAS THE ROOT CAUSE of the months-long
  "95% gray" H.265 playback bug.
- **Hold gaps open** (`videoResyncThreshold = 4s`, > measured retransmit
  latency): packets ahead of the gap are queued; when the retransmit arrives,
  the queue drains strictly in order and the frame reassembles intact. The
  cost of a real loss is a brief freeze, not corruption.
- **ACK every received packet** (`handleDataMessage` ACKs before seq
  handling, including duplicates) — that is what slides the station's
  retransmit window and stops the re-bursts.
- Only after the 4s hold expires with no fill (true loss): resync to the
  LOWEST QUEUED packet (`resyncVideoQueue`), reset frame reassembly, and let
  the keyframe-gated serve heal the decode chain. NEVER move
  `expectedSeqNo` backward.

The protocol's full-featured transport layer implements this same contract
via NACK/SACK control packets (uTP pt=2, RTCP-style PID+BLP chunks, u64 BE
una_seq), retransmit history, FEC, and drop-until-keyframe recovery. Our Go
client speaks the legacy protocol, where the equivalent feedback is per-packet
ACKs + the station's go-back-N — the rules above are how to be a well-behaved
legacy receiver.

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
