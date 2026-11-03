package web

import (
	"encoding/binary"
	"embed"
	"encoding/json"
	"errors"
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
	"github.com/shellster/eufy_cam/pkg/devices"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/pkg/mpegts"
	"github.com/shellster/eufy_cam/pkg/p2p"
	"github.com/shellster/eufy_cam/pkg/stream"
)

func writeJSON(w http.ResponseWriter, v interface{}) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		debuglog.Debugf("json encode error: %v", err)
	}
}

var (
	errDeviceNotFound  = errors.New("device not found")
	errStationNotFound = errors.New("station not found")
	errStationOffline  = errors.New("station not connected")
	errLivestreamStart = errors.New("failed to start livestream")
)

// watcherSet tracks heartbeat/client connections per device. When the last
// one leaves, the expire callback runs after a grace period, cancelled if a
// new watcher arrives first. Shared by the WebSocket heartbeat tracker and
// the stream-port client tracker.
type watcherSet struct {
	mu     sync.Mutex
	counts map[string]int
	timers map[string]*time.Timer
	grace  time.Duration
}

func newWatcherSet(grace time.Duration) *watcherSet {
	return &watcherSet{
		counts: make(map[string]int),
		timers: make(map[string]*time.Timer),
		grace:  grace,
	}
}

func (w *watcherSet) add(deviceSN string, channel int) int {
	key := fmt.Sprintf("%s#%d", deviceSN, channel)
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[key]; ok {
		t.Stop()
		delete(w.timers, key)
	}
	w.counts[key]++
	return w.counts[key]
}

func (w *watcherSet) remove(deviceSN string, channel int, expire func()) {
	key := fmt.Sprintf("%s#%d", deviceSN, channel)
	w.mu.Lock()
	defer w.mu.Unlock()
	count, ok := w.counts[key]
	if !ok {
		// Spurious remove (double disconnect, error path after a failed
		// start): nothing is tracked. No-op — decrementing an absent key
		// would re-arm a second grace timer while the first still fires.
		return
	}
	count--
	if count > 0 {
		w.counts[key] = count
		return
	}
	delete(w.counts, key)
	w.timers[key] = time.AfterFunc(w.grace, func() {
		w.mu.Lock()
		delete(w.timers, key)
		// Re-check under the lock: add() cancels PENDING timers, but a
		// timer whose callback already started cannot be cancelled — a
		// consumer that arrived milliseconds ago must not have its stream
		// killed by an expire that raced its own add().
		empty := w.counts[key] == 0
		w.mu.Unlock()
		if empty {
			expire()
		}
	})
}


// resolveChannel determines the device channel a request targets: an
// explicit ?channel= override, else the device's own channel from the cloud
// device list. Deterministic on every endpoint so all watchers for one
// camera land on the same streamWatchers key.
func resolveChannel(r *http.Request, s *Server, deviceSN string) (int, bool) {
	channel := -1
	if ch, err := strconv.Atoi(r.URL.Query().Get("channel")); err == nil {
		channel = ch
	}
	if channel < 0 {
		device, err := s.findDevice(deviceSN)
		if err != nil {
			return -1, false
		}
		channel = device.Channel
	}
	return channel, true
}

// stopExpiredStream stops a stream whose last consumer left one grace
// period ago. It serializes on the per-device lifecycle lock against
// concurrent starts/restarts, and resolves BOTH the session and its station
// from one locked snapshot so a session installed moments later cannot be
// stopped through a stale station pointer. idPtr is live: the HTTP serve
// loops update it on session replacement and the WS handler refreshes it on
// ping, so the ID guard tracks the current session instead of a stale one.
func (s *Server) stopExpiredStream(deviceSN string, channel int, idPtr *int64) {
	lock := streamOpLock(deviceSN)
	lock.Lock()
	defer lock.Unlock()

	strm := stream.GetStream(deviceSN, channel)
	if strm == nil || strm.ID != *idPtr {
		return
	}
	station := devices.GetStation(strm.Station)
	if stream.StopStreamIfID(deviceSN, channel, strm.ID) {
		log.Printf("Stream: grace period expired for %s ch=%d, stopping livestream (streamID=%d)", deviceSN, channel, strm.ID)
		if station != nil {
			_ = station.StopLivestream(deviceSN, channel)
		}
	}
}

// All stream lifecycle operations for a device (start/stop/restart) serialize
// on a per-device mutex so concurrent player recoveries, monitor restarts and
// API stops cannot tear down each other's in-flight P2P connections.
var streamOps sync.Map // deviceSN -> *sync.Mutex

