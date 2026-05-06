package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/icholy/digest"
)

var (
	serverURL = flag.String("server", "http://localhost:8080", "Server URL")
	authUser  = flag.String("user", "", "Username for digest auth")
	authPass  = flag.String("pass", "", "Password for digest auth")
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
		fmt.Fprintf(os.Stderr, "  stream <sn> [ch]  Stream camera video to stdout\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s list\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -user admin -pass secret list\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s stream T8134P2024342790 | ffmpeg -i - -c copy out.ts\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s stream T8134P2024342790 | ffmpeg -i - -f v4l2 /dev/video0\n", os.Args[0])
	}

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
			fmt.Fprintf(os.Stderr, "Usage: %s stream <deviceSN> [channel]\n", os.Args[0])
			os.Exit(1)
		}
		deviceSN := flag.Arg(1)
		channel := 0
		if flag.NArg() >= 3 {
			if _, err := fmt.Sscanf(flag.Arg(2), "%d", &channel); err != nil {
				fmt.Fprintf(os.Stderr, "Invalid channel number: %s\n", flag.Arg(2))
				os.Exit(1)
			}
		}
		doStream(deviceSN, channel)
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

func doStream(deviceSN string, channel int) {
	if channel == 0 {
		ch, err := lookupChannel(deviceSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		channel = ch
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
