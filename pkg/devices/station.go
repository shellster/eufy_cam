package devices

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/shellster/eufy_cam/pkg/api"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/pkg/p2p"
)

type Station struct {
	apiStation    *api.Station
	p2pClient     *p2p.Client
	rsaKey        *rsa.PrivateKey
	apiClient     *api.Client
	dskKey        string
	streamClients map[string]*p2p.Client
	// streamKeys remembers the RSA public-modulus payload used for each
	// device's stream client so multi-lens starts reuse the same key.
	streamRSA map[string]*rsa.PrivateKey
	// streamChannels tracks the lenses currently streaming per device so the
	// shared P2P client is only closed when the LAST lens stops.
	streamChannels map[string]map[byte]bool
	connTimeout    time.Duration
	encTimeout     time.Duration
	discAttempts   int
	ackTimeout     time.Duration
	mu             sync.Mutex
}

var (
	activeStations = make(map[string]*Station)
	stationsMutex  sync.RWMutex
)

func NewStation(apiStation *api.Station) (*Station, error) {
	return &Station{
		apiStation:     apiStation,
		p2pClient:      nil,
		streamClients:  make(map[string]*p2p.Client),
		streamRSA:      make(map[string]*rsa.PrivateKey),
		streamChannels: make(map[string]map[byte]bool),
	}, nil
}

func (s *Station) Connect(dskKey string) error {
	return s.ConnectWithAPI(dskKey, nil)
}

func (s *Station) ConnectWithAPI(dskKey string, apiClient *api.Client) error {
	s.mu.Lock()

	if s.p2pClient != nil {
		s.mu.Unlock()
		return fmt.Errorf("already connected")
	}

	s.apiClient = apiClient
	s.dskKey = dskKey

	connTimeout := s.connTimeout
	encTimeout := s.encTimeout
	discAttempts := s.discAttempts
	ackTimeout := s.ackTimeout

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to generate RSA key: %w", err)
	}
	s.rsaKey = rsaKey

	p2pClient := p2p.NewClient(s.apiStation, dskKey, apiClient)
	p2pClient.SetRSAKey(rsaKey)
	if discAttempts > 0 || ackTimeout > 0 {
		p2pClient.SetTimeouts(discAttempts, ackTimeout)
	}
	s.p2pClient = p2pClient
	s.mu.Unlock()

	if err := p2pClient.Connect(); err != nil {
		s.mu.Lock()
		s.p2pClient = nil
		s.mu.Unlock()
		return err
	}

	debuglog.Debugf("Station %s: waiting for P2P connection...", s.GetSerial())
	if !p2pClient.WaitForConnection(connTimeout) {
		_ = p2pClient.Close()
		s.mu.Lock()
		s.p2pClient = nil
		s.mu.Unlock()
		return fmt.Errorf("station %s: P2P connection timeout", s.GetSerial())
	}
	debuglog.Debugf("Station %s: P2P connected, waiting for encryption...", s.GetSerial())

	if !p2pClient.WaitForEncryption(encTimeout) {
		_ = p2pClient.Close()
		s.mu.Lock()
		s.p2pClient = nil
		s.mu.Unlock()
		return fmt.Errorf("station %s: encryption setup timeout", s.GetSerial())
	}
	debuglog.Debugf("Station %s: encryption ready", s.GetSerial())

	return nil
}

func (s *Station) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for deviceSN, sc := range s.streamClients {
		debuglog.Debugf("Station %s: closing stream connection for %s", s.GetSerial(), deviceSN)
		_ = sc.Close()
	}
	s.streamClients = make(map[string]*p2p.Client)
	s.streamRSA = make(map[string]*rsa.PrivateKey)
	s.streamChannels = make(map[string]map[byte]bool)

	if s.p2pClient == nil {
		return nil
	}

	err := s.p2pClient.Close()
	s.p2pClient = nil

	return err
}

// MainConnected reports whether the primary P2P connection is alive.
// Livestream clients are separate connections and are not consulted.
func (s *Station) MainConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p2pClient != nil && s.p2pClient.IsConnected()
}

// ReconnectMain replaces a dead primary P2P connection WITHOUT touching any
// active livestream clients. Empty arguments fall back to the stored values.
func (s *Station) ReconnectMain(dskKey string, apiClient *api.Client) error {
	s.mu.Lock()
	if dskKey != "" {
		s.dskKey = dskKey
	}
	if apiClient != nil {
		s.apiClient = apiClient
	}
	old := s.p2pClient
	s.p2pClient = nil
	s.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	debuglog.Debugf("Station %s: reconnecting main P2P connection", s.GetSerial())
	return s.ConnectWithAPI(s.dskKey, s.apiClient)
}

