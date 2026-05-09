package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/shellster/eufy_cam/config"
	"github.com/shellster/eufy_cam/pkg/api"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/pkg/devices"
	"github.com/shellster/eufy_cam/pkg/mpegts"
	"github.com/shellster/eufy_cam/pkg/p2p"
	"github.com/shellster/eufy_cam/pkg/stream"
)




func writeJSON(w http.ResponseWriter, v interface{}) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		debuglog.Debugf("json encode error: %v", err)
	}
}

// activeWatchers tracks WebSocket heartbeat connections per device.
// When the last watcher disconnects, a grace period timer starts
// before the livestream is actually stopped.
var (
	activeWatchers   = make(map[string]int)
	activeWatchersMu sync.Mutex
	stopTimers       = make(map[string]*time.Timer)

	streamClients    = make(map[string]int)
	streamClientsMu  sync.Mutex
	streamStopTimers = make(map[string]*time.Timer)
)

type Server struct {
	apiClient *api.Client
	config    *config.Config
	staticFS  embed.FS
	upgrader  websocket.Upgrader

	mu              sync.RWMutex
	loggedIn        bool
	captcha         *api.CaptchaChallenge
	onLoginSuccess  func()
}

func NewServer(apiClient *api.Client, cfg *config.Config, staticFS embed.FS) *Server {
	allowedHost := cfg.Server.Host
	if allowedHost == "0.0.0.0" || allowedHost == "" {
		allowedHost = "localhost"
	}
	return &Server{
		apiClient: apiClient,
		config:    cfg,
		staticFS:  staticFS,
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			u, err := url.Parse(origin)
			if err != nil {
				return false
			}
			return u.Hostname() == allowedHost || u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
		}},
	}
}

func (s *Server) SetCaptcha(captcha *api.CaptchaChallenge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captcha = captcha
}

func (s *Server) SetLoggedIn(loggedIn bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loggedIn = loggedIn
}

func (s *Server) SetOnLoginSuccess(fn func()) {
	s.onLoginSuccess = fn
}

func (s *Server) IsLoggedIn() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loggedIn
}

func (s *Server) RegisterRoutes(r *mux.Router) {
	r.HandleFunc("/", s.IndexHandler).Methods("GET")
	r.HandleFunc("/login", s.LoginPageHandler).Methods("GET")
	r.HandleFunc("/login/submit", s.LoginSubmitHandler).Methods("POST")
	r.HandleFunc("/login/captcha/image", s.LoginCaptchaImageHandler).Methods("GET")
	r.HandleFunc("/login/captcha/data", s.LoginCaptchaDataHandler).Methods("GET")
	r.HandleFunc("/login/refresh", s.LoginRefreshHandler).Methods("POST")
	r.HandleFunc("/api/login/status", s.LoginStatusHandler).Methods("GET")
	r.HandleFunc("/api/cameras", s.ListCamerasHandler).Methods("GET")
	r.HandleFunc("/api/stations", s.ListStationsHandler).Methods("GET")
	r.HandleFunc("/api/stream/start/{deviceSN}", s.StartStreamHandler).Methods("POST")
	r.HandleFunc("/api/stream/status/{deviceSN}", s.StreamStatusHandler).Methods("GET")
	r.HandleFunc("/api/stream/{deviceSN}", s.StreamHandler).Methods("GET")
	r.HandleFunc("/api/stream/ws/{deviceSN}", s.StreamWebSocketHandler)
	r.HandleFunc("/stream/{deviceSN}", s.StreamPageHandler).Methods("GET")
	sub, _ := fs.Sub(s.staticFS, "static")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
}

func (s *Server) RegisterStreamRoutes(r *mux.Router) {
	r.HandleFunc("/{deviceSN}", s.StreamPortHandler).Methods("GET")
}

