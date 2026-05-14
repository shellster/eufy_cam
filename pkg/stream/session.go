package stream

import (
	"sort"
	"time"

	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/pkg/p2p"
)

const maxFrames = 100
const maxBufferBytes = 4 * 1024 * 1024 // 4MB
const StaleThreshold = 5 * time.Second

func (s *StreamSession) IsStale() bool {
	if s.LastUpdate == 0 {
		return false
	}
	return time.Since(time.UnixMilli(s.LastUpdate)) > StaleThreshold
}

func (s *StreamSession) ResetLastUpdate() {
	s.LastUpdate = time.Now().UnixMilli()
}

func (s *StreamSession) AppendFrame(frameData []byte, metadata p2p.VideoFrameMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastUpdate = time.Now().UnixMilli()

	if s.Codec == 0 {
		detected := p2p.GetVideoCodec(frameData)
		if detected == p2p.VideoCodecH265 {
			s.Codec = 2
		} else if detected == p2p.VideoCodecH264 {
			s.Codec = 1
		} else if metadata.StreamType == 2 {
			s.Codec = 2
		}
		if s.Codec != 0 {
			debuglog.Debugf("StreamSession: detected codec %d for %s", s.Codec, s.DeviceSN)
		}
	}

	s.NextFrameID++
	s.FrameBuffer = append(s.FrameBuffer, TimedFrame{
		Data: frameData,
		PTS:  metadata.VideoTimestamp,
	})
	s.frameIDs = append(s.frameIDs, s.NextFrameID)

	if metadata.IsKeyFrame {
		s.lastKeyFrameIdx = len(s.FrameBuffer) - 1
	}

	// Drop oldest frames if buffer exceeds size limit
	dropped := 0
	var totalBytes int
	for i := len(s.FrameBuffer) - 1; i >= 0; i-- {
		totalBytes += len(s.FrameBuffer[i].Data)
		if totalBytes > maxBufferBytes {
			dropped = i + 1
			break
		}
	}
	if dropped > 0 {
		s.FrameBuffer = s.FrameBuffer[dropped:]
		s.frameIDs = s.frameIDs[dropped:]
		s.lastKeyFrameIdx -= dropped
		if s.lastKeyFrameIdx < 0 {
			s.lastKeyFrameIdx = 0
		}
	}

	if len(s.FrameBuffer) > maxFrames {
		dropCount := len(s.FrameBuffer) - maxFrames
		s.FrameBuffer = s.FrameBuffer[dropCount:]
		s.frameIDs = s.frameIDs[dropCount:]
		s.lastKeyFrameIdx -= dropCount
		if s.lastKeyFrameIdx < 0 {
			s.lastKeyFrameIdx = 0
		}
	}
}

// GetFramesSince returns all frames after sinceID. If sinceID is before the
// last keyframe, frames start from the keyframe so the client can resync.
// Returns the frames and the nextID to pass on the next call.
func (s *StreamSession) GetFramesSince(sinceID int) ([]TimedFrame, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.FrameBuffer) == 0 {
		return nil, sinceID
	}

	// Find where to start reading
	startIdx := 0
	if sinceID > 0 {
		idx := sort.SearchInts(s.frameIDs, sinceID+1)
		// If client is too far behind, resync from last keyframe
		if idx < s.lastKeyFrameIdx {
			idx = s.lastKeyFrameIdx
		}
		startIdx = idx
	} else {
		// New client — start from last keyframe
		startIdx = s.lastKeyFrameIdx
	}

	if startIdx >= len(s.FrameBuffer) {
		return nil, s.NextFrameID
	}

	frames := make([]TimedFrame, len(s.FrameBuffer)-startIdx)
	copy(frames, s.FrameBuffer[startIdx:])
	return frames, s.NextFrameID
}

func (s *StreamSession) ClearBuffer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FrameBuffer = s.FrameBuffer[:0]
	s.frameIDs = s.frameIDs[:0]
	s.lastKeyFrameIdx = 0
}

func (s *StreamSession) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.LastUpdate > 0
}

func (s *StreamSession) FrameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.FrameBuffer)
}
