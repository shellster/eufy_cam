package p2p

import (
	"context"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shellster/eufy_cam/pkg/api"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
)

const (
	DefaultPort          = 32108
	MaxRetries           = 10
	MaxConnectionTimeout = 25 * time.Second
	MaxAckTimeout        = 5 * time.Second
	MaxLookupTimeout     = 20 * time.Second
	HeartbeatInterval    = 3 * time.Second
	KeepaliveInterval    = 2 * time.Second
	MaxStreamDataWait    = 5 * time.Second
	MaxSequenceNumber    = 65535
	SequenceBoundary     = 20000
)

type P2PConnectionType int

const (
	ConnectionTypeOnlyLocal P2PConnectionType = 1
	ConnectionTypeQuickest  P2PConnectionType = 2
)

type EncryptionType int

const (
	EncryptionTypeNone   EncryptionType = 0
	EncryptionTypeLevel1 EncryptionType = 1
	EncryptionTypeLevel2 EncryptionType = 2
)

type CommandType uint16

const (
	CMD_START_REALTIME_MEDIA  CommandType = 1003
	CMD_STOP_REALTIME_MEDIA   CommandType = 1004
	CMD_SET_PAYLOAD           CommandType = 1350
	CMD_DOORBELL_SET_PAYLOAD  CommandType = 1700
	CMD_VIDEO_FRAME           CommandType = 1300
	CMD_GATEWAYINFO           CommandType = 1100
		CMD_PING                  CommandType = 1139
)