func streamOpLock(deviceSN string) *sync.Mutex {
	m, _ := streamOps.LoadOrStore(deviceSN, &sync.Mutex{})
	return m.(*sync.Mutex)
}

const streamGracePeriod = 30 * time.Second

// streamWatchers tracks EVERY consumer of a device channel across all
// endpoints (TS HTTP, raw HTTP, WebSocket heartbeat, stream port): the
// stream is stopped only when the last consumer anywhere is gone for the
// grace period. Splitting this count per endpoint caused a browser tab
// close (heartbeat count → 0) to kill a stream an NVR client was still
// pulling on another port.
var streamWatchers = newWatcherSet(streamGracePeriod)

type Server struct {
	apiClient *api.Client
	config    *config.Config
	staticFS  embed.FS
	upgrader  websocket.Upgrader

	mu             sync.RWMutex
	loggedIn       bool
	captcha        *api.CaptchaChallenge
	onLoginSuccess func()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onLoginSuccess = fn
}

func (s *Server) onLoginSuccessAsync() {
	s.mu.RLock()
	fn := s.onLoginSuccess
	s.mu.RUnlock()
	if fn != nil {
		go fn()
	}
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
	r.HandleFunc("/api/stream/stop/{deviceSN}", s.StopStreamHandler).Methods("POST")
	r.HandleFunc("/api/stream/{deviceSN}", s.StreamHandler).Methods("GET")
	r.HandleFunc("/api/raw/{deviceSN}", s.RawStreamHandler).Methods("GET")
	r.HandleFunc("/api/stream/ws/{deviceSN}", s.StreamWebSocketHandler)
	r.HandleFunc("/stream/{deviceSN}", s.StreamPageHandler).Methods("GET")
	sub, _ := fs.Sub(s.staticFS, "static")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
}

// RegisterStreamRoutes mounts the camera streaming endpoint on the optional
// unauthenticated stream port. Consumers use one URL shape everywhere —
// http://<host>:<port>/api/stream/<deviceSN> — and pick the port for its
// auth posture (main port: digest auth if enabled; stream port: none).
//
// ISOLATION RULE: this port is unauthenticated, so it must expose NOTHING
// but camera streaming. No API discovery, no camera/station lists, no login,
// no static files — anything mounted here is reachable without credentials.
// Anything else belongs on the main server's router (RegisterRoutes) only.
func (s *Server) RegisterStreamRoutes(r *mux.Router) {
	r.HandleFunc("/api/stream/{deviceSN}", s.StreamHandler).Methods("GET")
}


// ptsRemapper maps session PTS onto a connection-relative timeline that
// starts near zero — MSE only decodes when currentTime is inside the
// buffered range, and session PTS grows for minutes. It must survive a
// mid-connection session REPLACEMENT: a replacement session's PTS restarts
// near 1000 while this connection's base may be minutes in, so the naive
// difference underflows uint64 and the muxer emits garbage timestamps. On
// Reset (called by the serve loop when it re-binds) the base re-arms, and
// every output is clamped monotonically so consumers never see timestamps
// regress across the replacement boundary.
type ptsRemapper struct {
	base     uint64
	haveBase bool
	last     uint64
	haveLast bool
}

func (p *ptsRemapper) Remap(pts uint64) uint64 {
	if !p.haveBase {
		p.haveBase = true
		p.base = pts
	}
	if pts < p.base {
		pts = p.base // defensive: never let the difference underflow
	}
	out := pts - p.base + 1000
	if p.haveLast && out <= p.last {
		out = p.last + 1
	}
	p.last = out
	p.haveLast = true
	return out
}

// Reset re-arms the base for a replacement session. The monotonic clamp
// state is deliberately kept.
func (p *ptsRemapper) Reset() {
	p.haveBase = false
}

