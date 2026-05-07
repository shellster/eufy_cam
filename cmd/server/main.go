package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/shellster/eufy_cam/config"
	"github.com/shellster/eufy_cam/pkg/api"
	"github.com/shellster/eufy_cam/pkg/devices"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/pkg/web"
	staticpkg "github.com/shellster/eufy_cam"
)

var (
	cfg        *config.Config
	apiClient  *api.Client
	webServer  *web.Server
	httpServer *http.Server
)

func main() {
	debugFlag := flag.Bool("debug", false, "Enable debug logging")
	flag.Parse()

	var configPath string
	args := flag.Args()
	if len(args) > 0 {
		configPath = args[0]
	} else {
		configPath = "config.toml"
	}

	var err error
	cfg, err = config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if *debugFlag || cfg.Server.Debug {
		debuglog.SetDebug(true)
	}

	log.Printf("Starting Eufy Camera Streamer...")
	log.Printf("Config: Host=%s, Port=%d, Eufy Country=%s, P2P Type=%d",
		cfg.Server.Host, cfg.Server.Port, cfg.Eufy.Country, cfg.P2P.ConnectionType)

	apiClient, err = api.Init(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize API client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %v, shutting down...", sig)
		cancel()
		cleanup()
		os.Exit(0)
	}()

	// Start web server immediately — login/captcha handled via web UI
	startWebServer(ctx)

	// Attempt initial login in background
	go attemptLogin(ctx)

	// Wait for shutdown
	<-ctx.Done()
}

func attemptLogin(ctx context.Context) {
	log.Println("Attempting login to Eufy cloud...")

	err := apiClient.Login()
	if err == nil {
		log.Println("Login successful!")
		webServer.SetLoggedIn(true)
		loadAndConnectStations(ctx)
		return
	}

	loginErr, ok := err.(*api.LoginError)
	if ok && loginErr.Captcha != nil {
		log.Println("Captcha required — waiting for browser input...")
		webServer.SetCaptcha(loginErr.Captcha)
		openBrowser(fmt.Sprintf("http://localhost:%d/login", cfg.Server.Port))
		return
	}

	log.Printf("Login failed: %v", err)
	log.Println("You can retry from the web UI at /login")
}

func onLoginSuccess() {
	loadAndConnectStations(context.Background())
}

func loadAndConnectStations(ctx context.Context) {
	if err := loadStations(ctx); err != nil {
		log.Printf("Failed to load stations: %v", err)
	}
}

func loadStations(ctx context.Context) error {
	log.Println("Fetching stations from Eufy cloud...")

	stations, err := apiClient.GetStations()
	if err != nil {
		return fmt.Errorf("failed to get stations: %w", err)
	}

	log.Printf("Found %d stations", len(stations))

	for _, station := range stations {
		st := station
		log.Printf("Station: sn=%s p2pdid=%s appconn=%q name=%s", st.StationSN, st.P2PDID, st.AppConn, st.Name)
		if st.P2PDID == "" {
			log.Printf("Warning: Station %s has no P2P DID", st.StationSN)
			continue
		}

		if devices.GetStation(st.StationSN) == nil {
			devices.AddOrUpdateStation(&st)
		}
	}

	return connectStations(ctx)
}

func connectStations(ctx context.Context) error {
	log.Println("Connecting to stations via P2P...")

	allStations := devices.GetAllStations()
	var wg sync.WaitGroup
	connectedCount := 0

	for _, station := range allStations {
		wg.Add(1)
		go func(s *devices.Station) {
			defer wg.Done()

			log.Printf("Connecting to station %s...", s.GetSerial())

			dskKey := ""
			dsk, err := apiClient.GetDSKKeys(s.GetSerial())
			if err != nil {
				log.Printf("Warning: failed to get DSK key for %s: %v", s.GetSerial(), err)
			} else {
				dskKey = dsk.DSKKey
			}

			if err := s.ConnectWithAPI(dskKey, apiClient); err != nil {
				log.Printf("Failed to connect to station %s: %v", s.GetSerial(), err)
				return
			}

			log.Printf("Connected to station %s", s.GetSerial())
			connectedCount++
		}(station)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	wg.Wait()

	if connectedCount > 0 {
		log.Printf("Successfully connected to %d/%d stations", connectedCount, len(allStations))
	}

	return nil
}

func startWebServer(ctx context.Context) {
	webServer = web.NewServer(apiClient, cfg, staticpkg.Files)
	webServer.SetOnLoginSuccess(onLoginSuccess)

	r := mux.NewRouter()
	webServer.RegisterRoutes(r)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)

	digestMiddleware := web.NewDigestMiddleware(&cfg.Auth)

	httpServer = &http.Server{
		Addr:         addr,
		Handler:      recoveryMiddleware(digestMiddleware(r)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,
	}

	go func() {
		log.Printf("Starting web server on %s", addr)

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Web server error: %v", err)
		}
	}()

	log.Printf("Web server listening on %s", addr)

	// Graceful shutdown in background
	go func() {
		<-ctx.Done()
		log.Println("Shutting down web server...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("PANIC: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func cleanup() {
	log.Println("Cleaning up...")

	if webServer != nil {
		log.Println("Disconnecting all stations...")
		devices.DisconnectAll()
	}

	log.Println("Done")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		log.Printf("Open this URL in your browser: %s", url)
		return
	}
	_ = cmd.Start()
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}