type Client struct {
	station   *api.Station
	apiClient APICipherGetter
	dskKey    string
	conn      *net.UDPConn
	localAddr *net.UDPAddr
	remoteAddr     *net.UDPAddr
	connected      bool
	connecting     bool
	connectedReady chan struct{}

	seqNumber       uint16
	expectedSeqNo   [4]uint16
	videoCallbacks   map[string]StreamCallback
	channelToDevice  map[byte]string

	encryption     EncryptionType
	p2pKey         []byte
	encryptionReady chan struct{}
	rsaKey         *rsa.PrivateKey

	msgBuilder     [4]*MessageBuilder
	msgState       [4]*MessageStateTracker

	sendQueue    []*QueuedMessage
	messageStates map[uint16]*MessageState

	turnHandshaking map[string]bool
	turnConfirmed   bool

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

type StreamCallback func(deviceSN string, frameData []byte, metadata VideoFrameMetadata) error

type APICipherGetter interface {
	GetCipher(cipherID int, userID string) (*api.Cipher, error)
}

type MessageBuilder struct {
	header    P2PDataHeader
	bytesRead int
	messages  map[int][]byte
}

type P2PDataHeader struct {
	commandID  uint16
	bytesToRead int
	channel    byte
	signCode   byte
	dataType   byte
}

type MessageStateTracker struct {
	leftoverData        []byte
	queuedData          map[int]*P2PMessage
	videoCodec          VideoCodec
	// Video frame reassembly
	videoFrameBuf       []byte
	videoBytesToRead    int
	videoBytesRead      int
	videoHeader         P2PDataHeader
}

type P2PMessage struct {
	dataType P2PDataType
	seqNo    uint16
	data     []byte
}

type QueuedMessage struct {
	commandType     CommandType
	channel         byte
	payload         []byte
}

type MessageState struct {
	seqNo        uint16
	commandType  CommandType
	channel      byte
	data         []byte
	acknowledged bool
	retries      int
	timeout      *time.Timer
}

func NewClient(station *api.Station, dskKey string, apiClient APICipherGetter) *Client {
	c := &Client{
		station:         station,
		apiClient:       apiClient,
		dskKey:          dskKey,
		connected:       false,
		connecting:      false,
		encryption:      EncryptionTypeNone,
		encryptionReady: make(chan struct{}),
		connectedReady:  make(chan struct{}),
		videoCallbacks:  make(map[string]StreamCallback),
		channelToDevice: make(map[byte]string),
		messageStates:   make(map[uint16]*MessageState),
		turnHandshaking: make(map[string]bool),
	}
	for i := 0; i < 4; i++ {
		c.msgBuilder[i] = &MessageBuilder{messages: make(map[int][]byte)}
		c.msgState[i] = &MessageStateTracker{
			queuedData:   make(map[int]*P2PMessage),
			leftoverData: []byte{},
		}
	}
	return c
}

func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected || c.connecting {
		return nil
	}

	c.connecting = true

	addr, err := NewUDPAddr()
	if err != nil {
		c.connecting = false
		return fmt.Errorf("failed to create UDP addr: %w", err)
	}

	c.localAddr = addr

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		c.connecting = false
		return fmt.Errorf("failed to listen UDP: %w", err)
	}

	c.conn = conn
	c.ctx, c.cancel = context.WithCancel(context.Background())

	go c.readLoop()
	go c.discoveryLoop()
	go c.heartbeatLoop()

	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cancel != nil {
		c.cancel()
	}
	c.connected = false
	c.connecting = false

	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Client) WaitForConnection(timeout time.Duration) bool {
	select {
	case <-c.connectedReady:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (c *Client) SetRSAKey(key *rsa.PrivateKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rsaKey = key
}

func (c *Client) WaitForEncryption(timeout time.Duration) bool {
	select {
	case <-c.encryptionReady:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (c *Client) discoveryLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	attempts := 0
	maxAttempts := 15

	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			done := c.connected
			c.mu.Unlock()
			if done {
				return
			}

			if attempts >= maxAttempts {
				debuglog.Debugf("P2P: discovery failed after %d attempts for %s", maxAttempts, c.station.StationSN)
				return
			}

			switch attempts % 4 {
			case 0:
				c.localLookup()
			case 1:
				c.cloudLookup()
			case 2:
				c.cloudLookup2()
			case 3:
				c.cloudLookup()
			}
			attempts++

		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			addr := c.remoteAddr
			connected := c.connected
			c.mu.Unlock()

			if connected && addr != nil {
				if err := c.sendRaw(MessageTypePING, addr, nil); err != nil {
					debuglog.Debugf("P2P: ping send failed: %v", err)
				}
			}

		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) localLookup() {
	addr := &net.UDPAddr{
		IP:   net.IPv4bcast,
		Port: DefaultPort,
	}

	p2pDidBuf := p2pDidToBuffer(c.station.P2PDID)
	if err := c.sendRaw(MessageTypeLOCAL_LOOKUP, addr, p2pDidBuf); err != nil {
		debuglog.Debugf("P2P: local lookup send failed: %v", err)
	}
}

func (c *Client) sendGatewayInfo() {
	c.mu.Lock()
	remoteAddr := c.remoteAddr
	seq := c.seqNumber
	c.seqNumber++
	c.mu.Unlock()

	if remoteAddr == nil {
		return
	}

	header := BuildCommandHeader(seq, uint16(CMD_GATEWAYINFO), DataTypeDATA)
	payload := BuildVoidCommandPayload(255)
	packet := append(header, payload...)

	debuglog.Debugf("P2P: Sending CMD_GATEWAYINFO to %s seq=%d packet=%x", remoteAddr, seq, packet)
	if err := c.sendRaw(MessageTypeDATA, remoteAddr, packet); err != nil {
		debuglog.Debugf("P2P: gateway info send failed: %v", err)
	}
}


func (c *Client) cloudLookup() {
	addrs := c.getCloudAddresses()
	for _, addr := range addrs {
		localPort, localIP := c.getLocalAddr()
		payload := BuildLookupWithKeyPayload(c.station.P2PDID, c.dskKey, localPort, localIP)
		if err := c.sendRaw(MessageTypeLOOKUP_WITH_KEY, addr, payload); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}
	}
}

func (c *Client) cloudLookup2() {
	addrs := c.getCloudAddresses()
	for _, addr := range addrs {
		payload := BuildLookupWithKeyPayload2(c.station.P2PDID, c.dskKey)
		if err := c.sendRaw(MessageTypeLOOKUP_WITH_KEY2, addr, payload); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}
	}
}

func (c *Client) getLocalAddr() (int, string) {
	if c.conn == nil {
		return 0, "0.0.0.0"
	}
	addr := c.conn.LocalAddr()
	if addr == nil {
		return 0, "0.0.0.0"
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr.String())
	if err != nil {
		return 0, "0.0.0.0"
	}
	ip := "0.0.0.0"
	if udpAddr.IP != nil && !udpAddr.IP.IsUnspecified() {
		ip = udpAddr.IP.String()
	}
	return udpAddr.Port, ip
}

func (c *Client) getCloudAddresses() []*net.UDPAddr {
	addrs := make([]*net.UDPAddr, 0)
	ips := decodeP2PCloudIPs(c.station.AppConn)
	for _, ip := range ips {
		udpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:32100", ip))
		if err != nil {
			continue
		}
		addrs = append(addrs, udpAddr)
	}
	return addrs
}

func decodeP2PCloudIPs(data string) []string {
	if data == "" {
		return nil
	}

	lookupTable, _ := hex.DecodeString("4959433db5bf6da347534f6165e371e9677f02030badb3892b2f35c16b8b959711e5a70deff1050783fb9d3bc5c713171d1f2529d3df")

	encoded := data
	if idx := strings.Index(data, ":"); idx >= 0 {
		encoded = data[:idx]
	}

	output := make([]byte, len(encoded)/2)
	for i := 0; i < len(encoded)/2; i++ {
		z := byte(0x39)
		for j := 0; j < i; j++ {
			z = z ^ output[j]
		}
		x := int(encoded[i*2+1]) - int('A')
		y := (int(encoded[i*2]) - int('A')) * 0x10
		output[i] = z ^ lookupTable[i%len(lookupTable)] ^ byte(x+y)
	}

	result := make([]string, 0)
	for _, ip := range strings.Split(string(output), ",") {
		if ip != "" {
			result = append(result, ip)
		}
	}
	return result
}

// sendRaw sends a raw UDP packet with the outer frame: [msgType 2 BE][payloadLen 2 BE][payload]
func (c *Client) sendRaw(msgType P2PMessageType, addr *net.UDPAddr, payload []byte) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	if payload == nil {
		payload = []byte{}
	}

	buf := make([]byte, 4+len(payload))
	buf[0] = byte(msgType >> 8)
	buf[1] = byte(msgType)
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(payload)))
	copy(buf[4:], payload)

	_, err := c.conn.WriteToUDP(buf, addr)
	return err
}

