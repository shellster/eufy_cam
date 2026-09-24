package stream

import (
	"bytes"
	"sort"
	"time"

	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/pkg/p2p"
)

const maxFrames = 100
const maxBufferBytes = 4 * 1024 * 1024 // 4MB

// StaleThreshold is how long without frames before the feed is considered
// dead. Must comfortably exceed camera warm-up time after a restart (battery
// cameras can take several seconds to start sending video) and normal
// inter-frame jitter, but still recover in a reasonable time.
const StaleThreshold = 8 * time.Second

// maxPTSJumpMs bounds how far the camera timestamp may jump forward between
// frames before it is treated as a clock change and rebased.
const maxPTSJumpMs = 10000

// noKeyFrameFallback is how long clients wait for a keyframe after a stream
// (re)start before falling back to serving mid-GOP frames.
const noKeyFrameFallback = 3 * time.Second

// badFrameRestartThreshold is the number of consecutively invalid frames that
// triggers a P2P livestream restart.
const badFrameRestartThreshold = 60

// restartCooldown bounds how often a full P2P reconnect is attempted.
const restartCooldown = 5 * time.Second

var annexBStartCode = []byte{0x00, 0x00, 0x01}

func (s *StreamSession) IsStale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isStaleLocked()
}

func (s *StreamSession) isStaleLocked() bool {
	if s.LastUpdate == 0 {
		return false
	}
	return time.Since(time.UnixMilli(s.LastUpdate)) > StaleThreshold
}

func (s *StreamSession) ResetLastUpdate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastUpdate = time.Now().UnixMilli()
}

// NeedsRestart reports whether the upstream feed is dead (stale) or producing
// a sustained run of corrupt frames and must be re-established. Corruption
// restarts only trigger once a valid frame has been seen (otherwise a camera
// with an unvalidated-but-working format would restart-loop forever).
func (s *StreamSession) NeedsRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isStaleLocked() || (s.seenValidFrame && s.badFrames >= badFrameRestartThreshold)
}

func (s *StreamSession) BadFrameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.badFrames
}

// ResetFailureTracking clears bad-frame counters after a restart attempt.
func (s *StreamSession) ResetFailureTracking() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badFrames = 0
}

// MarkRestartFailed forces the session back into the needs-restart state so
// the next monitor pass retries, without the caller having to wait for the
// stale threshold to elapse again.
func (s *StreamSession) MarkRestartFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastUpdate = time.Now().Add(-StaleThreshold - time.Second).UnixMilli()
}

// ShouldAttemptRestart applies a cooldown so an unreachable station does not
// cause a storm of overlapping reconnect attempts.
func (s *StreamSession) ShouldAttemptRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(time.UnixMilli(s.lastRestartAt)) > restartCooldown
}

func (s *StreamSession) MarkRestartAttempt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRestartAt = time.Now().UnixMilli()
}

func (s *StreamSession) AppendFrame(frameData []byte, metadata p2p.VideoFrameMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastUpdate = time.Now().UnixMilli()

	// Validate the bitstream before buffering: once this camera has been
	// PROVEN to emit Annex-B frames (one valid frame seen), subsequent frames
	// without a start code are reassembly garbage and are dropped so they
	// cannot poison the decoder. Before that proof, frames are passed through
	// unfiltered — never blackhole a camera whose format we haven't verified.
	hasStartCode := len(frameData) > 0 && bytes.Contains(frameData, annexBStartCode)
	if s.seenValidFrame && !hasStartCode {
		s.badFrames++
		s.totalBadFrames++
		s.droppedFrames++
		debuglog.Debugf("StreamSession: %s dropped invalid frame #%d (badRun=%d, totalBad=%d, %d bytes)",
			s.DeviceSN, s.NextFrameID+1, s.badFrames, s.totalBadFrames, len(frameData))
		return
	}
	if len(frameData) == 0 {
		s.droppedFrames++
		return
	}
	s.badFrames = 0
	if hasStartCode {
		s.seenValidFrame = true
	}

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

	pts := s.rebasePTS(metadata)

	s.NextFrameID++
	s.FrameBuffer = append(s.FrameBuffer, TimedFrame{
		Data:       frameData,
		PTS:        pts,
		IsKeyFrame: metadata.IsKeyFrame,
	})
	s.frameIDs = append(s.frameIDs, s.NextFrameID)

	if metadata.IsKeyFrame {
		s.lastKeyFrameIdx = len(s.FrameBuffer) - 1
		s.awaitKeyframe = false
	} else if s.lastKeyFrameIdx < 0 && s.noKeyFrameSince == 0 {
		s.noKeyFrameSince = time.Now().UnixMilli()
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
		s.recalcKeyFrameIdx()
	}

	if len(s.FrameBuffer) > maxFrames {
		dropCount := len(s.FrameBuffer) - maxFrames
		s.FrameBuffer = s.FrameBuffer[dropCount:]
		s.frameIDs = s.frameIDs[dropCount:]
		s.recalcKeyFrameIdx()
	}
}