func (s *Server) StreamPortHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	if deviceSN == "" {
		http.Error(w, "deviceSN required", http.StatusBadRequest)
		return
	}

	// Auto-start stream if not already running
	strm := stream.GetStream(deviceSN)
	if strm == nil || strm.IsStale() {
		if err := s.startStreamForDevice(deviceSN); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		strm = stream.GetStream(deviceSN)
	}
	if strm == nil {
		http.Error(w, "failed to start stream", http.StatusInternalServerError)
		return
	}

	// Track client
	streamID := strm.ID
	stationSN := strm.Station
	channel := strm.Channel

	streamClientsMu.Lock()
	if timer, ok := streamStopTimers[deviceSN]; ok {
		timer.Stop()
		delete(streamStopTimers, deviceSN)
	}
	streamClients[deviceSN]++
	count := streamClients[deviceSN]
	streamClientsMu.Unlock()
	log.Printf("Stream port: client connected to %s (active: %d)", deviceSN, count)

	defer func() {
		streamClientsMu.Lock()
		streamClients[deviceSN]--
		remaining := streamClients[deviceSN]
		if remaining <= 0 {
			delete(streamClients, deviceSN)
		}
		streamClientsMu.Unlock()

		if remaining <= 0 {
			streamClientsMu.Lock()
			streamStopTimers[deviceSN] = time.AfterFunc(30*time.Second, func() {
				streamClientsMu.Lock()
				delete(streamStopTimers, deviceSN)
				streamClientsMu.Unlock()

				stopped := stream.StopStreamIfID(deviceSN, streamID)
				if stopped {
					log.Printf("Stream port: grace period expired for %s, stopping livestream (streamID=%d)", deviceSN, streamID)
					station := devices.GetStation(stationSN)
					if station != nil {
						_ = station.StopLivestream(deviceSN, channel)
					}
				}
			})
			streamClientsMu.Unlock()
			log.Printf("Stream port: last client gone for %s, scheduled stop in 30s", deviceSN)
		} else {
			log.Printf("Stream port: client disconnected from %s (remaining: %d)", deviceSN, remaining)
		}
	}()

	// Stream MPEG-TS
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	codec := mpegts.CodecH264
	if strm.Codec == 2 {
		codec = mpegts.CodecH265
	}

	muxer := mpegts.NewMuxer(w, codec)
	defer muxer.Close()

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	var ptsMs uint64
	frameCount := 0
	lastFrameID := 0

	var lastRestartCheck time.Time
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if stream.GetStream(deviceSN) == nil {
				return
			}
			if time.Since(lastRestartCheck) > 10*time.Second && strm.IsStale() {
				lastRestartCheck = time.Now()
				go s.restartStaleStream(deviceSN)
			}
			frames, nextID := strm.GetFramesSince(lastFrameID)
			lastFrameID = nextID
			if len(frames) > 0 {
				for _, frame := range frames {
					if err := muxer.WriteFrame(frame, ptsMs); err != nil {
						return
					}
					ptsMs += 66
					frameCount++
				}
				if canFlush {
					flusher.Flush()
				}
			}
		case <-r.Context().Done():
			log.Printf("Stream port: client disconnected from %s after %d frames", deviceSN, frameCount)
			return
		}
	}
}

func (s *Server) startStreamForDevice(deviceSN string) error {
	device, err := s.findDevice(deviceSN)
	if err != nil {
		return fmt.Errorf("device not found: %s", deviceSN)
	}

	station := devices.GetStation(device.StationSN)
	if station == nil {
		return fmt.Errorf("station %s not found", device.StationSN)
	}

	if !station.IsConnected() {
		return fmt.Errorf("station %s is not connected", device.StationSN)
	}

	// If a stream is already active and not stale, reuse it
	if strm := stream.GetStream(deviceSN); strm != nil && !strm.IsStale() {
		return nil
	}

	s.stopLivestream(deviceSN)

	stream.StartStream(deviceSN, station.GetSerial(), device.Channel, int(p2p.VideoCodecH264))

	if err := station.StartLivestream(deviceSN, device.Channel, int(p2p.VideoCodecH264), s.onVideoFrame); err != nil {
		_ = stream.StopStream(deviceSN)
		return fmt.Errorf("failed to start livestream: %v", err)
	}

	log.Printf("Stream port: started livestream for %s on station %s", deviceSN, device.StationSN)
	return nil
}

func (s *Server) IndexHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsLoggedIn() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	data, _ := s.staticFS.ReadFile("static/index.html")
	_, _ = w.Write(data)
}

func (s *Server) LoginPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	data, _ := s.staticFS.ReadFile("static/login.html")
	_, _ = w.Write(data)
}

func (s *Server) LoginStatusHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	loggedIn := s.loggedIn
	hasCaptcha := s.captcha != nil
	s.mu.RUnlock()

	anyConnected := false
	for _, st := range devices.GetAllStations() {
		if st.IsConnected() {
			anyConnected = true
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]bool{
		"logged_in":         loggedIn,
		"captcha_needed":   hasCaptcha && !loggedIn,
		"stations_connected": anyConnected,
	})
}

