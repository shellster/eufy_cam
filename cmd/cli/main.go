package main

import (
	"encoding/json"
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
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "eufy-cli [flags] <command> [args]",
		Short: "Eufy camera P2P streaming CLI",
		Example: `  eufy-cli list
  eufy-cli --user admin --pass secret list
  eufy-cli stream T8134P2024342790 | ffmpeg -i - -c copy out.ts
  eufy-cli --port 8090 stream T8134P2024342790
  eufy-cli --port 8090 --bind 0.0.0.0 stream T8134P2024342790`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().String("server", "http://localhost:8080", "Server URL")
	rootCmd.PersistentFlags().String("user", "", "Username for digest auth")
	rootCmd.PersistentFlags().String("pass", "", "Password for digest auth")

	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(streamCmd())
	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available cameras",
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL, _ := cmd.Flags().GetString("server")
			user, _ := cmd.Flags().GetString("user")
			pass, _ := cmd.Flags().GetString("pass")
			return doList(serverURL, user, pass)
		},
	}
}

func streamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stream <deviceSN>",
		Short: "Stream camera video to stdout or HTTP",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL, _ := cmd.Flags().GetString("server")
			user, _ := cmd.Flags().GetString("user")
			pass, _ := cmd.Flags().GetString("pass")
			port, _ := cmd.Flags().GetInt("port")
			bind, _ := cmd.Flags().GetString("bind")
			deviceSN := args[0]

			if port > 0 {
				return doStreamServe(serverURL, user, pass, deviceSN, port, bind)
			}
			return doStream(serverURL, user, pass, deviceSN)
		},
	}

	cmd.Flags().Int("port", 0, "Serve stream on this HTTP port instead of writing to stdout")
	cmd.Flags().String("bind", "localhost", "Bind address for HTTP serve mode (use 0.0.0.0 for all interfaces)")

	return cmd
}

func httpClient(user, pass string) *http.Client {
	if user != "" && pass != "" {
		return &http.Client{
			Transport: &digest.Transport{
				Username: user,
				Password: pass,
			},
		}
	}
	return http.DefaultClient
}

type camera struct {
	DeviceSN   string `json:"device_sn"`
	DeviceName string `json:"device_name"`
	StationSN  string `json:"station_sn"`
	Model      string `json:"model"`
	Channel    int    `json:"channel"`
}