func (c *Client) sendCamCheck(addr *net.UDPAddr) {
	payload := BuildCheckCamPayload(c.station.P2PDID)
	if err := c.sendRaw(MessageTypeCHECK_CAM, addr, payload); err != nil {
		debuglog.Debugf("P2P: send failed: %v", err)
	}
}

func (c *Client) connectToAddr(addr *net.UDPAddr) {
	c.sendCamCheck(addr)
	for i := addr.Port - 3; i < addr.Port; i++ {
		if i > 0 {
			c.sendCamCheck(&net.UDPAddr{IP: addr.IP, Port: i})
		}
	}
	for i := addr.Port + 1; i <= addr.Port+3; i++ {
		c.sendCamCheck(&net.UDPAddr{IP: addr.IP, Port: i})
	}
}

func (c *Client) readLoop() {
	buf := make([]byte, 2048)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_ = c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, raddr, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			continue
		}

		c.handleMessage(buf[:n], raddr)
	}
}

// handleMessage dispatches incoming UDP packets by message type.
// All messages use the outer frame: [msgType 2 BE][payloadLen 2 BE][payload].
// msg[0:2] = message type, msg[2:4] = payload length, msg[4:] = payload.
func (c *Client) handleMessage(msg []byte, raddr *net.UDPAddr) {
	if len(msg) < 4 {
		return
	}

	msgType := P2PMessageType(binary.BigEndian.Uint16(msg[0:2]))
	payload := msg[4:]

	debuglog.Debugf("P2P: RECV msgType=0x%x from %s len=%d hex=%s", msgType, raddr, len(payload), hex.EncodeToString(payload[:min(len(payload), 32)]))

	switch msgType {
	case MessageTypeCAM_ID, MessageTypeCAM_ID2, MessageTypeTURN_SERVER_CAM_ID:
		c.mu.Lock()
		if c.connected {
			c.mu.Unlock()
			return
		}
		c.connected = true
		c.connecting = false
		c.remoteAddr = raddr
		c.mu.Unlock()
		debuglog.Debugf("P2P: CAM_ID received from %s - connected!", raddr)

		select {
		case <-c.connectedReady:
		default:
			close(c.connectedReady)
		}

		go func() {
			c.sendGatewayInfo()
		}()

	case MessageTypeLOOKUP_ADDR:
		debuglog.Debugf("P2P: LOOKUP_ADDR received from %s, payload len=%d", raddr, len(payload))
		c.handleLookupAddr(payload, raddr)

	case MessageTypeLOOKUP_ADDR2:
		debuglog.Debugf("P2P: LOOKUP_ADDR2 received from %s, payload len=%d", raddr, len(payload))
		c.handleLookupAddr2(payload, raddr)

	case MessageTypeLOCAL_LOOKUP_RESP:
		debuglog.Debugf("P2P: LOCAL_LOOKUP_RESP from %s", raddr)
		c.mu.Lock()
		if !c.connected {
			c.remoteAddr = raddr
		}
		c.mu.Unlock()
		c.sendCamCheck(raddr)

	case MessageTypeTURN_SERVER_OK:
		c.handleTurnServerOK(payload, raddr)

	case MessageTypeTURN_SERVER_TOKEN:
		c.handleTurnServerToken(payload, raddr)

	case MessageTypeTURN_SERVER_LOOKUP_OK, MessageTypeTURN_SERVER_LIST:
		debuglog.Debugf("P2P: TURN_SERVER_LOOKUP_OK/LIST from %s, len=%d", raddr, len(payload))

	case MessageTypePONG:
		// Heartbeat response

	case MessageTypePING:
		if err := c.sendRaw(MessageTypePONG, raddr, nil); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}

	case MessageTypeACK:
		if len(payload) >= 6 {
			debuglog.Debugf("P2P: ACK seq=%d dataType=%02x", binary.BigEndian.Uint16(payload[4:6]), payload[1])
			c.handleAck(payload)
		}

	case MessageTypeDATA:
		if len(payload) >= 4 {
			debuglog.Debugf("P2P: INCOMING DATA type=%02x seq=%d len=%d", payload[1], binary.BigEndian.Uint16(payload[2:4]), len(payload)-4)
		}
		c.handleDataMessage(payload, raddr)

	case MessageTypeEND:
		c.mu.Lock()
		if c.remoteAddr != nil && raddr.IP.Equal(c.remoteAddr.IP) && raddr.Port == c.remoteAddr.Port {
			c.connected = false
			debuglog.Debugf("P2P: END received from connected peer %s - disconnected", raddr)
		}
		c.mu.Unlock()
	}
}