// serveStreamFrames polls the session for new frames and writes to the
// response as MPEG-TS until the client disconnects or the stream goes away.
// If the session object is replaced mid-connection (player-driven restart),
// the loop re-binds to the new session instead of serving a dead one:
// streamIDPtr is updated so the caller's grace-expire guard tracks the live
// session, and the PTS remapper re-arms for the replacement's restarted
// clock.
func (s *Server) serveStreamFrames(w http.ResponseWriter, r *http.Request, deviceSN string, strm *stream.StreamSession, streamIDPtr *int64, label string) {
	streamID := strm.ID
	*streamIDPtr = streamID

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	remapper := &ptsRemapper{}
	remapPTS := remapper.Remap

	var muxer *mpegts.Muxer
	defer func() {
		if muxer != nil {
			muxer.Close()
		}
	}()

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	frameCount := 0
	lastFrameID := 0

	var lastRestartCheck time.Time
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cur := stream.GetStream(deviceSN, strm.Channel)
			if cur == nil {
				return
			}
			if cur.ID != streamID {
				debuglog.Debugf("%s: session replaced for %s (id %d -> %d), re-binding",
					label, deviceSN, streamID, cur.ID)
				strm = cur
				streamID = cur.ID
				*streamIDPtr = streamID
				lastFrameID = 0
				// The replacement session's PTS clock restarts; re-arm the
				// base so the remapped timeline stays sane (the clamp keeps
				// it monotonic for the consumer).
				remapper.Reset()
			}
			if time.Since(lastRestartCheck) > 2*time.Second && strm.NeedsRestart() {
				lastRestartCheck = time.Now()
				go s.restartStaleStream(deviceSN, strm.Channel)
			}
			frames, nextID := strm.GetFramesSince(lastFrameID)
			lastFrameID = nextID
			if frameCount == 0 && len(frames) == 0 && muxer == nil {
				debuglog.Debugf("%s: %s ch=%d no frames yet (buffered=%d lastKF=%d awaitKF=%v)",
					label, deviceSN, strm.Channel, strm.FrameCount(), strm.LastKeyFrameIdx(), strm.AwaitKeyframe())
			}
			if len(frames) > 0 {
				for _, frame := range frames {
					if muxer == nil {
						codec := mpegts.CodecH264
						if strm.CurrentCodec() == 2 {
							codec = mpegts.CodecH265
						}
						muxer = mpegts.NewMuxer(w, codec)
					}
					if err := muxer.WriteFrame(frame.Data, remapPTS(frame.PTS)); err != nil {
						return
					}
					frameCount++
				}
				if canFlush {
					flusher.Flush()
				}
			}
		case <-r.Context().Done():
			log.Printf("%s: client disconnected from %s after %d frames", label, deviceSN, frameCount)
			return
		}
	}
}

// RawStreamHandler serves the session's Annex-B access units with a simple
// [u32 LE length][payload] framing instead of MPEG-TS. This is the feed for
// the H.265 camera player, which decodes HEVC in the browser via WASM
// (hevc.js) and needs the raw AUs, not a TS container.
func (s *Server) RawStreamHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	channel, ok := resolveChannel(r, s, deviceSN)
	if !ok {
		http.Error(w, "device not found: "+deviceSN, http.StatusNotFound)
		return
	}

	// Join the consumer set BEFORE starting (cancels any pending grace
	// timer from a previous consumer first). streamID is shared with the
	// serve loop; it stays 0 if the start fails, making the expire a no-op.
	streamID := int64(0)
	streamWatchers.add(deviceSN, channel)
	defer streamWatchers.remove(deviceSN, channel, func() {
		s.stopExpiredStream(deviceSN, channel, &streamID)
	})

	if err := s.startStreamForDevice(deviceSN, channel); err != nil {
		switch {
		case errors.Is(err, errDeviceNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errStationOffline), errors.Is(err, errStationNotFound):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, "failed to start livestream", http.StatusInternalServerError)
		}
		return
	}

	strm := stream.GetStream(deviceSN, channel)
	if strm == nil {
		http.Error(w, fmt.Sprintf("stream not found for device %s", deviceSN), http.StatusNotFound)
		return
	}
	streamID = strm.ID

	s.serveRawFrames(w, r, deviceSN, strm, &streamID)
}