func doList(serverURL, user, pass string) error {
	client := httpClient(user, pass)
	resp, err := client.Get(serverURL + "/api/cameras")
	if err != nil {
		return fmt.Errorf("error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, body)
	}

	var cameras []camera
	if err := json.NewDecoder(resp.Body).Decode(&cameras); err != nil {
		return fmt.Errorf("error parsing response: %v", err)
	}

	if len(cameras) == 0 {
		fmt.Println("No cameras found.")
		return nil
	}

	fmt.Printf("%-25s %-30s %-20s %s\n", "DEVICE SN", "NAME", "MODEL", "CH")
	fmt.Printf("%s %s %s %s\n", strings.Repeat("-", 25), strings.Repeat("-", 30), strings.Repeat("-", 20), strings.Repeat("-", 3))
	for _, c := range cameras {
		fmt.Printf("%-25s %-30s %-20s %d\n", c.DeviceSN, c.DeviceName, c.Model, c.Channel)
	}
	return nil
}

func lookupChannel(serverURL, user, pass, deviceSN string) (int, error) {
	client := httpClient(user, pass)
	resp, err := client.Get(serverURL + "/api/cameras")
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

func doStream(serverURL, user, pass, deviceSN string) error {
	channel, err := lookupChannel(serverURL, user, pass, deviceSN)
	if err != nil {
		return err
	}

	client := httpClient(user, pass)

	// Start stream
	startURL := fmt.Sprintf("%s/api/stream/start/%s?channel=%d", serverURL, deviceSN, channel)
	resp, err := client.Post(startURL, "", nil)
	if err != nil {
		return fmt.Errorf("error starting stream: %v", err)
	}
	_ = resp.Body.Close()

	if false {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to start stream (HTTP %d): %s", resp.StatusCode, body)
	}

	// Open WebSocket for heartbeat
	wsStr := wsURL(serverURL, fmt.Sprintf("/api/stream/ws/%s", deviceSN))
	wsConn, err := dialWebSocket(wsStr, user, pass)
	if err != nil {
		stopStream(serverURL, user, pass, deviceSN)
		return fmt.Errorf("error connecting WebSocket: %v", err)
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
		cleanup(deviceSN, wsConn, done, serverURL, user, pass)
		os.Exit(0)
	}()

	// Fetch MPEG-TS stream and write to stdout
	streamURL := fmt.Sprintf("%s/api/stream/%s", serverURL, deviceSN)
	streamResp, err := client.Get(streamURL)
	if err != nil {
		cleanup(deviceSN, wsConn, done, serverURL, user, pass)
		return fmt.Errorf("error fetching stream: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()

	_, _ = io.Copy(os.Stdout, streamResp.Body)

	cleanup(deviceSN, wsConn, done, serverURL, user, pass)
	return nil
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

func doStreamServe(serverURL, user, pass, deviceSN string, port int, bind string) error {
	channel, err := lookupChannel(serverURL, user, pass, deviceSN)
	if err != nil {
		return err
	}

	client := httpClient(user, pass)

	// Start stream
	startURL := fmt.Sprintf("%s/api/stream/start/%s?channel=%d", serverURL, deviceSN, channel)
	resp, err := client.Post(startURL, "", nil)
	if err != nil {
		return fmt.Errorf("error starting stream: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to start stream (HTTP %d)", resp.StatusCode)
	}

	// Open WebSocket for heartbeat
	wsStr := wsURL(serverURL, fmt.Sprintf("/api/stream/ws/%s", deviceSN))
	wsConn, err := dialWebSocket(wsStr, user, pass)
	if err != nil {
		stopStream(serverURL, user, pass, deviceSN)
		return fmt.Errorf("error connecting WebSocket: %v", err)
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
		cleanup(deviceSN, wsConn, done, serverURL, user, pass)
		os.Exit(0)
	}()

	// Fan-out writer for multiple clients
	fan := &fanWriter{}

	// Start fetching from server in background
	go func() {
		streamURL := fmt.Sprintf("%s/api/stream/%s", serverURL, deviceSN)
		startURL := fmt.Sprintf("%s/api/stream/start/%s?channel=%d", serverURL, deviceSN, channel)
		backoff := 2 * time.Second
		maxBackoff := 30 * time.Second

		for {
			streamResp, err := client.Get(streamURL)
			if err != nil {
				log.Printf("Error fetching stream: %v", err)
				select {
				case <-done:
					return
				case <-time.After(backoff):
					backoff = min(backoff*2, maxBackoff)
					continue
				}
			}

			if streamResp.StatusCode == http.StatusNotFound {
				_ = streamResp.Body.Close()
				log.Println("Stream not found on server, restarting...")
				startResp, err := client.Post(startURL, "", nil)
				if err != nil {
					log.Printf("Error restarting stream: %v", err)
				} else {
					_ = startResp.Body.Close()
				}
				select {
				case <-done:
					return
				case <-time.After(backoff):
					backoff = min(backoff*2, maxBackoff)
					continue
				}
			}

			n, _ := io.Copy(fan, streamResp.Body)
			_ = streamResp.Body.Close()

			select {
			case <-done:
				return
			default:
			}

			if n == 0 {
				log.Printf("Stream ended immediately (0 bytes), reconnecting in %v...", backoff)
				select {
				case <-done:
					return
				case <-time.After(backoff):
					backoff = min(backoff*2, maxBackoff)
				}
				continue
			}

			backoff = 2 * time.Second
			log.Println("Stream ended, reconnecting...")
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
		cleanup(deviceSN, wsConn, done, serverURL, user, pass)
		return fmt.Errorf("error listening on %s: %v", addr, err)
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
	return nil
}

func dialWebSocket(wsStr, user, pass string) (*websocket.Conn, error) {
	if user == "" || pass == "" {
		return wsDial(wsStr, nil)
	}

	// Make a preflight HTTP request to get the 401 digest challenge,
	// then compute the Authorization header for the WebSocket upgrade.
	httpURL := wsToHTTP(wsStr)
	resp, err := http.Get(httpURL)
	if err != nil {
		return nil, fmt.Errorf("digest preflight failed: %w", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		// No auth needed
		return wsDial(wsStr, nil)
	}

	chal, err := digest.FindChallenge(resp.Header)
	if err != nil {
		return nil, fmt.Errorf("digest challenge parse failed: %w", err)
	}

	cred, err := digest.Digest(chal, digest.Options{
		Method:   "GET",
		URI:      wsPath(wsStr),
		Username: user,
		Password: pass,
	})
	if err != nil {
		return nil, fmt.Errorf("digest computation failed: %w", err)
	}

	header := http.Header{}
	header.Set("Authorization", cred.String())

	return wsDial(wsStr, header)
}

func cleanup(deviceSN string, wsConn *websocket.Conn, done chan struct{}, serverURL, user, pass string) {
	close(done)
	_ = wsConn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	wsConn.Close() //nolint:errcheck
	stopStream(serverURL, user, pass, deviceSN)
}

func stopStream(serverURL, user, pass, deviceSN string) {
	client := httpClient(user, pass)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/stream/stop/%s", serverURL, deviceSN), nil)
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

func wsToHTTP(wsStr string) string {
	if strings.HasPrefix(wsStr, "wss://") {
		return "https://" + wsStr[6:]
	}
	return "http://" + strings.TrimPrefix(wsStr, "ws://")
}

func wsPath(wsStr string) string {
	u, err := url.Parse(wsStr)
	if err != nil {
		return "/"
	}
	return u.RequestURI()
}

func wsDial(wsStr string, header http.Header) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.Dial(wsStr, header)
	return conn, err
}