func (c *Client) handleLookupAddr(payload []byte, raddr *net.UDPAddr) {
	if len(payload) < 8 {
		return
	}

	c.mu.Lock()
	alreadyConnected := c.connected
	c.mu.Unlock()

	if alreadyConnected {
		return
	}

	ip, port, err := ParseLookupAddrResponse(payload)
	if err != nil {
		return
	}

	debuglog.Debugf("P2P: LOOKUP_ADDR parsed: %s:%d", ip, port)

	if ip == "0.0.0.0" || ip == "" {
		// Station is behind NAT - connect via cloud relay
		// Use the cloud server as relay with the station's port
		debuglog.Debugf("P2P: Station behind NAT, connecting via cloud relay %s with station port %d", raddr, port)
		relayAddr := &net.UDPAddr{IP: raddr.IP, Port: port}
		c.connectToAddr(relayAddr)
		// Also try direct to cloud relay port
		c.sendCamCheck(raddr)
	} else {
		debuglog.Debugf("P2P: Station direct addr %s:%d", ip, port)
		stationAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
		if err == nil {
			c.connectToAddr(stationAddr)
		}
	}
}

func (c *Client) handleLookupAddr2(payload []byte, raddr *net.UDPAddr) {
	// Payload is 20 bytes. Node.js reads msg[6:8], msg[8:12], msg[20:24] from raw buffer.
	// Our payload starts at msg[4:], so subtract 4 from all Node.js offsets.
	// Port: payload[2:4] LE, IP: payload[4:8] reversed, data: payload[16:20]
	if len(payload) < 20 {
		return
	}

	c.mu.Lock()
	alreadyConnected := c.connected
	c.mu.Unlock()

	if alreadyConnected {
		return
	}

	port := int(binary.LittleEndian.Uint16(payload[2:4]))
	ip := fmt.Sprintf("%d.%d.%d.%d", payload[7], payload[6], payload[5], payload[4])
	data := make([]byte, 4)
	copy(data, payload[16:20])

	debuglog.Debugf("P2P: LOOKUP_ADDR2 parsed: %s:%d data=%x", ip, port, data)

	// Node.js does NOT check for 0.0.0.0 here — always send CHECK_CAM2
	checkPayload := BuildCheckCamPayload2(c.station.P2PDID, data)
	stationAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		debuglog.Debugf("P2P: LOOKUP_ADDR2 invalid addr %s:%d: %v", ip, port, err)
		return
	}
	for i := 0; i < 4; i++ {
		if err := c.sendRaw(MessageTypeCHECK_CAM2, stationAddr, checkPayload); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}
	}

	// Send TURN_SERVER_INIT (no payload)
	if !c.turnHandshaking[ip] {
		c.turnHandshaking[ip] = true
		if err := c.sendRaw(MessageTypeTURN_SERVER_INIT, stationAddr, nil); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}
		debuglog.Debugf("P2P: Sent TURN_SERVER_INIT to %s:%d", ip, port)
	}
}

// handleDataMessage processes incoming DATA messages (msgType 0xf1d0).
// The payload starts AFTER the 4-byte outer frame [msgType 2][payloadLen 2].
// payload layout: [dataTypeHeader 2][seqNo 2 BE][data bytes...]
// data starts at payload[4:], NOT payload[6:] — there is no extra length field.
func (c *Client) handleDataMessage(payload []byte, raddr *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}

	dataTypeBuffer := payload[0:2]
	dataType := c.getDataType(dataTypeBuffer)
	seqNo := binary.BigEndian.Uint16(payload[2:4])

	c.sendAckBuf(raddr, dataTypeBuffer, seqNo)

	data := payload[4:]

	dtIdx := int(dataType)
	if dtIdx < 0 || dtIdx >= 4 {
		return
	}

	c.mu.Lock()
	expectedSeq := c.expectedSeqNo[dtIdx]
	c.mu.Unlock()

	if seqNo == expectedSeq {
		c.mu.Lock()
		c.expectedSeqNo[dtIdx] = incrementSequence(c.expectedSeqNo[dtIdx])
		c.mu.Unlock()

		// VIDEO data: reassemble multi-packet frames, then process.
		if dataType == DataTypeVIDEO {
			ms := c.msgState[dtIdx]
			if len(data) >= P2PDataHeaderBytes && string(data[0:4]) == MagicWord {
				// First packet of a video frame
				header := P2PDataHeader{}
				header.commandID = binary.LittleEndian.Uint16(data[4:6])
				header.bytesToRead = int(binary.LittleEndian.Uint32(data[6:10]))
				header.channel = data[12]
				header.signCode = data[13]
				header.dataType = data[14]
				videoData := data[P2PDataHeaderBytes:]
				ms.videoHeader = header
				ms.videoBytesToRead = header.bytesToRead
				ms.videoBytesRead = len(videoData)
				ms.videoFrameBuf = make([]byte, len(videoData))
				copy(ms.videoFrameBuf, videoData)
			} else if ms.videoBytesToRead > 0 {
				// Continuation packet
				ms.videoFrameBuf = append(ms.videoFrameBuf, data...)
				ms.videoBytesRead += len(data)
			}
			if ms.videoBytesToRead > 0 && ms.videoBytesRead >= ms.videoBytesToRead {
				c.handleVideoData(seqNo, ms.videoHeader, ms.videoFrameBuf)
				ms.videoFrameBuf = nil
				ms.videoBytesToRead = 0
				ms.videoBytesRead = 0
			}
		} else {
			msg := &P2PMessage{
				dataType:   dataType,
				seqNo:      seqNo,
				data:       data,
			}
			c.parseDataMessage(msg)

			ms := c.msgState[dtIdx]
			for {
				c.mu.Lock()
				nextSeq := c.expectedSeqNo[dtIdx]
				c.mu.Unlock()
				queued, ok := ms.queuedData[int(nextSeq)]
				if !ok {
					break
				}
				c.mu.Lock()
				c.expectedSeqNo[dtIdx] = incrementSequence(c.expectedSeqNo[dtIdx])
				c.mu.Unlock()
				c.parseDataMessage(queued)
				delete(ms.queuedData, int(nextSeq))
			}
		}
	} else {
		ms := c.msgState[dtIdx]
		ms.queuedData[int(seqNo)] = &P2PMessage{
			dataType:   dataType,
			seqNo:      seqNo,
			data:       data,
		}
	}
}

