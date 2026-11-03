package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The mock below replicates the REAL server semantics from pkg/web/handler.go:
//   - StreamWebSocketHandler: 8s read deadline, refreshed on every message
//   - StreamHandler: polled 50ms loop; response ends when the session is gone
//   - start/stop endpoints with real status codes

type mockServer struct {
	srv *httptest.Server

	mu           sync.Mutex
	streamActive bool
	streamEnds   chan struct{} // closes the current stream response when closed
	startCalls   atomic.Int64
	stopCalls    atomic.Int64
	gets         atomic.Int64

	wsReads    atomic.Int64
	wsDeadline atomic.Int64 // count of deadline expiries observed server-side
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	m := &mockServer{streamActive: true, streamEnds: make(chan struct{})}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/stream/start/", func(w http.ResponseWriter, r *http.Request) {
		m.startCalls.Add(1)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.streamActive {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"status":"active"}`)
			return
		}
		m.streamActive = true
		m.streamEnds = make(chan struct{})
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"started"}`)
	})

	mux.HandleFunc("/api/stream/stop/", func(w http.ResponseWriter, r *http.Request) {
		m.stopCalls.Add(1)
		m.mu.Lock()
		m.streamActive = false
		close(m.streamEnds)
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"stopped"}`)
	})

	// Replicates StreamHandler: 50ms poll loop, response ends when the
	// session is removed (stop endpoint or grace-period kill).
	mux.HandleFunc("/api/stream/", func(w http.ResponseWriter, r *http.Request) {
		m.gets.Add(1)
		m.mu.Lock()
		active := m.streamActive
		ends := m.streamEnds
		m.mu.Unlock()
		if !active {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "stream not found for device")
			return
		}

		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// MPEG-TS packets: 188 bytes starting with sync byte 0x47
				pkt := make([]byte, 188)
				pkt[0] = 0x47
				for i := range pkt {
					pkt[i] = byte(i)
				}
				pkt[0] = 0x47
				if _, err := w.Write(pkt); err != nil {
					return
				}
				flusher.Flush()
			case <-ends:
				return // session removed server-side: response ends
			case <-r.Context().Done():
				return
			}
		}
	})

	// Replicates StreamWebSocketHandler: 8s read deadline refreshed on every
	// received message.
	mux.HandleFunc("/api/stream/ws/", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure) {
					m.wsDeadline.Add(1)
				}
				return
			}
			m.wsReads.Add(1)
			_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		}
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// syncBuffer is a mutex-guarded buffer so tests can inspect output while
// pumpStream is still writing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Bytes()
}

func (s *syncBuffer) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func assertTSSynced(t *testing.T, b []byte) {
	t.Helper()
	for i := 0; i+188 <= len(b); i += 188 {
		if b[i] != 0x47 {
			t.Fatalf("output corrupted at byte %d: sync byte 0x47 expected, got 0x%02x (error body leaked into stream?)", i, b[i])
		}
	}
}

// TestPumpStreamReconnectsAcrossSeveredResponse validates that when the
// server ends the stream response mid-flight (P2P restart / grace-period
// kill), the CLI reconnects and keeps the output continuous.
func TestPumpStreamReconnectsAcrossSeveredResponse(t *testing.T) {
	m := newMockServer(t)
	done := make(chan struct{})
	out := &syncBuffer{}

	go func() {
		_ = pumpStream(http.DefaultClient, m.srv.URL, "DEV", 0, out, done)
	}()

	// Let it connect and stream briefly, then sever the response server-side
	// (as the real server does when the P2P feed restarts).
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	close(m.streamEnds)
	m.streamEnds = make(chan struct{})
	m.mu.Unlock()

	time.Sleep(500 * time.Millisecond)
	close(done)
	time.Sleep(100 * time.Millisecond)

	if gets := m.gets.Load(); gets < 2 {
		t.Fatalf("pumpStream did not reconnect after response severed: %d GETs", gets)
	}
	b := out.bytes()
	if len(b) < 188*2 {
		t.Fatalf("expected sustained output, got %d bytes", len(b))
	}
	assertTSSynced(t, b)
}

// TestPumpStream404RestartsStream validates the 404 → POST start → reconnect
// recovery path against the real endpoint semantics.
func TestPumpStream404RestartsStream(t *testing.T) {
	m := newMockServer(t)

	// Stop the stream so GET returns 404 (session gone server-side).
	resp, _ := http.Post(m.srv.URL+"/api/stream/stop/DEV", "", nil)
	_ = resp.Body.Close()

	done := make(chan struct{})
	out := &syncBuffer{}
	go func() {
		_ = pumpStream(http.DefaultClient, m.srv.URL, "DEV", 0, out, done)
	}()

	time.Sleep(2 * time.Second)
	close(done)
	time.Sleep(100 * time.Millisecond)

	if m.startCalls.Load() < 1 {
		t.Fatal("404 did not trigger a stream restart POST")
	}
	if m.gets.Load() < 2 {
		t.Fatalf("expected retry after 404 restart, got %d GETs", m.gets.Load())
	}
	if out.len() == 0 {
		t.Fatal("stream did not recover after 404 restart")
	}
	assertTSSynced(t, out.bytes())
}

// TestPumpStreamNeverWritesErrorBodies validates HTTP error bodies are kept
// out of the output stream.
func TestPumpStreamNeverWritesErrorBodies(t *testing.T) {
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stream/DEV" {
			if gets.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, "Internal Server Error: panic recovered")
				return
			}
			w.Header().Set("Content-Type", "video/mp2t")
			w.WriteHeader(http.StatusOK)
			pkt := bytes.Repeat([]byte{0x47}, 188)
			_, _ = w.Write(pkt)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"started"}`)
	}))
	defer srv.Close()

	done := make(chan struct{})
	out := &syncBuffer{}
	go func() {
		_ = pumpStream(http.DefaultClient, srv.URL, "DEV", 0, out, done)
	}()

	time.Sleep(1500 * time.Millisecond)
	close(done)
	time.Sleep(100 * time.Millisecond)

	if gets.Load() < 2 {
		t.Fatalf("expected retry after 500, got %d GETs", gets.Load())
	}
	if bytes.Contains(out.bytes(), []byte("Internal Server")) {
		t.Fatal("HTTP error body leaked into stream output")
	}
	if out.len() == 0 {
		t.Fatal("stream never recovered after initial 500")
	}
	assertTSSynced(t, out.bytes())
}