func (s *Server) LoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	answer := r.FormValue("answer")

	s.mu.Lock()
	captcha := s.captcha
	s.mu.Unlock()

	if captcha == nil {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{"success": false, "error": "No captcha challenge available"})
		return
	}

	if answer == "" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{"success": false, "error": "Please enter the captcha answer"})
		return
	}

	err := s.apiClient.LoginWithCaptcha(captcha.CaptchaID, answer)
	if err != nil {
		loginErr, ok := err.(*api.LoginError)
		if ok && loginErr.Captcha != nil {
			s.mu.Lock()
			s.captcha = loginErr.Captcha
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, map[string]interface{}{"success": false, "error": "Wrong captcha, try again", "new_captcha": true})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		debuglog.Debugf("login failed: %v", err)
		writeJSON(w, map[string]interface{}{"success": false, "error": "Authentication failed"})
		return
	}

	s.mu.Lock()
	s.loggedIn = true
	s.captcha = nil
	s.mu.Unlock()

	log.Println("Login successful via captcha!")

	if s.onLoginSuccess != nil {
		go s.onLoginSuccess()
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]interface{}{"success": true})
}

func (s *Server) LoginCaptchaImageHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	captcha := s.captcha
	s.mu.RUnlock()

	if captcha == nil {
		http.Error(w, "no captcha available", http.StatusNotFound)
		return
	}

	imgData, err := s.apiClient.FetchCaptchaImage(captcha.CaptchaID)
	if err != nil {
		http.Error(w, "failed to fetch captcha image", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(imgData)
}

func (s *Server) LoginCaptchaDataHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	captcha := s.captcha
	s.mu.RUnlock()

	if captcha == nil {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{"available": false})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]interface{}{
		"available":       true,
		"item":            captcha.Item,
		"captcha_base64":  captcha.CaptchaBase64,
		"captcha_img":     captcha.CaptchaImg,
		"captcha_url":     captcha.CaptchaURL,
	})
}

func (s *Server) LoginRefreshHandler(w http.ResponseWriter, r *http.Request) {
	err := s.apiClient.Login()
	if err != nil {
		loginErr, ok := err.(*api.LoginError)
		if ok && loginErr.Captcha != nil {
			s.mu.Lock()
			s.captcha = loginErr.Captcha
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, map[string]interface{}{"success": true, "captcha_id": loginErr.Captcha.CaptchaID})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		debuglog.Debugf("login failed: %v", err)
		writeJSON(w, map[string]interface{}{"success": false, "error": "Authentication failed"})
		return
	}

	s.mu.Lock()
	s.loggedIn = true
	s.captcha = nil
	s.mu.Unlock()

	if s.onLoginSuccess != nil {
		go s.onLoginSuccess()
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]interface{}{"success": true, "logged_in": true})
}

func (s *Server) ListCamerasHandler(w http.ResponseWriter, r *http.Request) {
	cameras, err := s.getCameraList()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, cameras)
}

func (s *Server) ListStationsHandler(w http.ResponseWriter, r *http.Request) {
	stations, err := s.getStationList()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, stations)
}

func (s *Server) StartStreamHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	channelStr := r.URL.Query().Get("channel")
	channel := -1
	if channelStr != "" {
		if ch, err := strconv.Atoi(channelStr); err == nil {
			channel = ch
		}
	}

	device, err := s.findDevice(deviceSN)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if channel < 0 {
		channel = device.Channel
	}

	station := devices.GetStation(device.StationSN)
	if station == nil {
		http.Error(w, fmt.Sprintf("station %s not found", device.StationSN), http.StatusNotFound)
		return
	}

	if !station.IsConnected() {
		http.Error(w, fmt.Sprintf("station %s is not connected", device.StationSN), http.StatusServiceUnavailable)
		return
	}

	// If a stream is already active for this device, reuse it
	if strm := stream.GetStream(deviceSN); strm != nil && !strm.IsStale() {
		debuglog.Debugf("Stream already active for %s, reusing existing session (id=%d)", deviceSN, strm.ID)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]string{"status": "active", "device_sn": deviceSN})
		return
	}

	// Stop any stale existing stream for this device first
	s.stopLivestream(deviceSN)

	stream.StartStream(deviceSN, station.GetSerial(), channel, int(p2p.VideoCodecH264))

	if err := station.StartLivestream(deviceSN, channel, int(p2p.VideoCodecH264), s.onVideoFrame); err != nil {
		_ = stream.StopStream(deviceSN)
		debuglog.Debugf("failed to start livestream: %v", err)
		http.Error(w, "failed to start livestream", http.StatusInternalServerError)
		return
	}

	debuglog.Debugf("Started livestream for device %s on station %s", deviceSN, device.StationSN)

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"status": "started", "device_sn": deviceSN})
}


