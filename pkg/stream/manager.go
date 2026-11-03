package stream

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type TimedFrame struct {
	Data       []byte
	PTS        uint64
	IsKeyFrame bool
}

type StreamSession struct {
	ID              int64
	DeviceSN        string
	Station         string
	Channel         int
	Codec           int
	FrameBuffer     []TimedFrame
	frameIDs        []int
	NextFrameID     int
	lastKeyFrameIdx int
	StartedAt       int64
	LastUpdate      int64
	mu              sync.Mutex

	// PTS rebasing: camera timestamps reset or jump whenever the P2P stream
	// restarts. These fields force a strictly monotonic, session-relative PTS
	// so the MPEG-TS output stays valid across restarts.
	hasPTS     bool
	lastRawPTS uint64
	lastPTS    uint64
	ptsOffset  uint64

	// Corruption tracking: frames that fail validation are dropped and
	// counted; a sustained run of bad frames triggers a P2P stream restart.
	// seenValidFrame gates enforcement so an unproven camera format is served
	// through (legacy behavior) instead of blackholing.
	badFrames       int
	seenValidFrame  bool
	everHadFrame    bool
	awaitKeyframe   bool
	lastRestartAt   int64
}

var (
	streamIDCounter int64
	// activeStreams is keyed by "deviceSN#channel" (one session per
	// device channel).
	activeStreams = make(map[string]*StreamSession)
	streamsMutex  sync.RWMutex
)

// streamKey builds the session map key for a device channel.
func streamKey(deviceSN string, channel int) string {
	return fmt.Sprintf("%s#%d", deviceSN, channel)
}

func StartStream(deviceSN, stationSN string, channel, codec int) *StreamSession {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	key := streamKey(deviceSN, channel)
	if existing, ok := activeStreams[key]; ok && !existing.isStaleLocked() {
		// Reuse the live session instead of replacing it (a second viewer
		// must not reset the frame buffer mid-GOP).
		return existing
	}

	session := &StreamSession{
		ID:              atomic.AddInt64(&streamIDCounter, 1),
		DeviceSN:        deviceSN,
		Station:         stationSN,
		Channel:         channel,
		Codec:           codec,
		FrameBuffer:     make([]TimedFrame, 0),
		StartedAt:       time.Now().UnixMilli(),
		LastUpdate:      time.Now().UnixMilli(),
		lastKeyFrameIdx: -1,
		awaitKeyframe:   true,
	}

	activeStreams[key] = session
	return session
}

func StopStream(deviceSN string, channel int) error {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	key := streamKey(deviceSN, channel)
	session, ok := activeStreams[key]
	if !ok {
		return fmt.Errorf("stream not found for %s", key)
	}

	delete(activeStreams, key)

	session.mu.Lock()
	defer session.mu.Unlock()
	session.FrameBuffer = nil

	return nil
}

// StopStreamIfID stops the stream only if its ID matches, preventing
// a stale cleanup from killing a newly started stream.
func StopStreamIfID(deviceSN string, channel int, id int64) bool {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	key := streamKey(deviceSN, channel)
	session, ok := activeStreams[key]
	if !ok || session.ID != id {
		return false
	}

	delete(activeStreams, key)

	session.mu.Lock()
	defer session.mu.Unlock()
	session.FrameBuffer = nil
	return true
}

// GetStream returns the session for a device channel. Use -1 to look up
// whichever session exists for the device.
func GetStream(deviceSN string, channel int) *StreamSession {
	streamsMutex.RLock()
	defer streamsMutex.RUnlock()

	if session, ok := activeStreams[streamKey(deviceSN, channel)]; ok {
		return session
	}
	if channel < 0 {
		// Legacy/ambiguous lookup: return any session for this device.
		prefix := deviceSN + "#"
		for k, s := range activeStreams {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				return s
			}
		}
	}
	return nil
}

// StreamChannelsForDevice returns the channels that currently have live
// sessions for a device.
func StreamChannelsForDevice(deviceSN string) []int {
	streamsMutex.RLock()
	defer streamsMutex.RUnlock()

	prefix := deviceSN + "#"
	var channels []int
	for k, s := range activeStreams {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			channels = append(channels, s.Channel)
		}
	}
	sort.Ints(channels)
	return channels
}

