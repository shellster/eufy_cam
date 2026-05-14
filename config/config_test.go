package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultTimeouts(t *testing.T) {
	path := writeTestConfig(t, `[p2p]
local_port = 0
connection_type = 2
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.P2P.ConnectionTimeout != 60 {
		t.Errorf("ConnectionTimeout = %d, want 60", cfg.P2P.ConnectionTimeout)
	}
	if cfg.P2P.EncryptionTimeout != 30 {
		t.Errorf("EncryptionTimeout = %d, want 30", cfg.P2P.EncryptionTimeout)
	}
	if cfg.P2P.DiscoveryAttempts != 30 {
		t.Errorf("DiscoveryAttempts = %d, want 30", cfg.P2P.DiscoveryAttempts)
	}
	if cfg.P2P.AckTimeout != 15 {
		t.Errorf("AckTimeout = %d, want 15", cfg.P2P.AckTimeout)
	}
}

func TestCustomTimeouts(t *testing.T) {
	path := writeTestConfig(t, `[p2p]
local_port = 0
connection_type = 2
connection_timeout = 120
encryption_timeout = 60
discovery_attempts = 50
ack_timeout = 30
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.P2P.ConnectionTimeout != 120 {
		t.Errorf("ConnectionTimeout = %d, want 120", cfg.P2P.ConnectionTimeout)
	}
	if cfg.P2P.EncryptionTimeout != 60 {
		t.Errorf("EncryptionTimeout = %d, want 60", cfg.P2P.EncryptionTimeout)
	}
	if cfg.P2P.DiscoveryAttempts != 50 {
		t.Errorf("DiscoveryAttempts = %d, want 50", cfg.P2P.DiscoveryAttempts)
	}
	if cfg.P2P.AckTimeout != 30 {
		t.Errorf("AckTimeout = %d, want 30", cfg.P2P.AckTimeout)
	}
}

func TestEnvVarOverrides(t *testing.T) {
	path := writeTestConfig(t, `[p2p]
connection_timeout = 90
`)

	os.Setenv("P2P_CONNECTION_TIMEOUT", "45")
	os.Setenv("P2P_ENCRYPTION_TIMEOUT", "20")
	os.Setenv("P2P_DISCOVERY_ATTEMPTS", "25")
	os.Setenv("P2P_ACK_TIMEOUT", "12")
	defer func() {
		os.Unsetenv("P2P_CONNECTION_TIMEOUT")
		os.Unsetenv("P2P_ENCRYPTION_TIMEOUT")
		os.Unsetenv("P2P_DISCOVERY_ATTEMPTS")
		os.Unsetenv("P2P_ACK_TIMEOUT")
	}()

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.P2P.ConnectionTimeout != 45 {
		t.Errorf("ConnectionTimeout = %d, want 45 (env override)", cfg.P2P.ConnectionTimeout)
	}
	if cfg.P2P.EncryptionTimeout != 20 {
		t.Errorf("EncryptionTimeout = %d, want 20 (env override)", cfg.P2P.EncryptionTimeout)
	}
	if cfg.P2P.DiscoveryAttempts != 25 {
		t.Errorf("DiscoveryAttempts = %d, want 25 (env override)", cfg.P2P.DiscoveryAttempts)
	}
	if cfg.P2P.AckTimeout != 12 {
		t.Errorf("AckTimeout = %d, want 12 (env override)", cfg.P2P.AckTimeout)
	}
}

func TestZeroValuesGetDefaults(t *testing.T) {
	path := writeTestConfig(t, `[p2p]
connection_timeout = 0
encryption_timeout = 0
discovery_attempts = 0
ack_timeout = 0
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.P2P.ConnectionTimeout != 60 {
		t.Errorf("ConnectionTimeout = %d, want 60 (zero default)", cfg.P2P.ConnectionTimeout)
	}
	if cfg.P2P.EncryptionTimeout != 30 {
		t.Errorf("EncryptionTimeout = %d, want 30 (zero default)", cfg.P2P.EncryptionTimeout)
	}
	if cfg.P2P.DiscoveryAttempts != 30 {
		t.Errorf("DiscoveryAttempts = %d, want 30 (zero default)", cfg.P2P.DiscoveryAttempts)
	}
	if cfg.P2P.AckTimeout != 15 {
		t.Errorf("AckTimeout = %d, want 15 (zero default)", cfg.P2P.AckTimeout)
	}
}

func TestConfigDurationConversion(t *testing.T) {
	path := writeTestConfig(t, `[p2p]
connection_timeout = 45
encryption_timeout = 20
ack_timeout = 8
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	connDur := time.Duration(cfg.P2P.ConnectionTimeout) * time.Second
	encDur := time.Duration(cfg.P2P.EncryptionTimeout) * time.Second
	ackDur := time.Duration(cfg.P2P.AckTimeout) * time.Second

	if connDur != 45*time.Second {
		t.Errorf("connDur = %v, want 45s", connDur)
	}
	if encDur != 20*time.Second {
		t.Errorf("encDur = %v, want 20s", encDur)
	}
	if ackDur != 8*time.Second {
		t.Errorf("ackDur = %v, want 8s", ackDur)
	}
}