func (s *Server) StreamHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	if deviceSN == "" {
		http.Error(w, "deviceSN required", http.StatusBadRequest)
		return
	}

	strm := stream.GetStream(deviceSN)
	if strm == nil {
		http.Error(w, fmt.Sprintf("stream not found for device %s", deviceSN), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	codec := mpegts.CodecH264
	if strm.Codec == 2 {
		codec = mpegts.CodecH265
	}

	muxer := mpegts.NewMuxer(w, codec)
	defer muxer.Close()

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	var ptsMs uint64
	frameCount := 0
	lastFrameID := 0

	var lastRestartCheck time.Time
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Re-check stream is still active (WS heartbeat controls lifecycle)
			if stream.GetStream(deviceSN) == nil {
				return
			}
			if time.Since(lastRestartCheck) > 10*time.Second && strm.IsStale() {
				lastRestartCheck = time.Now()
				go s.restartStaleStream(deviceSN)
			}
			frames, nextID := strm.GetFramesSince(lastFrameID)
			lastFrameID = nextID
			if len(frames) > 0 {
				for _, frame := range frames {
					if err := muxer.WriteFrame(frame, ptsMs); err != nil { return }
					ptsMs += 66
					frameCount++
				}
				if frameCount <= 5 {
					debuglog.Debugf("StreamHandler: sent %d frames to %s (%d bytes total)", len(frames), deviceSN, frameCount)
				}
				if canFlush {
					flusher.Flush()
				}
			}
		case <-r.Context().Done():
			debuglog.Debugf("StreamHandler: client disconnected from %s after %d frames", deviceSN, frameCount)
			return
		}
	}
}

func (s *Server) StreamWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		debuglog.Debugf("StreamWebSocket: upgrade failed for %s: %v", deviceSN, err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Capture the stream session details at connect time so we only stop THIS stream on disconnect
	strm := stream.GetStream(deviceSN)
	var streamID int64
	var stationSN string
	var channel int
	if strm != nil {
		streamID = strm.ID
		stationSN = strm.Station
		channel = strm.Channel
	}

	activeWatchersMu.Lock()
	// Cancel any pending stop timer for this device
	if timer, ok := stopTimers[deviceSN]; ok {
		timer.Stop()
		delete(stopTimers, deviceSN)
	}
	activeWatchers[deviceSN]++
	count := activeWatchers[deviceSN]
	activeWatchersMu.Unlock()
	debuglog.Debugf("StreamWebSocket: watcher connected for %s (total watchers: %d, streamID=%d)", deviceSN, count, streamID)

	defer func() {
		activeWatchersMu.Lock()
		activeWatchers[deviceSN]--
		remaining := activeWatchers[deviceSN]
		if remaining <= 0 {
			delete(activeWatchers, deviceSN)
		}
		activeWatchersMu.Unlock()

		if remaining <= 0 {
			// Schedule stream stop after grace period instead of immediately
			activeWatchersMu.Lock()
			stopTimers[deviceSN] = time.AfterFunc(30*time.Second, func() {
				activeWatchersMu.Lock()
				delete(stopTimers, deviceSN)
				activeWatchersMu.Unlock()

				// Only stop if the stream hasn't been replaced by a new one
				stopped := stream.StopStreamIfID(deviceSN, streamID)
				if stopped {
					debuglog.Debugf("StreamWebSocket: grace period expired for %s, stopping livestream (streamID=%d)", deviceSN, streamID)
					station := devices.GetStation(stationSN)
					if station != nil {
						_ = station.StopLivestream(deviceSN, channel)
					}
				}
			})
			activeWatchersMu.Unlock()
			debuglog.Debugf("StreamWebSocket: last watcher gone for %s, scheduled stop in 30s", deviceSN)
		} else {
			debuglog.Debugf("StreamWebSocket: watcher disconnected for %s (remaining: %d)", deviceSN, remaining)
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, nil)
	})

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			debuglog.Debugf("StreamWebSocket: connection lost for %s: %v", deviceSN, err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	}
}

func (s *Server) StreamPageHandler(w http.ResponseWriter, r *http.Request) {
	data, _ := s.staticFS.ReadFile("static/player.html")
	_, _ = w.Write(data)
}