func (c *Client) getDataType(buf []byte) P2PDataType {
	if len(buf) < 2 {
		return DataTypeDATA
	}
	switch buf[1] {
	case byte(DataTypeVIDEO):
		return DataTypeVIDEO
	case byte(DataTypeCONTROL):
		return DataTypeCONTROL
	case byte(DataTypeBINARY):
		return DataTypeBINARY
	default:
		return DataTypeDATA
	}
}

func (c *Client) sendAckBuf(addr *net.UDPAddr, dataTypeBuffer []byte, seqNo uint16) {
	ackPayload := make([]byte, 6)
	copy(ackPayload[0:2], dataTypeBuffer)
	binary.BigEndian.PutUint16(ackPayload[2:4], 1)
	binary.BigEndian.PutUint16(ackPayload[4:6], seqNo)
	if err := c.sendRaw(MessageTypeACK, addr, ackPayload); err != nil {
		debuglog.Debugf("P2P: send failed: %v", err)
	}
}

func incrementSequence(seq uint16) uint16 {
	return (seq + 1) % MaxSequenceNumber
}

// P2PDataHeaderBytes is the size of the inner P2P data header in received data.
// Format: [XZYH 4][commandID 2 LE][bytesToRead 4 LE][? 2][channel 1][signCode 1][type 1] = 16 bytes
// This is DIFFERENT from the send header which is only 10 bytes.
const P2PDataHeaderBytes = 16

// parseDataMessage reassembles multi-packet DATA messages.
// The data portion (from payload[4:]) may contain multiple inner messages.
// Each inner message starts with a 16-byte header beginning with "XZYH" magic.
// Large payloads span multiple UDP packets and are collected until bytesRead == bytesToRead.
func (c *Client) parseDataMessage(message *P2PMessage) {
	dtIdx := int(message.dataType)
	if dtIdx < 0 || dtIdx >= 4 {
		return
	}
	builder := c.msgBuilder[dtIdx]
	ms := c.msgState[dtIdx]

	if len(ms.leftoverData) > 0 {
		message.data = append(ms.leftoverData, message.data...)
		ms.leftoverData = []byte{}
	}

	data := message.data

	for len(data) > 0 {
		firstPart := len(data) >= 4 && string(data[0:4]) == MagicWord

		if firstPart {
			if len(data) < P2PDataHeaderBytes {
				ms.leftoverData = data
				break
			}

			header := P2PDataHeader{}
			header.commandID = binary.LittleEndian.Uint16(data[4:6])
			header.bytesToRead = int(binary.LittleEndian.Uint32(data[6:10]))
			header.channel = data[12]
			header.signCode = data[13]
			header.dataType = data[14]

			builder.header = header
			data = data[P2PDataHeaderBytes:]

			if len(data) >= header.bytesToRead {
				payload := data[:header.bytesToRead]
				builder.messages[int(message.seqNo)] = payload
				builder.bytesRead = len(payload)
				data = data[header.bytesToRead:]

				if len(data) > 0 && len(data) <= P2PDataHeaderBytes {
					ms.leftoverData = data
					data = nil
				}
			} else {
				if len(data) <= P2PDataHeaderBytes {
					ms.leftoverData = data
				} else {
					builder.messages[int(message.seqNo)] = data
					builder.bytesRead = len(data)
				}
				data = nil
			}
		} else {
			remaining := builder.header.bytesToRead - builder.bytesRead
			if remaining == 0 && len(data) > P2PDataHeaderBytes {
				data = nil
				continue
			} else if remaining <= len(data) {
				payload := data[:remaining]
				builder.messages[int(message.seqNo)] = payload
				builder.bytesRead += len(payload)
				data = data[remaining:]

				if len(data) > 0 && len(data) <= P2PDataHeaderBytes {
					ms.leftoverData = data
					data = nil
				}
			} else {
				if len(data) <= P2PDataHeaderBytes {
					ms.leftoverData = data
				} else {
					builder.messages[int(message.seqNo)] = data
					builder.bytesRead += len(data)
				}
				data = nil
			}
		}

		if builder.bytesRead == builder.header.bytesToRead && builder.header.bytesToRead > 0 {
			completeData := sortMessageParts(builder.messages)
			c.handleCompleteData(builder.header, message.seqNo, message.dataType, completeData)
			builder.header = P2PDataHeader{}
			builder.bytesRead = 0
			builder.messages = make(map[int][]byte)
		}
	}
}