func (s *Station) SetP2PTimeouts(connTimeout, encTimeout, ackTimeout time.Duration, discAttempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connTimeout = connTimeout
	s.encTimeout = encTimeout
	s.ackTimeout = ackTimeout
	s.discAttempts = discAttempts
}

func (s *Station) IsConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.p2pClient != nil && s.p2pClient.IsConnected() {
		return true
	}
	for _, sc := range s.streamClients {
		if sc.IsConnected() {
			return true
		}
	}
	return false
}

// StartLivestream opens (or reuses) the dedicated P2P connection for a
// device and starts the realtime stream on the given channel.
//
// Multi-lens devices (Battery Doorbell Plus E340, SoloCam S340/E42 pan-tilt)
// expose one lens per channel - e.g. the E340's main lens on device_channel
// and its package lens on device_channel+1. Channels of the same device share
// ONE P2P connection: a start for an already-connected device only registers
// a video callback for the new channel and sends the start command again.
func (s *Station) StartLivestream(deviceSN string, channel int, videoCodec int, callback p2p.StreamCallback) error {
	s.mu.Lock()
	dskKey := s.dskKey
	apiClient := s.apiClient
	connTimeout := s.connTimeout
	encTimeout := s.encTimeout
	discAttempts := s.discAttempts
	ackTimeout := s.ackTimeout

	// Multi-lens reuse: if a stream client is already live for this device,
	// share it instead of tearing it down (which would kill the other lens).
	existing := s.streamClients[deviceSN]
	if existing != nil && !existing.IsConnected() {
		debuglog.Debugf("StartLivestream: existing stream connection for %s is dead, replacing", deviceSN)
		_ = existing.Close()
		delete(s.streamClients, deviceSN)
		delete(s.streamRSA, deviceSN)
		delete(s.streamChannels, deviceSN)
		existing = nil
	}
	if chs, ok := s.streamChannels[deviceSN]; ok && chs[byte(channel)] {
		debuglog.Debugf("StartLivestream: channel %d already streaming for %s", channel, deviceSN)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ch := byte(channel)

	if existing != nil {
		// Shared-connection path: register the callback for this lens and
		// send the start command. The RSA key from the original connect is
		// kept so per-frame AES decryption stays consistent.
		debuglog.Debugf("StartLivestream: reusing P2P connection for %s channel %d", deviceSN, channel)
		existing.SetVideoCallback(deviceSN, ch, callback)

		rsaKey := s.streamRSA[deviceSN]
		payloadData := livestreamPayload(s.apiStation.Member.AdminUserID, channel, rsaKey)
		payloadJSON, err := json.Marshal(payloadData)
		if err != nil {
			return fmt.Errorf("failed to marshal payload: %w", err)
		}
		if err := existing.SendCommandWithStringPayload(p2p.CMD_SET_PAYLOAD, ch, string(payloadJSON)); err != nil {
			return fmt.Errorf("failed to send start command: %w", err)
		}
		s.mu.Lock()
		if s.streamChannels[deviceSN] == nil {
			s.streamChannels[deviceSN] = make(map[byte]bool)
		}
		s.streamChannels[deviceSN][ch] = true
		s.mu.Unlock()
		return nil
	}

	// Fresh connection path (original behavior).
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate RSA key: %w", err)
	}

	streamClient := p2p.NewClient(s.apiStation, dskKey, apiClient)
	streamClient.SetRSAKey(rsaKey)
	if discAttempts > 0 || ackTimeout > 0 {
		streamClient.SetTimeouts(discAttempts, ackTimeout)
	}

	debuglog.Debugf("StartLivestream: connecting dedicated P2P for %s on channel %d", deviceSN, channel)

	if err := streamClient.Connect(); err != nil {
		return fmt.Errorf("stream connect failed for %s: %w", deviceSN, err)
	}

	if !streamClient.WaitForConnection(connTimeout) {
		_ = streamClient.Close()
		return fmt.Errorf("stream connection timeout for %s", deviceSN)
	}
	debuglog.Debugf("StartLivestream: P2P connected for %s, waiting for encryption", deviceSN)

	if !streamClient.WaitForEncryption(encTimeout) {
		_ = streamClient.Close()
		return fmt.Errorf("stream encryption timeout for %s", deviceSN)
	}
	debuglog.Debugf("StartLivestream: encryption ready for %s", deviceSN)

	streamClient.SetVideoCallback(deviceSN, ch, callback)

	s.mu.Lock()
	s.streamClients[deviceSN] = streamClient
	s.streamRSA[deviceSN] = rsaKey
	if s.streamChannels[deviceSN] == nil {
		s.streamChannels[deviceSN] = make(map[byte]bool)
	}
	s.streamChannels[deviceSN][ch] = true
	s.mu.Unlock()

	payloadData := livestreamPayload(s.apiStation.Member.AdminUserID, channel, rsaKey)
	payloadJSON, err := json.Marshal(payloadData)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	debuglog.Debugf("StartLivestream: account_id=%s channel=%d device=%s", s.apiStation.Member.AdminUserID, channel, deviceSN)

	return streamClient.SendCommandWithStringPayload(p2p.CMD_SET_PAYLOAD, ch, string(payloadJSON))
}