// rebasePTS converts the camera timestamp into a strictly monotonic,
// session-relative PTS in milliseconds. Camera timestamps reset to near-zero
// whenever the livestream restarts; passing them through unchanged makes the
// MPEG-TS timestamps jump backwards, which breaks MSE playback.
// All arithmetic is modular (uint64 wrap), so "negative" offsets work.
func (s *StreamSession) rebasePTS(metadata p2p.VideoFrameMetadata) uint64 {
	step := uint64(33)
	if metadata.VideoFPS > 0 && metadata.VideoFPS <= 120 {
		if v := uint64(1000 / uint64(metadata.VideoFPS)); v > 0 {
			step = v
		}
	}

	raw := metadata.VideoTimestamp

	if !s.hasPTS {
		s.hasPTS = true
		s.lastRawPTS = raw
		s.lastPTS = 1000
		s.ptsOffset = 1000 - raw
		return 1000
	}

	delta := int64(raw) - int64(s.lastRawPTS)
	if delta <= 0 || delta > maxPTSJumpMs {
		// Timestamp regressed (stream restart) or jumped (clock change):
		// rebase so output continues from lastPTS + step.
		s.ptsOffset = s.lastPTS + step - raw
		debuglog.Debugf("StreamSession: %s PTS rebased (raw delta=%dms)", s.DeviceSN, delta)
	}

	pts := raw + s.ptsOffset
	s.lastPTS = pts
	s.lastRawPTS = raw
	return pts
}

// GetFramesSince returns all frames after sinceID. If sinceID is before the
// last keyframe, frames start from the keyframe so the client can resync.
// Returns the frames and the nextID to pass on the next call.
// After a stream (re)start, no frames are served until a keyframe arrives (or
// the noKeyFrameFallback timeout expires) so clients never receive undecodable
// mid-GOP data from a fresh encoder session.
func (s *StreamSession) GetFramesSince(sinceID int) ([]TimedFrame, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.FrameBuffer) == 0 {
		return nil, sinceID
	}

	if s.lastKeyFrameIdx < 0 {
		// No keyframe in the buffer yet. Wait for one so every client starts
		// at a decodable point, but do not wait forever.
		if s.noKeyFrameSince == 0 || time.Since(time.UnixMilli(s.noKeyFrameSince)) < noKeyFrameFallback {
			return nil, sinceID
		}
	}

	// Find where to start reading
	startIdx := 0
	if sinceID > 0 {
		idx := sort.SearchInts(s.frameIDs, sinceID+1)
		// If client is too far behind, resync from last keyframe
		if s.lastKeyFrameIdx >= 0 && idx < s.lastKeyFrameIdx {
			idx = s.lastKeyFrameIdx
		}
		startIdx = idx
	} else if s.lastKeyFrameIdx >= 0 {
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
	s.lastKeyFrameIdx = -1
	// The next frames come from a fresh encoder session: wait for a keyframe
	// before serving anything.
	s.awaitKeyframe = true
	s.noKeyFrameSince = time.Now().UnixMilli()
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

// recalcKeyFrameIdx rescans the (trimmed) buffer for the LAST keyframe.
// Clients that fall behind resync to it, landing near the live edge rather
// than accumulating backlog from the oldest keyframe.
func (s *StreamSession) recalcKeyFrameIdx() {
	s.lastKeyFrameIdx = -1
	for i, f := range s.FrameBuffer {
		if f.IsKeyFrame {
			s.lastKeyFrameIdx = i
		}
	}
}
