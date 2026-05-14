package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/pelletier/go-toml"
)

type Config struct {
	Eufy   EufyConfig   `toml:"eufy"`
	Server ServerConfig  `toml:"server"`
	P2P    P2PConfig     `toml:"p2p"`
	Auth   AuthConfig    `toml:"auth"`
	Stream StreamConfig  `toml:"stream"`
}

type EufyConfig struct {
	Username          string `toml:"username"`
	Password          string `toml:"password"`
	Country           string `toml:"country"`
	Language          string `toml:"language"`
	TrustedDeviceName string `toml:"trusted_device_name"`
}

type ServerConfig struct {
	Host  string `toml:"host"`
	Port  int    `toml:"port"`
	Debug bool   `toml:"debug"`
}

type AuthConfig struct {
	Type     string `toml:"type"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

func (a *AuthConfig) IsDigest() bool {
	return a.Type == "digest"
}

type P2PConfig struct {
	LocalPort         int `toml:"local_port"`
	ConnectionType    int `toml:"connection_type"`
	ConnectionTimeout int `toml:"connection_timeout"`
	EncryptionTimeout int `toml:"encryption_timeout"`
	DiscoveryAttempts int `toml:"discovery_attempts"`
	AckTimeout        int `toml:"ack_timeout"`
}

type StreamConfig struct {
	Enabled bool   `toml:"enabled"`
	Port    int    `toml:"port"`
	Bind    string `toml:"bind"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Override with env vars
	if v := os.Getenv("EUFY_USERNAME"); v != "" {
		cfg.Eufy.Username = v
	}
	if v := os.Getenv("EUFY_PASSWORD"); v != "" {
		cfg.Eufy.Password = v
	}
	if v := os.Getenv("EUFY_COUNTRY"); v != "" {
		cfg.Eufy.Country = v
	}
	if v := os.Getenv("EUFY_LANGUAGE"); v != "" {
		cfg.Eufy.Language = v
	}
	if v := os.Getenv("EUFY_TRUSTED_DEVICE_NAME"); v != "" {
		cfg.Eufy.TrustedDeviceName = v
	}
	if v := os.Getenv("SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("SERVER_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err == nil {
			cfg.Server.Port = port
		}
	}
	if v := os.Getenv("SERVER_DEBUG"); v != "" {
		cfg.Server.Debug = v == "1" || v == "true" || v == "yes"
	}
	if v := os.Getenv("P2P_LOCAL_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err == nil {
			cfg.P2P.LocalPort = port
		}
	}
	if v := os.Getenv("P2P_CONNECTION_TYPE"); v != "" {
		typ, err := strconv.Atoi(v)
		if err == nil {
			cfg.P2P.ConnectionType = typ
		}
	}
	if v := os.Getenv("P2P_CONNECTION_TIMEOUT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.P2P.ConnectionTimeout = i
		}
	}
	if v := os.Getenv("P2P_ENCRYPTION_TIMEOUT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.P2P.EncryptionTimeout = i
		}
	}
	if v := os.Getenv("P2P_DISCOVERY_ATTEMPTS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.P2P.DiscoveryAttempts = i
		}
	}
	if v := os.Getenv("P2P_ACK_TIMEOUT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.P2P.AckTimeout = i
		}
	}
	if v := os.Getenv("AUTH_TYPE"); v != "" {
		cfg.Auth.Type = v
	}
	if v := os.Getenv("AUTH_USERNAME"); v != "" {
		cfg.Auth.Username = v
	}
	if v := os.Getenv("AUTH_PASSWORD"); v != "" {
		cfg.Auth.Password = v
	}
	if v := os.Getenv("STREAM_ENABLED"); v != "" {
		cfg.Stream.Enabled = v == "1" || v == "true" || v == "yes"
	}
	if v := os.Getenv("STREAM_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err == nil {
			cfg.Stream.Port = port
		}
	}
	if v := os.Getenv("STREAM_BIND"); v != "" {
		cfg.Stream.Bind = v
	}

	// Set defaults
	if cfg.Eufy.Country == "" {
		cfg.Eufy.Country = "US"
	}
	if cfg.Eufy.Language == "" {
		cfg.Eufy.Language = "en"
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.P2P.LocalPort == 0 {
		cfg.P2P.LocalPort = 0
	}
	if cfg.P2P.ConnectionType == 0 {
		cfg.P2P.ConnectionType = 2 // QUICKEST
	}
	if cfg.P2P.ConnectionTimeout == 0 {
		cfg.P2P.ConnectionTimeout = 60
	}
	if cfg.P2P.EncryptionTimeout == 0 {
		cfg.P2P.EncryptionTimeout = 30
	}
	if cfg.P2P.DiscoveryAttempts == 0 {
		cfg.P2P.DiscoveryAttempts = 30
	}
	if cfg.P2P.AckTimeout == 0 {
		cfg.P2P.AckTimeout = 15
	}
	if cfg.Stream.Bind == "" {
		cfg.Stream.Bind = "0.0.0.0"
	}

	return &cfg, nil
}
