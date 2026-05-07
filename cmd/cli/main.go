package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/icholy/digest"
)

var (
	serverURL = flag.String("server", "http://localhost:8080", "Server URL")
	authUser  = flag.String("user", "", "Username for digest auth")
	authPass  = flag.String("pass", "", "Password for digest auth")
	servePort = flag.Int("port", 0, "Serve stream on this HTTP port instead of writing to stdout")
	serveBind = flag.String("bind", "localhost", "Bind address for HTTP serve mode (use 0.0.0.0 for all interfaces)")
)

func httpClient() *http.Client {
	if *authUser != "" && *authPass != "" {
		return &http.Client{
			Transport: &digest.Transport{
				Username: *authUser,
				Password: *authPass,
			},
		}
	}
	return http.DefaultClient
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <command> [args]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  list              List available cameras\n")
		fmt.Fprintf(os.Stderr, "  stream <sn>       Stream camera video to stdout or HTTP\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s list\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -user admin -pass secret list\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s stream T8134P2024342790 | ffmpeg -i - -c copy out.ts\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -port 8090 stream T8134P2024342790\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -port 8090 -bind 0.0.0.0 stream T8134P2024342790\n", os.Args[0])
	}

	// Reorder args so flags and their values precede positional args.
	// Lets "eufy-cli stream SN --port 8090" work like "eufy-cli --port 8090 stream SN".
	args := os.Args[1:]
	var flagArgs, positionalArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			if !strings.Contains(args[i], "=") {
				name := strings.TrimLeft(args[i], "-")
				if f := flag.Lookup(name); f != nil && f.DefValue != "true" && f.DefValue != "false" && i+1 < len(args) {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
		} else {
			positionalArgs = append(positionalArgs, args[i])
		}
	}
	os.Args = append([]string{os.Args[0]}, append(flagArgs, positionalArgs...)...)

	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	cmd := flag.Arg(0)

	switch cmd {
	case "list":
		doList()
	case "stream":
		if flag.NArg() < 2 {
			fmt.Fprintf(os.Stderr, "Usage: %s stream <deviceSN>\n", os.Args[0])
			os.Exit(1)
		}
		deviceSN := flag.Arg(1)
		if *servePort > 0 {
			doStreamServe(deviceSN, *servePort, *serveBind)
		} else {
			doStream(deviceSN)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		flag.Usage()
		os.Exit(1)
	}
}

type camera struct {
	DeviceSN   string `json:"device_sn"`
	DeviceName string `json:"device_name"`
	StationSN  string `json:"station_sn"`
	Model      string `json:"model"`
	Channel    int    `json:"channel"`
}

func doList() {
	client := httpClient()
	resp, err := client.Get(*serverURL + "/api/cameras")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Server returned %d: %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	var cameras []camera
	if err := json.NewDecoder(resp.Body).Decode(&cameras); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}

	if len(cameras) == 0 {
		fmt.Println("No cameras found.")
		return
	}

	fmt.Printf("%-25s %-30s %-20s %s\n", "DEVICE SN", "NAME", "MODEL", "CH")
	fmt.Printf("%s %s %s %s\n", strings.Repeat("-", 25), strings.Repeat("-", 30), strings.Repeat("-", 20), strings.Repeat("-", 3))
	for _, c := range cameras {
		fmt.Printf("%-25s %-30s %-20s %d\n", c.DeviceSN, c.DeviceName, c.Model, c.Channel)
	}
}