func (s *Server) StreamStatusHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	w.Header().Set("Content-Type", "application/json")

	strm := stream.GetStream(deviceSN)
	if strm == nil {
		writeJSON(w, map[string]interface{}{
			"active":     false,
			"frameCount": 0,
			"message":    "no stream session",
		})
		return
	}

	msg := "waiting for frames"
	if strm.FrameCount() > 0 {
		msg = "receiving frames"
	}

	writeJSON(w, map[string]interface{}{
		"active":     true,
		"frameCount": strm.FrameCount(),
		"codec":      strm.Codec,
		"message":    msg,
	})
}

func (s *Server) onVideoFrame(deviceSN string, frameData []byte, metadata p2p.VideoFrameMetadata) error {
	session := stream.GetStream(deviceSN)
	if session == nil {
		debuglog.Debugf("onVideoFrame: no stream session for %s", deviceSN)
		return fmt.Errorf("stream session not found")
	}

	session.AppendFrame(frameData, metadata)
	if session.FrameCount() <= 5 {
		debuglog.Debugf("onVideoFrame: %s frame #%d buffered, %d bytes, keyFrame=%v", deviceSN, session.FrameCount(), len(frameData), metadata.IsKeyFrame)
	}
	return nil
}

func (s *Server) restartStaleStream(deviceSN string) {
	strm := stream.GetStream(deviceSN)
	if strm == nil {
		return
	}
	if !strm.IsStale() {
		return
	}

	station := devices.GetStation(strm.Station)
	if station == nil || !station.IsConnected() {
		return
	}

	debuglog.Debugf("Restarting stale livestream for %s", deviceSN)
	_ = station.StopLivestream(deviceSN, strm.Channel)
	strm.ResetLastUpdate()
	strm.ClearBuffer()

	if err := station.StartLivestream(deviceSN, strm.Channel, strm.Codec, s.onVideoFrame); err != nil {
		debuglog.Debugf("Failed to restart livestream for %s: %v", deviceSN, err)
	}
}

func (s *Server) stopLivestream(deviceSN string) {
	strm := stream.GetStream(deviceSN)
	if strm == nil {
		debuglog.Debugf("stopLivestream: no active stream for %s", deviceSN)
		return
	}
	station := devices.GetStation(strm.Station)
	if station != nil {
		_ = station.StopLivestream(deviceSN, strm.Channel)
	}
	_ = stream.StopStream(deviceSN)
	debuglog.Debugf("Stopped livestream for device %s (station=%s channel=%d)", deviceSN, strm.Station, strm.Channel)
}

func (s *Server) getCameraList() ([]CameraInfo, error) {
	devs, err := s.apiClient.GetDevices()
	if err != nil {
		return nil, fmt.Errorf("failed to get devices: %w", err)
	}

	var cameras []CameraInfo
	for _, device := range devs {
		cameras = append(cameras, CameraInfo{
			DeviceSN:   device.DeviceSN,
			DeviceName: device.DeviceName,
			StationSN:  device.StationSN,
			Model:      device.Model,
			Channel:    device.Channel,
		})
	}

	return cameras, nil
}

func (s *Server) getStationList() ([]StationInfo, error) {
	stations, err := s.apiClient.GetStations()
	if err != nil {
		return nil, fmt.Errorf("failed to get stations: %w", err)
	}

	var stationInfos []StationInfo
	for _, station := range stations {
		s := devices.GetStation(station.StationSN)
		connected := s != nil && s.IsConnected()
		stationInfos = append(stationInfos, StationInfo{
			StationSN: station.StationSN,
			Name:      station.Name,
			Model:     station.Model,
			Connected: connected,
		})
	}

	return stationInfos, nil
}

func (s *Server) findDevice(deviceSN string) (*api.Device, error) {
	devs, err := s.apiClient.GetDevices()
	if err != nil {
		return nil, err
	}
	for _, device := range devs {
		if device.DeviceSN == deviceSN {
			return &device, nil
		}
	}
	return nil, fmt.Errorf("device not found: %s", deviceSN)
}

type CameraInfo struct {
	DeviceSN   string `json:"device_sn"`
	DeviceName string `json:"device_name"`
	StationSN  string `json:"station_sn"`
	Model      string `json:"model"`
	Channel    int    `json:"channel"`
	Online     bool   `json:"online"`
	Streaming  bool   `json:"streaming"`
}

type StationInfo struct {
	StationSN string `json:"station_sn"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Connected bool   `json:"connected"`
}
