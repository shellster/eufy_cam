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
	connTimeout   time.Duration
	encTimeout    time.Duration
	discAttempts  int
	ackTimeout    time.Duration
	mu            sync.Mutex
}

var (
	activeStations = make(map[string]*Station)
	stationsMutex  sync.RWMutex
)

func NewStation(apiStation *api.Station) (*Station, error) {
	return &Station{
		apiStation:    apiStation,
		p2pClient:     nil,
		streamClients: make(map[string]*p2p.Client),
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
		return err
	}

	debuglog.Debugf("Station %s: waiting for P2P connection...", s.GetSerial())
	if !p2pClient.WaitForConnection(connTimeout) {
		return fmt.Errorf("station %s: P2P connection timeout", s.GetSerial())
	}
	debuglog.Debugf("Station %s: P2P connected, waiting for encryption...", s.GetSerial())

	if !p2pClient.WaitForEncryption(encTimeout) {
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

	if s.p2pClient == nil {
		return nil
	}

	err := s.p2pClient.Close()
	s.p2pClient = nil

	return err
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

func (s *Station) StartLivestream(deviceSN string, channel int, videoCodec int, callback p2p.StreamCallback) error {
	s.mu.Lock()
	dskKey := s.dskKey
	apiClient := s.apiClient
	connTimeout := s.connTimeout
	encTimeout := s.encTimeout
	discAttempts := s.discAttempts
	ackTimeout := s.ackTimeout

	// Close existing stream connection for this camera
	if sc, ok := s.streamClients[deviceSN]; ok {
		debuglog.Debugf("StartLivestream: closing existing stream connection for %s", deviceSN)
		_ = sc.Close()
		delete(s.streamClients, deviceSN)
	}
	s.mu.Unlock()

	// Create dedicated P2P connection for this camera's stream
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
		return fmt.Errorf("stream connection timeout for %s", deviceSN)
	}
	debuglog.Debugf("StartLivestream: P2P connected for %s, waiting for encryption", deviceSN)

	if !streamClient.WaitForEncryption(encTimeout) {
		return fmt.Errorf("stream encryption timeout for %s", deviceSN)
	}
	debuglog.Debugf("StartLivestream: encryption ready for %s", deviceSN)

	streamClient.SetVideoCallback(deviceSN, byte(channel), callback)

	s.mu.Lock()
	s.streamClients[deviceSN] = streamClient
	s.mu.Unlock()

	// Export RSA public key modulus, strip leading byte
	rsaPubKeyHex := ""
	if rsaKey != nil {
		n := rsaKey.N.Bytes()
		if len(n) > 1 {
			rsaPubKeyHex = fmt.Sprintf("%x", n[1:])
		}
	}

	payloadData := map[string]interface{}{
		"account_id": s.apiStation.Member.AdminUserID,
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

	payloadJSON, err := json.Marshal(payloadData)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	debuglog.Debugf("StartLivestream: account_id=%s channel=%d device=%s", s.apiStation.Member.AdminUserID, channel, deviceSN)

	return streamClient.SendCommandWithStringPayload(p2p.CMD_SET_PAYLOAD, byte(channel), string(payloadJSON))
}

func (s *Station) StopLivestream(deviceSN string, channel int) error {
	s.mu.Lock()
	sc, ok := s.streamClients[deviceSN]
	if ok {
		delete(s.streamClients, deviceSN)
	}
	s.mu.Unlock()

	if !ok {
		debuglog.Debugf("StopLivestream: no stream connection for %s", deviceSN)
		return nil
	}

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
		apiStation:    apiStation,
		p2pClient:     nil,
		streamClients: make(map[string]*p2p.Client),
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
