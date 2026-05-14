package p2p

import (
	"testing"
	"time"

	"github.com/shellster/eufy_cam/pkg/api"
)

func TestDefaultTimeouts(t *testing.T) {
	station := &api.Station{StationSN: "TEST"}
	c := NewClient(station, "testkey", nil)

	if c.discoveryAttempts != 15 {
		t.Errorf("default discoveryAttempts = %d, want 15", c.discoveryAttempts)
	}
	if c.ackTimeout != 10*time.Second {
		t.Errorf("default ackTimeout = %v, want 10s", c.ackTimeout)
	}
}

func TestSetTimeouts(t *testing.T) {
	station := &api.Station{StationSN: "TEST"}
	c := NewClient(station, "testkey", nil)

	c.SetTimeouts(42, 25*time.Second)

	if c.discoveryAttempts != 42 {
		t.Errorf("discoveryAttempts = %d, want 42", c.discoveryAttempts)
	}
	if c.ackTimeout != 25*time.Second {
		t.Errorf("ackTimeout = %v, want 25s", c.ackTimeout)
	}
}

func TestSetTimeoutsOverridesDefaults(t *testing.T) {
	station := &api.Station{StationSN: "TEST"}
	c := NewClient(station, "testkey", nil)

	if c.discoveryAttempts != 15 {
		t.Errorf("pre-override discoveryAttempts = %d, want 15", c.discoveryAttempts)
	}

	c.SetTimeouts(100, 60*time.Second)

	if c.discoveryAttempts != 100 {
		t.Errorf("post-override discoveryAttempts = %d, want 100", c.discoveryAttempts)
	}
	if c.ackTimeout != 60*time.Second {
		t.Errorf("post-override ackTimeout = %v, want 60s", c.ackTimeout)
	}
}
