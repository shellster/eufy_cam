package stream

import (
	"testing"
	"time"

	"github.com/shellster/eufy_cam/pkg/p2p"
)

var testNAL = []byte{0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f, 0xda, 0x05, 0x01}

func newTestSession(t *testing.T, sn string) *StreamSession {
	t.Helper()
	t.Cleanup(func() { _ = StopStream(sn, 0) })
	return StartStream(sn, "STATION", 0, 0)
}

func meta(key bool, ts uint64) p2p.VideoFrameMetadata {
	return p2p.VideoFrameMetadata{IsKeyFrame: key, VideoTimestamp: ts, VideoFPS: 15}
}

func appendNAL(s *StreamSession, key bool, ts uint64) {
	s.AppendFrame(testNAL, meta(key, ts))
}

func TestAppendFrameRejectsInvalidBitstream(t *testing.T) {
	s := newTestSession(t, "TEST-BADFRAME")

	// A valid frame must arrive first to prove this camera emits Annex-B.
	appendNAL(s, true, 1000)
	if s.FrameCount() != 1 {
		t.Fatalf("valid frame not buffered: %d frames", s.FrameCount())
	}

	garbage := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01, 0x02, 0x03}
	s.AppendFrame(garbage, meta(true, 1066))
	if s.FrameCount() != 1 {
		t.Fatalf("invalid frame buffered after proof: %d frames", s.FrameCount())
	}
	if s.BadFrameCount() != 1 {
		t.Fatalf("bad frame count = %d, want 1", s.BadFrameCount())
	}

	appendNAL(s, false, 1132)
	if s.FrameCount() != 2 {
		t.Fatalf("valid frame not buffered: %d frames", s.FrameCount())
	}
	if s.BadFrameCount() != 0 {
		t.Fatalf("bad frame count not reset: %d", s.BadFrameCount())
	}
}

// Unvalidated camera formats must be served through, never blackholed.
func TestAppendFrameServesUnknownFormatBeforeProof(t *testing.T) {
	s := newTestSession(t, "TEST-UNKNOWN-FORMAT")

	garbage := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01, 0x02, 0x03}
	s.AppendFrame(garbage, meta(true, 1000))
	if s.FrameCount() != 1 {
		t.Fatalf("unproven format must be buffered (legacy behavior), got %d frames", s.FrameCount())
	}
	if s.BadFrameCount() != 0 {
		t.Fatalf("unproven format must not count as bad: %d", s.BadFrameCount())
	}

	// 60+ garbage frames without proof must NOT trigger restarts.
	for i := 0; i < badFrameRestartThreshold+10; i++ {
		s.AppendFrame(garbage, meta(false, uint64(1066+i*66)))
	}
	if s.NeedsRestart() {
		t.Fatal("corruption restart must not trigger without format proof")
	}
}

func TestNeedsRestartAfterSustainedBadFrames(t *testing.T) {
	s := newTestSession(t, "TEST-BADRESTART")

	appendNAL(s, true, 1000)

	garbage := []byte{0xff, 0xfe, 0xfd}
	for i := 0; i < badFrameRestartThreshold; i++ {
		s.AppendFrame(garbage, meta(false, uint64(1066+i*66)))
	}

	if !s.NeedsRestart() {
		t.Fatal("NeedsRestart must be true after sustained corrupt frames")
	}
	if s.IsStale() {
		t.Fatal("frames are flowing (bad ones), session must not be stale")
	}

	s.ResetFailureTracking()
	if s.NeedsRestart() {
		t.Fatal("NeedsRestart must be false after resetting failure tracking")
	}
}

func TestPTSRebaseAcrossCameraReset(t *testing.T) {
	s := newTestSession(t, "TEST-PTS-RESET")

	appendNAL(s, true, 100000)
	appendNAL(s, false, 100033)
	appendNAL(s, false, 100066)

	frames, next := s.GetFramesSince(0)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}

	// Camera timestamp resets to near zero after a stream restart.
	appendNAL(s, true, 5)
	appendNAL(s, false, 38)

	frames2, _ := s.GetFramesSince(next)
	if len(frames2) != 2 {
		t.Fatalf("got %d frames after camera reset, want 2", len(frames2))
	}

	all := append(frames, frames2...)
	for i := 1; i < len(all); i++ {
		if all[i].PTS <= all[i-1].PTS {
			t.Fatalf("PTS not monotonic at frame %d: %d <= %d (camera reset must be rebased)",
				i, all[i].PTS, all[i-1].PTS)
		}
	}
}