func sortMessageParts(messages map[int][]byte) []byte {
	keys := make([]int, 0, len(messages))
	for k := range messages {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		diff := keys[i] - keys[j]
		if abs(diff) > 65000 {
			return keys[i] > keys[j]
		}
		return diff < 0
	})

	result := make([]byte, 0)
	for _, k := range keys {
		result = append(result, messages[k]...)
	}
	return result
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (c *Client) handleCompleteData(header P2PDataHeader, seqNo uint16, dataType P2PDataType, data []byte) {
	switch dataType {
	case DataTypeVIDEO:
		c.handleVideoData(seqNo, header, data)
	case DataTypeDATA, DataTypeCONTROL:
		c.handleCommandData(seqNo, header, data)
	}
}

func (c *Client) handleVideoData(seqNo uint16, header P2PDataHeader, data []byte) {
	c.mu.Lock()
	rsaKey := c.rsaKey
	// Look up which device SN this channel maps to
	deviceSN := c.channelToDevice[header.channel]
	cb := c.videoCallbacks[deviceSN]
	c.mu.Unlock()

	if CommandType(header.commandID) == CMD_VIDEO_FRAME {
		metadata, frameData, err := ParseVideoFrameWithDecryption(data, rsaKey, header.signCode)
		if err != nil {
			debuglog.Debugf("P2P: video frame parse error: %v", err)
			return
		}

		debuglog.Debugf("P2P: video frame ch=%d device=%s keyFrame=%v streamType=%d seqNo=%d fps=%d %dx%d dataLen=%d signCode=%d",
			header.channel, deviceSN, metadata.IsKeyFrame, metadata.StreamType, metadata.VideoSeqNo, metadata.VideoFPS,
			metadata.VideoWidth, metadata.VideoHeight, len(frameData), header.signCode)

		ms := c.msgState[DataTypeVIDEO]

		// Detect codec on first frame
		if ms.videoCodec == VideoCodecUnknown {
			ms.videoCodec = GetVideoCodec(frameData)
			if ms.videoCodec == VideoCodecUnknown {
				switch metadata.StreamType {
				case 1:
					ms.videoCodec = VideoCodecH264
				case 2:
					ms.videoCodec = VideoCodecH265
				}
			}
			if ms.videoCodec != VideoCodecUnknown {
				debuglog.Debugf("P2P: detected video codec: %d", ms.videoCodec)
			}
		}

		if cb != nil {
			if err := cb(deviceSN, frameData, metadata); err != nil {
				debuglog.Debugf("P2P: video callback error: %v", err)
			}
		}
	}
}

// handleCommandData processes assembled DATA command responses.
// When signCode > 0, data is AES-128-ECB encrypted with p2pKey and must be decrypted first.
// After decryption, data[0:4] contains the return code (LE int32).
func (c *Client) handleCommandData(seqNo uint16, header P2PDataHeader, data []byte) {
	cmdType := CommandType(header.commandID)

	// Decrypt DATA responses with signCode > 0 using p2pKey (AES-128-ECB)
	if header.signCode > 0 {
		c.mu.Lock()
		p2pKey := c.p2pKey
		enc := c.encryption
		c.mu.Unlock()

		if enc != EncryptionTypeNone && len(p2pKey) == 16 && len(data) >= aes.BlockSize {
			decrypted := DecryptECBPayload(data, p2pKey)
			data = decrypted
		}
	}

	debuglog.Debugf("P2P: command data seq=%d cmd=%d channel=%d signCode=%d len=%d hex=%x", seqNo, cmdType, header.channel, header.signCode, len(data), data[:min(len(data), 64)])

	switch cmdType {
	case CMD_GATEWAYINFO:
		c.handleGatewayInfo(data)
	case CMD_SET_PAYLOAD:
		c.handleSetPayloadResponse(data)
	}
}

// handleGatewayInfo processes the CMD_GATEWAYINFO response from the station.
// This is the encryption handshake that must complete before the station accepts commands.
// Response format: [cipherID 2 LE][unknown 2][encryptedKey N bytes (null-terminated)]
// Flow: extract cipherID → call API getCipher → if found: RSA decrypt key (LEVEL_2)
//       → if not found: derive key from SN+p2pDid (LEVEL_1 fallback)
func (c *Client) handleGatewayInfo(data []byte) {
	if len(data) < 4 {
		debuglog.Debugf("P2P: GATEWAYINFO response too short: %d bytes", len(data))
		c.setupEncryptionFallback()
		return
	}

	cipherID := binary.LittleEndian.Uint16(data[0:2])
	encryptedKey := readNullTerminated(data[4:])

	debuglog.Debugf("P2P: GATEWAYINFO cipherID=%d encryptedKeyLen=%d", cipherID, len(encryptedKey))

	if c.apiClient != nil && len(encryptedKey) > 0 {
		cipher, err := c.apiClient.GetCipher(int(cipherID), c.station.Member.AdminUserID)
		if err != nil {
			debuglog.Debugf("P2P: Failed to get cipher: %v, falling back to LEVEL_1", err)
			c.setupEncryptionFallback()
			return
		}

		if cipher != nil && cipher.PrivateKey != "" {
			p2pKey, decErr := decryptP2PKeyWithCipher(encryptedKey, cipher.PrivateKey)
			if decErr != nil {
				debuglog.Debugf("P2P: Failed to decrypt p2pKey with cipher: %v, falling back to LEVEL_1", decErr)
				c.setupEncryptionFallback()
				return
			}

			c.mu.Lock()
			c.encryption = EncryptionTypeLevel2
			c.p2pKey = p2pKey
			c.mu.Unlock()
			debuglog.Debugf("P2P: Encryption LEVEL_2 set, keyLen=%d", len(p2pKey))
			c.signalEncryptionReady()
			return
		}
	}

	c.setupEncryptionFallback()
}

