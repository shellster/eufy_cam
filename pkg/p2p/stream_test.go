package p2p

import "testing"

// Real-world NAL sequences captured live from eufy cameras.
func TestGetVideoCodec(t *testing.T) {
	// H.264 keyframe: AUD + SPS + PPS + IDR (eufyCam 3C, Front Camera)
	h264Frame := []byte{
		0x00, 0x00, 0x00, 0x01, 0x09, 0xf0,
		0x00, 0x00, 0x00, 0x01, 0x67, 0x64, 0xc0, 0x28,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xeb, 0xeb, 0x3c,
		0x00, 0x00, 0x00, 0x01, 0x65, 0xaa, 0xbb,
	}

	// H.265 keyframe: AUD + VPS + SPS + PPS + IDR
	// (SoloCam S340 "Backyard" — previously misdetectected as unknown and
	// muxed into an H.264-declared TS, making the stream unplayable)
	h265Frame := []byte{
		0x00, 0x00, 0x00, 0x01, 0x40, 0x01, 0x0c, 0x01, 0xff, 0xff,
		0x00, 0x00, 0x00, 0x01, 0x42, 0x01, 0x01, 0x01,
		0x00, 0x00, 0x00, 0x01, 0x44, 0x01, 0xc1, 0x72,
		0x00, 0x00, 0x00, 0x01, 0x26, 0x01, 0xaf, 0x0c,
	}

	// H.265 with leading AUD (0x46 = 35<<1, AUD in HEVC) — 3-byte start code
	h265Aud := []byte{
		0x00, 0x00, 0x01, 0x46, 0x01, 0x50,
		0x00, 0x00, 0x01, 0x26, 0x01, 0xaf,
	}

	tests := []struct {
		name string
		data []byte
		want VideoCodec
	}{
		{"h264 keyframe", h264Frame, VideoCodecH264},
		{"h265 keyframe vps first", h265Frame, VideoCodecH265},
		{"h265 aud then slice", h265Aud, VideoCodecH265},
		{"h264 non-idr slice", []byte{0x00, 0x00, 0x01, 0x41, 0x9e}, VideoCodecH264},
		// A bare TRAIL_R slice (0x02 0x01) is ambiguous: H.264 nal type 1
		// (non-IDR slice) shares the same first byte. Real detection runs on
		// the first keyframe, which always carries VPS/SPS/PPS, so this is
		// documented Unknown behavior, not a bug.
		{"h265 trailing slice (ambiguous)", []byte{0x00, 0x00, 0x01, 0x02, 0x01, 0xaf}, VideoCodecUnknown},
		{"too short", []byte{0x00, 0x00, 0x01}, VideoCodecUnknown},
		{"no start codes", []byte{0x12, 0x34, 0x56, 0x78, 0x9a}, VideoCodecUnknown},
		{"nil", nil, VideoCodecUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetVideoCodec(tt.data); got != tt.want {
				t.Errorf("GetVideoCodec(%d bytes) = %v, want %v", len(tt.data), got, tt.want)
			}
		})
	}
}