// TestStartStreamValidatesStatus validates that start failures surface as
// errors instead of being silently ignored.
func TestStartStreamValidatesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "station not connected", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := startStream(http.DefaultClient, srv.URL, "DEV", 0)
	if err == nil {
		t.Fatal("startStream must fail on HTTP 503")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("503")) {
		t.Fatalf("error must include status code: %v", err)
	}
}

// TestHeartbeatKeepsServerReadDeadlineAlive runs the heartbeat against a
// server enforcing the REAL 8-second read deadline for 10+ seconds: the 3s
// ping cadence must keep the watcher registered (a dead watcher gets the
// livestream stopped after the grace period on the real server).
func TestHeartbeatKeepsServerReadDeadlineAlive(t *testing.T) {
	m := newMockServer(t)

	hb, err := dialHeartbeat(m.srv.URL, "", "", "DEV")
	if err != nil {
		t.Fatalf("heartbeat dial failed: %v", err)
	}
	hb.start()

	// Longer than the 8s deadline: proves pings continuously refresh it.
	time.Sleep(10 * time.Second)

	if m.wsDeadline.Load() != 0 {
		t.Fatalf("server read deadline expired (watcher would be dropped): %d", m.wsDeadline.Load())
	}
	if m.wsReads.Load() < 3 {
		t.Fatalf("server received too few pings: %d", m.wsReads.Load())
	}

	// Concurrent close + ping must not panic (run with -race).
	hb.close()
	hb.close()
}

// TestHeartbeatCloseIsIdempotent validates double-close safety.
func TestHeartbeatCloseIsIdempotent(t *testing.T) {
	m := newMockServer(t)
	hb, err := dialHeartbeat(m.srv.URL, "", "", "DEV")
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	hb.start()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hb.close()
		}()
	}
	wg.Wait()
	if !hb.closed() {
		t.Fatal("heartbeat must be closed after close()")
	}
}
