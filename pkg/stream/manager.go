package stream

import (
	"fmt"
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
	IsMuxed         bool
	FrameBuffer     []TimedFrame
	frameIDs        []int
	NextFrameID     int
	lastKeyFrameIdx int
	StartedAt       int64
	LastUpdate      int64
	mu              sync.Mutex
	RestartMu       sync.Mutex
}

var (
	streamIDCounter int64
	activeStreams    = make(map[string]*StreamSession)
	streamsMutex    sync.RWMutex
)

func StartStream(deviceSN, stationSN string, channel, codec int) *StreamSession {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	session := &StreamSession{
		ID:          atomic.AddInt64(&streamIDCounter, 1),
		DeviceSN:    deviceSN,
		Station:     stationSN,
		Channel:     channel,
		Codec:       codec,
		FrameBuffer: make([]TimedFrame, 0),
		StartedAt:   time.Now().UnixMilli(),
		LastUpdate:  time.Now().UnixMilli(),
	}

	activeStreams[deviceSN] = session
	return session
}

func StopStream(deviceSN string) error {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	session, ok := activeStreams[deviceSN]
	if !ok {
		return fmt.Errorf("stream not found for device %s", deviceSN)
	}

	delete(activeStreams, deviceSN)

	session.mu.Lock()
	defer session.mu.Unlock()
	session.FrameBuffer = nil

	return nil
}

// StopStreamIfID stops the stream only if its ID matches, preventing
// a stale cleanup from killing a newly started stream.
func StopStreamIfID(deviceSN string, id int64) bool {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	session, ok := activeStreams[deviceSN]
	if !ok || session.ID != id {
		return false
	}

	delete(activeStreams, deviceSN)
	session.mu.Lock()
	defer session.mu.Unlock()
	session.FrameBuffer = nil
	return true
}

func GetStream(deviceSN string) *StreamSession {
	streamsMutex.RLock()
	defer streamsMutex.RUnlock()
	return activeStreams[deviceSN]
}

func GetAllStreams() []*StreamSession {
	streamsMutex.RLock()
	defer streamsMutex.RUnlock()

	streams := make([]*StreamSession, 0, len(activeStreams))
	for _, s := range activeStreams {
		streams = append(streams, s)
	}
	return streams
}

func StopAll() {
	streamsMutex.Lock()
	defer streamsMutex.Unlock()

	for deviceSN := range activeStreams {
		delete(activeStreams, deviceSN)
	}
}
