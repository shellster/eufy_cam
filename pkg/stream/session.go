package stream

import (
	"time"

	"github.com/shellster/eufy_cam/pkg/p2p"
)

const maxFrames = 100
const maxBufferBytes = 4 * 1024 * 1024 // 4MB

func (s *StreamSession) AppendFrame(frameData []byte, metadata p2p.VideoFrameMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastUpdate = time.Now().UnixMilli()

	if metadata.StreamType == 2 && s.Codec != 2 {
		s.Codec = 2
	}

	s.FrameBuffer = append(s.FrameBuffer, frameData)

	// Drop oldest frames if buffer exceeds size limit
	var totalBytes int
	for i := len(s.FrameBuffer) - 1; i >= 0; i-- {
		totalBytes += len(s.FrameBuffer[i])
		if totalBytes > maxBufferBytes {
			s.FrameBuffer = s.FrameBuffer[i+1:]
			break
		}
	}

	if len(s.FrameBuffer) > maxFrames {
		s.FrameBuffer = s.FrameBuffer[len(s.FrameBuffer)-maxFrames:]
	}
}

func (s *StreamSession) GetFrames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.FrameBuffer) == 0 {
		return nil
	}

	frames := s.FrameBuffer
	s.FrameBuffer = s.FrameBuffer[:0]
	return frames
}

func (s *StreamSession) ClearBuffer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FrameBuffer = make([][]byte, 0)
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