// serveRawFrames polls the session and writes framed Annex-B access units:
// [u32 LE length][payload]. Payload is the same AU data the MPEG-TS path
// would mux (Annex-B NALs, keyframe-aligned start).
// serveRawFrames polls the session and writes framed Annex-B access units:
// [u32 LE length][payload]. Payload is the same AU data the MPEG-TS path
// would mux (Annex-B NALs, keyframe-aligned start). streamIDPtr is updated
// on mid-connection session replacement so the caller's grace-expire guard
// tracks the live session.
func (s *Server) serveRawFrames(w http.ResponseWriter, r *http.Request, deviceSN string, strm *stream.StreamSession, streamIDPtr *int64) {
	streamID := strm.ID
	*streamIDPtr = streamID

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	frameCount := 0
	lastFrameID := 0

	var lastRestartCheck time.Time
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	writeFrame := func(data []byte) error {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(data)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	}

	for {
		select {
		case <-ticker.C:
			cur := stream.GetStream(deviceSN, strm.Channel)
			if cur == nil {
				return
			}
			if cur.ID != streamID {
				strm = cur
				streamID = cur.ID
				*streamIDPtr = streamID
				lastFrameID = 0
			}
			if time.Since(lastRestartCheck) > 2*time.Second && strm.NeedsRestart() {
				lastRestartCheck = time.Now()
				go s.restartStaleStream(deviceSN, strm.Channel)
			}
			frames, nextID := strm.GetFramesSince(lastFrameID)
			lastFrameID = nextID
			for _, frame := range frames {
				if err := writeFrame(frame.Data); err != nil {
					return
				}
				frameCount++
			}
			if len(frames) > 0 && canFlush {
				flusher.Flush()
			}
		case <-r.Context().Done():
			log.Printf("RawStream: client disconnected from %s after %d frames", deviceSN, frameCount)
			return
		}
	}
}

func (s *Server) startStreamForDevice(deviceSN string, channel int) error {
	lock := streamOpLock(deviceSN)
	lock.Lock()
	defer lock.Unlock()
	return s.ensureStreamLocked(deviceSN, channel)
}