// livestreamPayload builds the CMD_SET_PAYLOAD (1350) JSON for starting a
// realtime stream. Verified live against eufyCam 3C (H264), SoloCam S340 and
// SoloCam E42 pan-tilt (H265) and Battery Doorbell Plus E340 (dual-lens).
func livestreamPayload(accountID string, channel int, rsaKey *rsa.PrivateKey) map[string]interface{} {
	rsaPubKeyHex := ""
	if rsaKey != nil {
		n := rsaKey.N.Bytes()
		if len(n) > 1 {
			rsaPubKeyHex = fmt.Sprintf("%x", n[1:])
		}
	}
	return map[string]interface{}{
		"account_id": accountID,
		"cmd":        int(p2p.CMD_START_REALTIME_MEDIA),
		"mChannel":   channel,
		"mValue3":    int(p2p.CMD_START_REALTIME_MEDIA),
		"payload": map[string]interface{}{
			"ClientOS":    "Android",
			"camera_type": 0,
			"entrytype":   0,
			"key":         rsaPubKeyHex,
			"streamtype":  1,
		},
	}
}

// StopLivestream stops one lens of a device. The shared P2P connection is
// only closed when the device's LAST streaming lens stops.
func (s *Station) StopLivestream(deviceSN string, channel int) error {
	s.mu.Lock()
	sc, ok := s.streamClients[deviceSN]
	if ok {
		if chs := s.streamChannels[deviceSN]; chs != nil {
			delete(chs, byte(channel))
		}
		last := len(s.streamChannels[deviceSN]) == 0
		if !last {
			debuglog.Debugf("StopLivestream: stopping channel %d for %s (client shared)", channel, deviceSN)
			_ = sc.SendCommandWithInt(p2p.CMD_STOP_REALTIME_MEDIA, byte(channel), uint32(channel))
			s.mu.Unlock()
			return nil
		}
		delete(s.streamClients, deviceSN)
		delete(s.streamRSA, deviceSN)
		delete(s.streamChannels, deviceSN)
	} else {
		s.mu.Unlock()
		debuglog.Debugf("StopLivestream: no stream connection for %s", deviceSN)
		return nil
	}
	s.mu.Unlock()

	debuglog.Debugf("StopLivestream: closing stream connection for %s", deviceSN)
	_ = sc.SendCommandWithInt(p2p.CMD_STOP_REALTIME_MEDIA, byte(channel), uint32(channel))
	return sc.Close()
}

func (s *Station) GetSerial() string {
	return s.apiStation.StationSN
}

func AddOrUpdateStation(apiStation *api.Station) *Station {
	stationsMutex.Lock()
	defer stationsMutex.Unlock()

	if existing, ok := activeStations[apiStation.StationSN]; ok {
		existing.apiStation = apiStation
		return existing
	}

	station := &Station{
		apiStation:     apiStation,
		p2pClient:      nil,
		streamClients:  make(map[string]*p2p.Client),
		streamRSA:      make(map[string]*rsa.PrivateKey),
		streamChannels: make(map[string]map[byte]bool),
	}

	activeStations[apiStation.StationSN] = station
	return station
}

func GetStation(serial string) *Station {
	stationsMutex.RLock()
	defer stationsMutex.RUnlock()

	return activeStations[serial]
}

func GetAllStations() []*Station {
	stationsMutex.RLock()
	defer stationsMutex.RUnlock()

	stations := make([]*Station, 0, len(activeStations))
	for _, s := range activeStations {
		stations = append(stations, s)
	}
	return stations
}

func DisconnectAll() {
	stationsMutex.Lock()
	defer stationsMutex.Unlock()

	for _, s := range activeStations {
		_ = s.Disconnect()
	}
}