func TestPTSRebaseForwardJump(t *testing.T) {
	s := newTestSession(t, "TEST-PTS-JUMP")

	appendNAL(s, true, 1000)
	// Camera clock jumps far ahead: PTS must continue smoothly, not skip.
	appendNAL(s, false, 90000)

	frames, _ := s.GetFramesSince(0)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	delta := frames[1].PTS - frames[0].PTS
	if delta > maxPTSJumpMs {
		t.Fatalf("PTS forward jump of %dms was not rebased (max %dms)", delta, maxPTSJumpMs)
	}
}

func TestGetFramesSinceWaitsForKeyframe(t *testing.T) {
	s := newTestSession(t, "TEST-WAITKEY")

	// Fresh stream: first frame from the camera is always a keyframe, but if
	// only mid-GOP frames have arrived clients must wait, not decode garbage.
	appendNAL(s, false, 1000)
	frames, _ := s.GetFramesSince(0)
	if len(frames) != 0 {
		t.Fatalf("served %d mid-GOP frames to new client, want 0", len(frames))
	}

	appendNAL(s, true, 1066)
	frames, _ = s.GetFramesSince(0)
	if len(frames) != 1 {
		t.Fatalf("got %d frames after keyframe, want 1 (new client starts at last keyframe)", len(frames))
	}
	if !frames[0].IsKeyFrame {
		t.Fatal("first served frame must be a keyframe")
	}
}

func TestGetFramesSinceResyncsToLastKeyframe(t *testing.T) {
	s := newTestSession(t, "TEST-RESYNC")

	appendNAL(s, true, 1000) // key
	appendNAL(s, false, 1066)
	appendNAL(s, true, 2000) // key
	appendNAL(s, false, 2066)

	// A lagging client must be jumped to the LAST keyframe (live edge), not
	// the oldest one (backlog burst).
	frames, _ := s.GetFramesSince(0)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (from last keyframe)", len(frames))
	}
	if !frames[0].IsKeyFrame {
		t.Fatal("resync point must be a keyframe")
	}
	if frames[0].PTS != 2000 { // session-relative: first frame PTS=1000, ts 2000 -> 2000
		t.Fatalf("resynced to wrong frame: PTS=%d", frames[0].PTS)
	}
}

func TestClearBufferForcesKeyframeWait(t *testing.T) {
	s := newTestSession(t, "TEST-CLEAR")

	appendNAL(s, true, 1000)
	appendNAL(s, false, 1066)

	s.ClearBuffer()

	// Frames arriving after a stream restart come from a new encoder session:
	// nothing is served until a keyframe arrives.
	appendNAL(s, false, 5)
	frames, _ := s.GetFramesSince(0)
	if len(frames) != 0 {
		t.Fatalf("served %d frames while waiting for keyframe, want 0", len(frames))
	}

	appendNAL(s, true, 100)
	frames, _ = s.GetFramesSince(0)
	if len(frames) != 1 {
		t.Fatalf("got %d frames after keyframe, want 1 (serving resumes at the keyframe)", len(frames))
	}
}

func TestClearBufferFallbackAfterTimeout(t *testing.T) {
	s := newTestSession(t, "TEST-CLEAR-FALLBACK")

	s.ClearBuffer()
	appendNAL(s, false, 1000)

	// If no keyframe ever arrives, serving resumes after the fallback window.
	s.mu.Lock()
	s.noKeyFrameSince = time.Now().Add(-noKeyFrameFallback - time.Second).UnixMilli()
	s.mu.Unlock()

	frames, _ := s.GetFramesSince(0)
	if len(frames) != 1 {
		t.Fatalf("fallback did not resume serving: got %d frames, want 1", len(frames))
	}
}

func TestStaleDetection(t *testing.T) {
	s := newTestSession(t, "TEST-STALE")

	appendNAL(s, true, 1000)
	if s.IsStale() {
		t.Fatal("fresh session must not be stale")
	}

	s.mu.Lock()
	s.LastUpdate = time.Now().Add(-2 * StaleThreshold).UnixMilli()
	s.mu.Unlock()

	if !s.IsStale() {
		t.Fatal("session with old LastUpdate must be stale")
	}
	if !s.NeedsRestart() {
		t.Fatal("stale session must need restart")
	}
}