func (c *Client) setupEncryptionFallback() {
	key := GetP2PEncryptionKey(c.station.StationSN, c.station.P2PDID)
	c.mu.Lock()
	c.encryption = EncryptionTypeLevel1
	c.p2pKey = []byte(key)
	c.mu.Unlock()
	debuglog.Debugf("P2P: Encryption LEVEL_1 set, key=%s (len=%d)", key, len(key))
	c.signalEncryptionReady()
}

func (c *Client) signalEncryptionReady() {
	select {
	case <-c.encryptionReady:
		// already closed
	default:
		close(c.encryptionReady)
	}
	c.sendQueuedMessage()
}

func readNullTerminated(data []byte) []byte {
	for i, b := range data {
		if b == 0 {
			return data[:i]
		}
	}
	return data
}

func decryptP2PKeyWithCipher(encryptedKey []byte, privateKeyPEM string) ([]byte, error) {
	pemData := privateKeyPEM
	// Strip newlines that may interfere
	pemData = strings.ReplaceAll(pemData, "\n", "")
	pemData = strings.ReplaceAll(pemData, "\r", "")
	// Re-add proper PEM formatting
	if !strings.Contains(pemData, "-----BEGIN") {
		// No PEM headers at all
		return nil, fmt.Errorf("private key has no PEM headers")
	}

	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to parse PEM private key")
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		key2, err2 := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse private key: %w (pkcs1: %w)", err, err2)
		}
		return rsa.DecryptPKCS1v15(rand.Reader, key2, encryptedKey)
	}

	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}

	return rsa.DecryptPKCS1v15(rand.Reader, rsaKey, encryptedKey)
}

func (c *Client) SendCommand(commandType CommandType, channel byte, payload []byte) error {
	c.mu.Lock()
	remoteAddr := c.remoteAddr
	seq := c.seqNumber
	c.seqNumber++
	if c.seqNumber > MaxSequenceNumber {
		c.seqNumber = 0
	}
	c.mu.Unlock()

	if remoteAddr == nil {
		return fmt.Errorf("no remote address")
	}

	header := BuildCommandHeader(seq, uint16(commandType), DataTypeDATA)
	packet := append(header, payload...)

	debuglog.Debugf("P2P: SendCommand cmd=%d seq=%d remote=%s header=%x payload=%x", commandType, seq, remoteAddr, header, payload[:min(len(payload), 32)])

	return c.sendRaw(MessageTypeDATA, remoteAddr, packet)
}

func (c *Client) SendCommandWithInt(commandType CommandType, channel byte, value uint32) error {
	c.mu.Lock()
	encType := c.encryption
	p2pKey := c.p2pKey
	c.mu.Unlock()

	payload := BuildIntCommandPayload(int(encType), p2pKey, uint16(commandType), value, "", channel)
	return c.SendCommand(commandType, channel, payload)
}

func (c *Client) SendCommandWithStringPayload(commandType CommandType, channel byte, value string) error {
	c.mu.Lock()
	remoteAddr := c.remoteAddr
	encType := c.encryption
	p2pKey := c.p2pKey
	c.mu.Unlock()

	if remoteAddr == nil {
		return fmt.Errorf("no remote address")
	}

	payload := BuildCommandWithStringTypePayload(int(encType), p2pKey, uint16(commandType), value, channel)
	return c.SendCommand(commandType, channel, payload)
}

func (c *Client) sendQueuedMessage() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.sendQueue) == 0 {
		return
	}

	if !c.connected {
		return
	}

	for i, msg := range c.sendQueue {
		exists := false
		waitingAck := false

		for _, state := range c.messageStates {
			if state.commandType == msg.commandType && !state.acknowledged {
				exists = true
			}
			if !state.acknowledged {
				waitingAck = true
			}
		}

		if !exists && !waitingAck {
			c.sendQueue = append(c.sendQueue[:i], c.sendQueue[i+1:]...)
			go c.executeSend(msg)
			return
		}
	}
}

