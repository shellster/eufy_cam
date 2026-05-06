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
	apiStation *api.Station
	p2pClient  *p2p.Client
	rsaKey     *rsa.PrivateKey
	apiClient  *api.Client
	mu         sync.Mutex
}

var (
	activeStations = make(map[string]*Station)
	stationsMutex  sync.RWMutex
)

func NewStation(apiStation *api.Station) (*Station, error) {
	return &Station{
		apiStation: apiStation,
		p2pClient:  nil,
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

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to generate RSA key: %w", err)
	}
	s.rsaKey = rsaKey

	p2pClient := p2p.NewClient(s.apiStation, dskKey, apiClient)
	p2pClient.SetRSAKey(rsaKey)
	s.p2pClient = p2pClient
	s.mu.Unlock()

	if err := p2pClient.Connect(); err != nil {
		return err
	}

	debuglog.Debugf("Station %s: waiting for P2P connection...", s.GetSerial())
	if !p2pClient.WaitForConnection(30 * time.Second) {
		return fmt.Errorf("station %s: P2P connection timeout", s.GetSerial())
	}
	debuglog.Debugf("Station %s: P2P connected, waiting for encryption...", s.GetSerial())

	if !p2pClient.WaitForEncryption(15 * time.Second) {
		return fmt.Errorf("station %s: encryption setup timeout", s.GetSerial())
	}
	debuglog.Debugf("Station %s: encryption ready", s.GetSerial())

	return nil
}

func (s *Station) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.p2pClient == nil {
		return nil
	}

	err := s.p2pClient.Close()
	s.p2pClient = nil

	return err
}

func (s *Station) IsConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.p2pClient != nil && s.p2pClient.IsConnected()
}

func (s *Station) StartLivestream(deviceSN string, channel int, videoCodec int, callback p2p.StreamCallback) error {
	if s.p2pClient == nil {
		return fmt.Errorf("not connected to station")
	}

	s.p2pClient.SetVideoCallback(deviceSN, byte(channel), callback)

	// Export RSA public key modulus, strip leading byte (matching Node.js: n.subarray(1).toString("hex"))
	rsaPubKeyHex := ""
	if s.rsaKey != nil {
		n := s.rsaKey.N.Bytes()
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
			"ClientOS":     "Android",
			"camera_type": 0,
			"entrytype":    0,
			"key":          rsaPubKeyHex,
			"streamtype":   1,
		},
	}

	payloadJSON, err := json.Marshal(payloadData)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	debuglog.Debugf("StartLivestream: account_id=%s channel=%d json=%s", s.apiStation.Member.AdminUserID, channel, string(payloadJSON))

	return s.p2pClient.SendCommandWithStringPayload(p2p.CMD_SET_PAYLOAD, byte(channel), string(payloadJSON))
}

func (s *Station) StopLivestream(deviceSN string, channel int) error {
	if s.p2pClient == nil || !s.p2pClient.IsConnected() {
		return fmt.Errorf("not connected to station")
	}

	s.p2pClient.ClearVideoCallbackForDevice(deviceSN)

	err := s.p2pClient.SendCommandWithInt(p2p.CMD_STOP_REALTIME_MEDIA, byte(channel), uint32(channel))
	if err != nil {
		return fmt.Errorf("failed to send stop livestream: %w", err)
	}

	return nil
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
		apiStation: apiStation,
		p2pClient:  nil,
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
