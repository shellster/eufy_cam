package stream

import (
	"bytes"
	"testing"

	"github.com/shellster/eufy_cam/pkg/p2p"
)

// Real AU shapes captured from the H.265 pan-tilt cameras. The keyframe AU
// leads with VPS/SPS/PPS/SEI before the IDR slice; the P-frame AU is
// SEI + TRAIL_R.
var (
	hevcKeyframeAU = bytes.Join([][]byte{
		{0x00, 0x00, 0x01, 0x40, 0x01}, // VPS (type 32)
		{0x00, 0x00, 0x01, 0x42, 0x01}, // SPS (type 33)
		{0x00, 0x00, 0x01, 0x44, 0x01}, // PPS (type 34)
		{0x00, 0x00, 0x01, 0x4e, 0x01}, // SEI (type 39)
		{0x00, 0x00, 0x01, 0x26, 0x01}, // IDR_W_RADL (type 19)
	}, nil)
	hevcPFrameAU = bytes.Join([][]byte{
		{0x00, 0x00, 0x01, 0x4e, 0x01}, // SEI (type 39)
		{0x00, 0x00, 0x01, 0x02, 0x01}, // TRAIL_R (type 1)
	}, nil)
	h264KeyframeAU = []byte{0x00, 0x00, 0x01, 0x65, 0x88}             // IDR (type 5)
	h264PFrameAU   = []byte{0x00, 0x00, 0x01, 0x41, 0x9a}             // non-IDR slice (type 1)
	hevcCRA        = []byte{0x00, 0x00, 0x01, 0x2a, 0x01}             // CRA (type 21)
	hevcIDRNLP     = []byte{0x00, 0x00, 0x01, 0x28, 0x01}             // IDR_N_LP (type 20)
)

func TestIsKeyframeBitstreamHEVCParamsBeforeIDR(t *testing.T) {
	// The SoloCam pan-tilt family prefixes VPS/SPS/PPS/SEI before the IDR
	// slice. A detector that only inspects the FIRST NAL sees the VPS and
	// misses every keyframe on these cameras.
	if !isKeyframeBitstream(hevcKeyframeAU, 2) {
		t.Fatal("HEVC AU with param sets before IDR must be detected as keyframe")
	}
}

func TestIsKeyframeBitstreamHEVCRandomAccessTypes(t *testing.T) {
	for name, au := range map[string][]byte{
		"IDR_W_RADL": {0x00, 0x00, 0x01, 0x26, 0x01},
		"IDR_N_LP":   hevcIDRNLP,
		"CRA":        hevcCRA,
	} {
		if !isKeyframeBitstream(au, 2) {
			t.Fatalf("%s NAL must be detected as keyframe", name)
		}
	}
}

func TestIsKeyframeBitstreamHEVCPFrame(t *testing.T) {
	if isKeyframeBitstream(hevcPFrameAU, 2) {
		t.Fatal("SEI + TRAIL_R AU must not be detected as keyframe")
	}
}

func TestIsKeyframeBitstreamH264(t *testing.T) {
	if !isKeyframeBitstream(h264KeyframeAU, 1) {
		t.Fatal("H.264 IDR NAL must be detected as keyframe")
	}
	if isKeyframeBitstream(h264PFrameAU, 1) {
		t.Fatal("H.264 non-IDR slice must not be detected as keyframe")
	}
}

func TestIsKeyframeBitstreamUnknownCodec(t *testing.T) {
	// Before the session has latched a codec, both parses are tried.
	if !isKeyframeBitstream(hevcKeyframeAU, 0) {
		t.Fatal("unknown codec: HEVC keyframe AU must be detected")
	}
	if !isKeyframeBitstream(h264KeyframeAU, 0) {
		t.Fatal("unknown codec: H.264 keyframe AU must be detected")
	}
}

func TestIsKeyframeBitstreamGarbage(t *testing.T) {
	for name, au := range map[string][]byte{
		"empty":      {},
		"no-sc":      {0x00, 0x01, 0x02, 0x26, 0x01},
		"truncated":  {0x00, 0x00, 0x01},
		"sc-at-edge": {0x00, 0x00, 0x01, 0x02},
	} {
		if isKeyframeBitstream(au, 2) {
			t.Fatalf("%s input must not be detected as keyframe", name)
		}
	}
}

// The pan-tilt cameras never set the isKeyFrame flag in the eufy video
// header, even for real IDR frames. AppendFrame must fall back to the
// bitstream so keyframe-gated serving works on these cameras.
func TestAppendFrameDetectsKeyframeDespiteUnsetHeaderFlag(t *testing.T) {
	s := newTestSession(t, "TEST-E42-KF")

	meta := func(ts uint64) p2p.VideoFrameMetadata {
		// IsKeyFrame deliberately false; StreamType 1 = H.265 frame header.
		return p2p.VideoFrameMetadata{IsKeyFrame: false, VideoTimestamp: ts, VideoFPS: 15, StreamType: 1}
	}
	s.AppendFrame(hevcKeyframeAU, meta(1000))
	s.AppendFrame(hevcPFrameAU, meta(1066))

	frames, _ := s.GetFramesSince(0)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (must serve from keyframe)", len(frames))
	}
	if !frames[0].IsKeyFrame {
		t.Fatal("bitstream IDR AU with unset header flag must be stored as keyframe")
	}
}

// Without a bitstream-detected keyframe the hold-for-keyframe gate would
// starve a viewer forever on cameras that never set the header flag; with
// it, a late-joining viewer is served exactly from the IDR.
func TestGetFramesSinceStartsAtBitstreamKeyframeForLateJoiner(t *testing.T) {
	s := newTestSession(t, "TEST-E42-JOIN")

	meta := func(ts uint64) p2p.VideoFrameMetadata {
		return p2p.VideoFrameMetadata{IsKeyFrame: false, VideoTimestamp: ts, VideoFPS: 15, StreamType: 1}
	}
	// Buffer a full GOP: IDR, P-frames, then a second IDR.
	s.AppendFrame(hevcKeyframeAU, meta(1000))
	for ts := uint64(1066); ts < 1400; ts += 66 {
		s.AppendFrame(hevcPFrameAU, meta(ts))
	}
	s.AppendFrame(hevcKeyframeAU, meta(1466))
	s.AppendFrame(hevcPFrameAU, meta(1532))

	frames, _ := s.GetFramesSince(0)
	if len(frames) == 0 {
		t.Fatal("no frames served — keyframe never detected")
	}
	if !frames[0].IsKeyFrame {
		t.Fatal("late joiner must start at the most recent keyframe")
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (second IDR + trailing P-frame)", len(frames))
	}
}