func (c *Client) executeSend(msg *QueuedMessage) {
	c.mu.Lock()
	remoteAddr := c.remoteAddr
	seq := c.seqNumber
	c.seqNumber++
	if c.seqNumber > MaxSequenceNumber {
		c.seqNumber = 0
	}
	c.mu.Unlock()

	if remoteAddr == nil {
		return
	}

	header := BuildCommandHeader(seq, uint16(msg.commandType), DataTypeDATA)
	packet := append(header, msg.payload...)

	state := &MessageState{
		seqNo:        seq,
		commandType:  msg.commandType,
		channel:      msg.channel,
		data:         packet,
		acknowledged: false,
		retries:      0,
	}

	c.mu.Lock()
	c.messageStates[seq] = state
	c.mu.Unlock()

	state.timeout = time.AfterFunc(10*time.Second, func() {
		c.mu.Lock()
		delete(c.messageStates, seq)
		c.mu.Unlock()
		c.sendQueuedMessage()
	})

	debuglog.Debugf("P2P: Sending queued cmd=%d seq=%d remote=%s", msg.commandType, seq, remoteAddr)
	if err := c.sendRaw(MessageTypeDATA, remoteAddr, packet); err != nil {
		debuglog.Debugf("P2P: send failed: %v", err)
	}
}

func (c *Client) handleAck(payload []byte) {
	if len(payload) < 6 {
		return
	}

	seqNo := binary.BigEndian.Uint16(payload[4:6])

	c.mu.Lock()
	state, ok := c.messageStates[seqNo]
	c.mu.Unlock()

	if ok && !state.acknowledged {
		state.acknowledged = true
		if state.timeout != nil {
			state.timeout.Stop()
		}
		c.mu.Lock()
		delete(c.messageStates, seqNo)
		c.mu.Unlock()
		c.sendQueuedMessage()
	}
}

func (c *Client) handleSetPayloadResponse(data []byte) {
	if len(data) < 4 {
		debuglog.Debugf("P2P: CMD_SET_PAYLOAD response too short: %d bytes", len(data))
		return
	}
	returnCode := int32(binary.LittleEndian.Uint32(data[0:4]))
	debuglog.Debugf("P2P: CMD_SET_PAYLOAD response return_code=%d (0x%x)", returnCode, uint32(returnCode))
	if returnCode != 0 {
		debuglog.Debugf("P2P: WARNING: station rejected command (code %d) - livestream may not start", returnCode)
	}
}

func (c *Client) SetVideoCallback(deviceSN string, channel byte, callback StreamCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.videoCallbacks[deviceSN] = callback
	c.channelToDevice[channel] = deviceSN
}

func (c *Client) ClearVideoCallback() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.videoCallbacks = make(map[string]StreamCallback)
	c.channelToDevice = make(map[byte]string)
}

func (c *Client) ClearVideoCallbackForDevice(deviceSN string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.videoCallbacks, deviceSN)
	for ch, sn := range c.channelToDevice {
		if sn == deviceSN {
			delete(c.channelToDevice, ch)
		}
	}
	// Reset video accumulator state so next stream starts fresh
	ms := c.msgState[DataTypeVIDEO]
	ms.videoFrameBuf = nil
	ms.videoBytesToRead = 0
	ms.videoBytesRead = 0
	ms.videoCodec = VideoCodecUnknown
}

func (c *Client) handleTurnServerOK(payload []byte, raddr *net.UDPAddr) {
	c.mu.Lock()
	alreadyConnected := c.connected
	alreadyConfirmed := c.turnConfirmed
	c.mu.Unlock()

	if alreadyConnected || alreadyConfirmed {
		return
	}

	c.mu.Lock()
	c.turnConfirmed = true
	c.mu.Unlock()

	if err := c.sendRaw(MessageTypeTURN_CLIENT_OK, raddr, nil); err != nil {
		debuglog.Debugf("P2P: send failed: %v", err)
	}
	debuglog.Debugf("P2P: TURN_SERVER_OK from %s, sent TURN_CLIENT_OK", raddr)
}

func (c *Client) handleTurnServerToken(payload []byte, raddr *net.UDPAddr) {
	if len(payload) < 6 {
		return
	}

	c.mu.Lock()
	alreadyConnected := c.connected
	c.mu.Unlock()

	if alreadyConnected {
		return
	}

	// Node.js reads msg[4:8] for binaryIP, msg[8:10] for port from raw buffer.
	// Our payload starts at msg[4:], so: binaryIP=payload[0:4], port=payload[4:6]
	ip := fmt.Sprintf("%d.%d.%d.%d", payload[3], payload[2], payload[1], payload[0])
	port := int(binary.BigEndian.Uint16(payload[4:6]))
	binaryIP := make([]byte, 4)
	copy(binaryIP, payload[0:4])

	debuglog.Debugf("P2P: TURN_SERVER_TOKEN from %s: ip=%s port=%d binaryIP=%x", raddr, ip, port, binaryIP)

	// Send CHECK_CAM2 to cloud relay with token data
	checkPayload := BuildCheckCamPayload2(c.station.P2PDID, binaryIP)
	for i := 0; i < 4; i++ {
		if err := c.sendRaw(MessageTypeCHECK_CAM2, &net.UDPAddr{IP: raddr.IP, Port: port}, checkPayload); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}
	}

	// Send TURN_LOOKUP_WITH_KEY to all cloud addresses
	turnPayload := BuildLookupWithKeyPayload3(c.station.P2PDID, raddr.IP.String(), port, binaryIP)
	for _, addr := range c.getCloudAddresses() {
		if err := c.sendRaw(MessageTypeTURN_LOOKUP_WITH_KEY, addr, turnPayload); err != nil {
			debuglog.Debugf("P2P: send failed: %v", err)
		}
	}
}