func lookupChannel(deviceSN string) (int, error) {
	client := httpClient()
	resp, err := client.Get(*serverURL + "/api/cameras")
	if err != nil {
		return 0, fmt.Errorf("failed to fetch camera list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	var cameras []camera
	if err := json.NewDecoder(resp.Body).Decode(&cameras); err != nil {
		return 0, fmt.Errorf("failed to parse camera list: %v", err)
	}

	for _, c := range cameras {
		if c.DeviceSN == deviceSN {
			return c.Channel, nil
		}
	}

	return 0, fmt.Errorf("camera %s not found", deviceSN)
}

func doStream(deviceSN string) {
	channel, err := lookupChannel(deviceSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	client := httpClient()

	// Start stream
	startURL := fmt.Sprintf("%s/api/stream/start/%s?channel=%d", *serverURL, deviceSN, channel)
	resp, err := client.Post(startURL, "", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting stream: %v\n", err)
		os.Exit(1)
	}
	_ = resp.Body.Close()

	if false {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Failed to start stream (HTTP %d): %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	// Open WebSocket for heartbeat
	wsURL := wsURL(*serverURL, fmt.Sprintf("/api/stream/ws/%s", deviceSN))
	wsConn, err := dialWebSocket(wsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting WebSocket: %v\n", err)
		stopStream(deviceSN)
		os.Exit(1)
	}

	// Heartbeat goroutine
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := wsConn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Handle cleanup on signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cleanup(deviceSN, wsConn, done)
		os.Exit(0)
	}()

	// Fetch MPEG-TS stream and write to stdout
	streamURL := fmt.Sprintf("%s/api/stream/%s", *serverURL, deviceSN)
	streamResp, err := client.Get(streamURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching stream: %v\n", err)
		cleanup(deviceSN, wsConn, done)
		os.Exit(1)
	}
	defer func() { _ = streamResp.Body.Close() }()

	_, _ = io.Copy(os.Stdout, streamResp.Body)

	cleanup(deviceSN, wsConn, done)
}

// fanWriter fans out data from a single reader to multiple HTTP clients.
type fanWriter struct {
	mu      sync.Mutex
	writers []io.Writer
}

func (f *fanWriter) addWriter(w io.Writer) {
	f.mu.Lock()
	f.writers = append(f.writers, w)
	f.mu.Unlock()
}

func (f *fanWriter) removeWriter(w io.Writer) {
	f.mu.Lock()
	for i, ww := range f.writers {
		if ww == w {
			f.writers = append(f.writers[:i], f.writers[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
}

func (f *fanWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var broken []int
	for i, w := range f.writers {
		if _, err := w.Write(p); err != nil {
			broken = append(broken, i)
		}
	}
	// Remove broken writers in reverse order
	for i := len(broken) - 1; i >= 0; i-- {
		idx := broken[i]
		f.writers = append(f.writers[:idx], f.writers[idx+1:]...)
	}
	return len(p), nil
}

func (f *fanWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writers)
}

func doStreamServe(deviceSN string, port int, bind string) {
	channel, err := lookupChannel(deviceSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	client := httpClient()

	// Start stream
	startURL := fmt.Sprintf("%s/api/stream/start/%s?channel=%d", *serverURL, deviceSN, channel)
	resp, err := client.Post(startURL, "", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting stream: %v\n", err)
		os.Exit(1)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Failed to start stream (HTTP %d)\n", resp.StatusCode)
		os.Exit(1)
	}

	// Open WebSocket for heartbeat
	wsURL := wsURL(*serverURL, fmt.Sprintf("/api/stream/ws/%s", deviceSN))
	wsConn, err := dialWebSocket(wsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting WebSocket: %v\n", err)
		stopStream(deviceSN)
		os.Exit(1)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := wsConn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cleanup(deviceSN, wsConn, done)
		os.Exit(0)
	}()

	// Fan-out writer for multiple clients
	fan := &fanWriter{}

	// Start fetching from server in background
	go func() {
		streamURL := fmt.Sprintf("%s/api/stream/%s", *serverURL, deviceSN)
		for {
			streamResp, err := client.Get(streamURL)
			if err != nil {
				log.Printf("Error fetching stream: %v", err)
				select {
				case <-done:
					return
				case <-time.After(2 * time.Second):
					continue
				}
			}
			_, _ = io.Copy(fan, streamResp.Body)
			_ = streamResp.Body.Close()
			select {
			case <-done:
				return
			default:
				log.Println("Stream ended, reconnecting...")
			}
		}
	}()

	// HTTP server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Client connected: %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, canFlush := w.(http.Flusher)
		pipeR, pipeW := io.Pipe()
		fan.addWriter(pipeW)
		defer fan.removeWriter(pipeW)
		defer pipeW.Close()

		buf := make([]byte, 32*1024)
		for {
			n, err := pipeR.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		log.Printf("Client disconnected: %s", r.RemoteAddr)
	})

	addr := fmt.Sprintf("%s:%d", bind, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listening on %s: %v\n", addr, err)
		cleanup(deviceSN, wsConn, done)
		os.Exit(1)
	}

	log.Printf("Serving MPEG-TS stream for %s on http://%s/", deviceSN, addr)
	log.Printf("Multiple clients can connect simultaneously (active: %d)", fan.count())

	// Periodically log client count
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n := fan.count(); n > 0 {
					log.Printf("Active clients: %d", n)
				}
			case <-done:
				return
			}
		}
	}()

	srv := &http.Server{Handler: handler}
	_ = srv.Serve(ln)
}

func dialWebSocket(wsURL string) (*websocket.Conn, error) {
	if *authUser == "" || *authPass == "" {
		return wsDial(wsURL, nil)
	}

	// Make a preflight HTTP request to get the 401 digest challenge,
	// then compute the Authorization header for the WebSocket upgrade.
	httpURL := wsToHTTP(wsURL)
	resp, err := http.Get(httpURL)
	if err != nil {
		return nil, fmt.Errorf("digest preflight failed: %w", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		// No auth needed
		return wsDial(wsURL, nil)
	}

	chal, err := digest.FindChallenge(resp.Header)
	if err != nil {
		return nil, fmt.Errorf("digest challenge parse failed: %w", err)
	}

	cred, err := digest.Digest(chal, digest.Options{
		Method:   "GET",
		URI:      wsPath(wsURL),
		Username: *authUser,
		Password: *authPass,
	})
	if err != nil {
		return nil, fmt.Errorf("digest computation failed: %w", err)
	}

	header := http.Header{}
	header.Set("Authorization", cred.String())

	return wsDial(wsURL, header)
}

func cleanup(deviceSN string, wsConn *websocket.Conn, done chan struct{}) {
	close(done)
	_ = wsConn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	wsConn.Close() //nolint:errcheck
	stopStream(deviceSN)
}

func stopStream(deviceSN string) {
	client := httpClient()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/stream/stop/%s", *serverURL, deviceSN), nil)
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func wsURL(server, path string) string {
	u, _ := url.Parse(server)
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + u.Host + path
}

func wsToHTTP(wsURL string) string {
	if strings.HasPrefix(wsURL, "wss://") {
		return "https://" + wsURL[6:]
	}
	return "http://" + strings.TrimPrefix(wsURL, "ws://")
}

func wsPath(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "/"
	}
	return u.RequestURI()
}

func wsDial(wsURL string, header http.Header) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	return conn, err
}