// ensureStreamLocked starts (or reuses) the livestream for a device.
// Callers MUST hold the per-device streamOpLock.
func (s *Server) ensureStreamLocked(deviceSN string, channel int) error {
	device, err := s.findDevice(deviceSN)
	if err != nil {
		return fmt.Errorf("%w: %s", errDeviceNotFound, deviceSN)
	}

	if channel < 0 {
		channel = device.Channel
	}

	station := devices.GetStation(device.StationSN)
	if station == nil {
		return fmt.Errorf("%w: %s", errStationNotFound, device.StationSN)
	}

	if !station.IsConnected() {
		return fmt.Errorf("%w: %s", errStationOffline, device.StationSN)
	}

	// If this device's stream is already active and not stale, reuse it.
	if strm := stream.GetStream(deviceSN, channel); strm != nil && !strm.IsStale() {
		return nil
	}

	s.stopLivestream(deviceSN, channel)

	stream.StartStream(deviceSN, station.GetSerial(), channel, int(p2p.VideoCodecH264))

	// H264 is requested from every camera family, but the pan-tilt family
	// (SoloCam S340/E42) streams H.265 regardless of the requested
	// streamtype. The session detects the real codec from the frame
	// headers, and the player picks the WASM decode path via the h265 flag.
	if err := station.StartLivestream(deviceSN, channel, device.DeviceType, int(p2p.VideoCodecH264), s.onVideoFrame); err != nil {
		_ = stream.StopStream(deviceSN, channel)
		return fmt.Errorf("%w: %v", errLivestreamStart, err)
	}

	// StartLivestream blocks through P2P connect + camera warm-up; reset the
	// staleness clock now so the monitor does not kill the fresh stream.
	if strm := stream.GetStream(deviceSN, channel); strm != nil {
		strm.ResetLastUpdate()
	}

	log.Printf("Started livestream for %s on station %s (channel=%d)", deviceSN, device.StationSN, channel)
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
		"logged_in":          loggedIn,
		"captcha_needed":     hasCaptcha && !loggedIn,
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

	s.onLoginSuccessAsync()

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
		"available":      true,
		"item":           captcha.Item,
		"captcha_base64": captcha.CaptchaBase64,
		"captcha_img":    captcha.CaptchaImg,
		"captcha_url":    captcha.CaptchaURL,
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

	s.onLoginSuccessAsync()

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
	if channel < 0 {
		if device, err := s.findDevice(deviceSN); err == nil {
			channel = device.Channel
		}
	}

	lock := streamOpLock(deviceSN)
	lock.Lock()
	err := s.ensureStreamLocked(deviceSN, channel)
	reused := err == nil && stream.GetStream(deviceSN, channel) != nil
	lock.Unlock()

	if err != nil {
		switch {
		case errors.Is(err, errDeviceNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errStationOffline), errors.Is(err, errStationNotFound):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			debuglog.Debugf("failed to start livestream: %v", err)
			http.Error(w, "failed to start livestream", http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if reused {
		debuglog.Debugf("Stream already active for %s ch=%d, reusing existing session (id=%d)", deviceSN, channel, stream.GetStream(deviceSN, channel).ID)
		writeJSON(w, map[string]string{"status": "active", "device_sn": deviceSN})
		return
	}
	writeJSON(w, map[string]string{"status": "started", "device_sn": deviceSN})
}

// StopStreamHandler stops a device livestream and removes its session.
// Called by CLI clients on clean shutdown so the stream does not linger for
// the WebSocket grace period.
func (s *Server) StopStreamHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	w.Header().Set("Content-Type", "application/json")

	channelStr := r.URL.Query().Get("channel")
	channel := -1
	if channelStr != "" {
		if ch, err := strconv.Atoi(channelStr); err == nil {
			channel = ch
		}
	}

	lock := streamOpLock(deviceSN)
	lock.Lock()
	defer lock.Unlock()

	// No channel param: stop every active stream of this device (legacy
	// CLI behavior).
	if channels := stream.StreamChannelsForDevice(deviceSN); len(channels) == 0 {
		writeJSON(w, map[string]string{"status": "not_active", "device_sn": deviceSN})
		return
	}

	stopped := 0
	for _, ch := range stream.StreamChannelsForDevice(deviceSN) {
		if channel >= 0 && ch != channel {
			continue
		}
		s.stopLivestream(deviceSN, ch)
		stopped++
	}

	log.Printf("Stopped %d livestream(s) for %s via API", stopped, deviceSN)
	writeJSON(w, map[string]string{"status": "stopped", "device_sn": deviceSN})
}

func (s *Server) StreamHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceSN := vars["deviceSN"]

	if deviceSN == "" {
		http.Error(w, "deviceSN required", http.StatusBadRequest)
		return
	}

	channel, ok := resolveChannel(r, s, deviceSN)
	if !ok {
		http.Error(w, "device not found: "+deviceSN, http.StatusNotFound)
		return
	}

	// Join the consumer set BEFORE starting: a pending grace timer from a
	// previous consumer must be cancelled before the (possibly slow, up to
	// ~60s camera wake) start runs. streamID is shared with the serve loop
	// so the expire guard tracks the live session across replacements; it
	// stays 0 if the start fails, making the expire a no-op.
	streamID := int64(0)
	count := streamWatchers.add(deviceSN, channel)
	log.Printf("Stream: client connected to %s ch=%d on %s (active: %d)", deviceSN, channel, r.Host, count)
	defer streamWatchers.remove(deviceSN, channel, func() {
		s.stopExpiredStream(deviceSN, channel, &streamID)
	})

	if strm := stream.GetStream(deviceSN, channel); strm == nil || strm.IsStale() {
		if err := s.startStreamForDevice(deviceSN, channel); err != nil {
			switch {
			case errors.Is(err, errDeviceNotFound):
				http.Error(w, err.Error(), http.StatusNotFound)
			case errors.Is(err, errStationOffline), errors.Is(err, errStationNotFound):
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			default:
				http.Error(w, "failed to start livestream", http.StatusInternalServerError)
			}
			return
		}
	}
	strm := stream.GetStream(deviceSN, channel)
	if strm == nil {
		http.Error(w, "failed to start stream", http.StatusInternalServerError)
		return
	}
	streamID = strm.ID

	s.serveStreamFrames(w, r, deviceSN, strm, &streamID, "StreamHandler")
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

	// Deterministic watcher key: the live session's channel if one exists,
	// else the device's own channel from the cloud device list (the channel
	// a subsequent auto-start will use). streamID stays 0 when no session
	// exists, making the expire a no-op.
	watchChannel := -1
	if ch, err := strconv.Atoi(r.URL.Query().Get("channel")); err == nil {
		watchChannel = ch
	}
	channel := watchChannel
	streamID := int64(0)
	if strm := stream.GetStream(deviceSN, watchChannel); strm != nil {
		channel = strm.Channel
		streamID = strm.ID
	} else if dev, err := s.findDevice(deviceSN); err == nil {
		channel = dev.Channel
	} else {
		channel = -1
	}

	count := streamWatchers.add(deviceSN, channel)
	debuglog.Debugf("StreamWebSocket: watcher connected for %s ch=%d (total watchers: %d, streamID=%d)", deviceSN, channel, count, streamID)
	defer streamWatchers.remove(deviceSN, channel, func() {
		s.stopExpiredStream(deviceSN, channel, &streamID)
	})

	// Pings refresh the read deadline AND the bound session: if the stream
	// is replaced while this heartbeat lives, the expire guard must track
	// the live session ID (residual staleness <= one ping interval, 3s).
	conn.SetPingHandler(func(appData string) error {
		if cur := stream.GetStream(deviceSN, channel); cur != nil {
			streamID = cur.ID
		}
		_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, nil)
	})

	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
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

func (s *Server) onVideoFrame(deviceSN string, channel byte, frameData []byte, metadata p2p.VideoFrameMetadata) error {
	session := stream.GetStream(deviceSN, int(channel))
	if session == nil {
		debuglog.Debugf("onVideoFrame: no stream session for %s ch=%d", deviceSN, channel)
		return fmt.Errorf("stream session not found")
	}

	session.AppendFrame(frameData, metadata)
	if session.FrameCount() <= 5 {
		debuglog.Debugf("onVideoFrame: %s frame #%d buffered, %d bytes, keyFrame=%v", deviceSN, session.FrameCount(), len(frameData), metadata.IsKeyFrame)
	}
	return nil
}

// restartStaleStream re-establishes the P2P livestream for a device when the
// upstream feed is stale (no frames) or persistently corrupt. It performs a
// full teardown and reconnect: StopLivestream closes the dedicated P2P
// connection (also recovering from a dead/half-open UDP session) and
// StartLivestream builds a fresh one. The per-device streamOpLock serializes
// this against starts/stops from players, the CLI and the monitor.
func (s *Server) restartStaleStream(deviceSN string, channel int) {
	lock := streamOpLock(deviceSN)
	lock.Lock()
	defer lock.Unlock()

	strm := stream.GetStream(deviceSN, channel)
	if strm == nil {
		return
	}

	// Re-check under the lock: another goroutine may have just restarted.
	if !strm.NeedsRestart() {
		return
	}

	// Cooldown: avoid stacking reconnect attempts when the station is
	// unreachable (each attempt can block for the full connect timeout).
	if !strm.ShouldAttemptRestart() {
		return
	}
	strm.MarkRestartAttempt()

	station := devices.GetStation(strm.Station)
	if station == nil {
		return
	}

	debuglog.Debugf("Restarting livestream for %s (stale=%v badFrames=%d)", deviceSN, strm.IsStale(), strm.BadFrameCount())

	// Full teardown so a dead P2P session is replaced, not reused.
	_ = station.StopLivestream(deviceSN, strm.Channel)
	strm.ResetFailureTracking()
	strm.ClearBuffer()

	deviceType := 0
	if device, err := s.findDevice(deviceSN); err == nil {
		deviceType = device.DeviceType
	}
	if err := station.StartLivestream(deviceSN, strm.Channel, deviceType, int(p2p.VideoCodecH264), s.onVideoFrame); err != nil {
		// Leave LastUpdate in the past so the next check retries.
		debuglog.Debugf("Failed to restart livestream for %s: %v (will retry)", deviceSN, err)
		strm.MarkRestartFailed()
		return
	}

	// Reset staleness ONLY after the reconnect finished: StartLivestream can
	// block for the whole connect timeout, and battery cameras need several
	// more seconds of warm-up before the first frame arrives. Resetting
	// earlier makes the monitor kill the stream mid-warm-up in a loop.
	strm.ResetLastUpdate()
	debuglog.Debugf("Restarted livestream for %s, waiting for camera warm-up", deviceSN)
}

func (s *Server) stopLivestream(deviceSN string, channel int) {
	strm := stream.GetStream(deviceSN, channel)
	if strm == nil {
		debuglog.Debugf("stopLivestream: no active stream for %s ch=%d", deviceSN, channel)
		return
	}
	station := devices.GetStation(strm.Station)
	if station != nil {
		_ = station.StopLivestream(deviceSN, channel)
	}
	_ = stream.StopStream(deviceSN, channel)
	debuglog.Debugf("Stopped livestream for device %s ch=%d (station=%s)", deviceSN, channel, strm.Station)
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
			DeviceType: device.DeviceType,
			H265:       devices.IsH265Camera(device.DeviceType),
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
	DeviceType int    `json:"device_type"`
	H265       bool   `json:"h265"`
	Online     bool   `json:"online"`
	Streaming  bool   `json:"streaming"`
}

type StationInfo struct {
	StationSN string `json:"station_sn"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Connected bool   `json:"connected"`
}
