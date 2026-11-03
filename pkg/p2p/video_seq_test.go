package p2p

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/shellster/eufy_cam/pkg/api"
)

var testAnnexB = []byte{0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f, 0xda, 0x05, 0x01, 0xa2, 0x4b}

// buildVideoData builds the video frame payload (22-byte header + NAL data).
func buildVideoData(isKey bool, ts uint64, payload []byte) []byte {
	video := make([]byte, 22+len(payload))
	binary.LittleEndian.PutUint32(video[0:4], uint32(len(payload)))
	if isKey {
		video[4] = 1
	}
	video[5] = 0
	binary.LittleEndian.PutUint16(video[6:8], 1)
	binary.LittleEndian.PutUint16(video[8:10], 15)
	binary.LittleEndian.PutUint16(video[10:12], 1920)
	binary.LittleEndian.PutUint16(video[12:14], 1080)
	for i := 0; i < 6; i++ {
		video[14+i] = byte(ts >> (8 * uint(i)))
	}
	copy(video[22:], payload)
	return video
}

// wrapWithInnerHeader prepends the 16-byte inner P2P data header and declares
// the video payload to be totalLen bytes (may exceed the carried data to
// simulate multi-packet frames).
func wrapWithInnerHeader(video []byte, totalLen int, channel byte) []byte {
	hdr := make([]byte, P2PDataHeaderBytes)
	copy(hdr[0:4], MagicWord)
	binary.LittleEndian.PutUint16(hdr[4:6], uint16(CMD_VIDEO_FRAME))
	binary.LittleEndian.PutUint32(hdr[6:10], uint32(totalLen))
	hdr[12] = channel
	hdr[13] = 0
	hdr[14] = 1
	return append(hdr, video...)
}

type frameCollector struct {
	mu     sync.Mutex
	frames []int    // payload lengths
	ts     []uint64 // video timestamps
}

func (fc *frameCollector) cb(deviceSN string, channel byte, frameData []byte, metadata VideoFrameMetadata) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.frames = append(fc.frames, len(frameData))
	fc.ts = append(fc.ts, metadata.VideoTimestamp)
	return nil
}

func (fc *frameCollector) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.frames)
}

func (fc *frameCollector) timestamps() []uint64 {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]uint64(nil), fc.ts...)
}

func newVideoTestClient(t *testing.T) (*Client, *frameCollector) {
	t.Helper()
	station := &api.Station{StationSN: "TEST"}
	c := NewClient(station, "testkey", nil)
	fc := &frameCollector{}
	c.SetVideoCallback("DEV", 0, fc.cb)
	return c, fc
}

func TestIncrementSequenceWraparound(t *testing.T) {
	if got := incrementSequence(65534); got != 65535 {
		t.Fatalf("incrementSequence(65534) = %d, want 65535", got)
	}
	if got := incrementSequence(65535); got != 0 {
		t.Fatalf("incrementSequence(65535) = %d, want 0 (natural uint16 wrap, 65535 must not be skipped)", got)
	}
}

func TestVideoSeqFirstPacketArbitrarySeq(t *testing.T) {
	c, fc := newVideoTestClient(t)

	// Station may start its video sequence anywhere; the first packet must be
	// accepted or the stream deadlocks.
	c.handleVideoMessage(1, 4242, wrapWithInnerHeader(buildVideoData(true, 1000, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 1 {
		t.Fatalf("first packet with arbitrary seq not processed: got %d frames", fc.count())
	}

	c.handleVideoMessage(1, 4243, wrapWithInnerHeader(buildVideoData(false, 1066, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 2 {
		t.Fatalf("in-order packet not processed: got %d frames", fc.count())
	}
}

func TestVideoSeqReorderDrainedInOrder(t *testing.T) {
	c, fc := newVideoTestClient(t)

	c.handleVideoMessage(1, 100, wrapWithInnerHeader(buildVideoData(true, 1000, testAnnexB), 22+len(testAnnexB), 0))
	// 101 arrives before 100: must be queued, not dropped...
	c.handleVideoMessage(1, 102, wrapWithInnerHeader(buildVideoData(false, 1132, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 1 {
		t.Fatalf("reordered packet should be queued, got %d frames", fc.count())
	}
	// ...and drained once the gap fills.
	c.handleVideoMessage(1, 101, wrapWithInnerHeader(buildVideoData(false, 1066, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 3 {
		t.Fatalf("gap fill should drain queue: got %d frames, want 3", fc.count())
	}

	c.mu.Lock()
	expected := c.expectedSeqNo[1]
	c.mu.Unlock()
	if expected != 103 {
		t.Fatalf("expected seq after drain = %d, want 103", expected)
	}
}

// Regression test for the readLoop buffer-aliasing bug: queued packets are
// stored while the readLoop's reusable buffer gets overwritten by later
// packets. The queue MUST retain each packet's own bytes.
func TestVideoQueueDataNotAliasedToReadBuffer(t *testing.T) {
	c, fc := newVideoTestClient(t)

	// Simulate readLoop: one shared buffer, successive packets overwrite it.
	buf := make([]byte, 65535)

	write := func(pkt []byte) {
		copy(buf, pkt)
		c.handleDataMessage(buf[4:len(pkt)], nil) // payload = outer frame minus 4-byte header
	}

	outer := func(inner []byte) []byte {
		out := make([]byte, 4)
		out[0] = 0xf1
		out[1] = 0xd0
		binary.BigEndian.PutUint16(out[2:4], uint16(len(inner)))
		return append(out, inner...)
	}

	frame := func(key bool, ts uint64, payload []byte, seq uint16) []byte {
		video := buildVideoData(key, ts, payload)
		// dataType header [0xd1 0x01] + seqNo 2 BE + inner data
		pkt := []byte{0xd1, byte(DataTypeVIDEO), byte(seq >> 8), byte(seq)}
		return append(pkt, wrapWithInnerHeader(video, len(video), 0)...)
	}

	// Packet 100 processed in order.
	p100 := outer(frame(true, 1000, testAnnexB, 100))
	write(p100)
	if fc.count() != 1 {
		t.Fatalf("first packet not processed: %d frames", fc.count())
	}

	// Packet 102 queued; buffer then overwritten by unrelated packet 103.
	p102 := outer(frame(false, 1132, testAnnexB, 102))
	write(p102)
	if fc.count() != 1 {
		t.Fatalf("out-of-order packet should be queued: %d frames", fc.count())
	}

	// Overwrite the shared buffer with a DIFFERENT packet (simulating the
	// next readLoop iteration reusing the buffer) before the gap fills.
	pOther := outer(frame(false, 99999, bigTestNAL(200), 103))
	copy(buf, pOther)

	// Now packet 101 arrives and drains queued 102 — its bytes must be the
	// ORIGINAL packet 102 data, not the overwritten buffer contents.
	p101 := outer(frame(false, 1066, testAnnexB, 101))
	write(p101)

	if fc.count() != 3 {
		t.Fatalf("gap fill should drain queue: got %d frames, want 3", fc.count())
	}

	// The drained packet-102 frame must carry ITS OWN timestamp (1132), not
	// the timestamp of whichever packet last overwrote the shared buffer.
	ts := fc.timestamps()
	wantTS := []uint64{1000, 1066, 1132}
	for i, w := range wantTS {
		if ts[i] != w {
			t.Fatalf("frame %d timestamp = %d, want %d (queued packet data was aliased to the read buffer)", i, ts[i], w)
		}
	}

	c.mu.Lock()
	st := c.videoStates[0]
	c.mu.Unlock()
	if st != nil && st.bytesToRead > 0 {
		t.Fatalf("frame reassembly left open: %d/%d", st.bytesRead, st.bytesToRead)
	}
}

func TestVideoSeqGapFastResync(t *testing.T) {
	c, fc := newVideoTestClient(t)

	c.handleVideoMessage(1, 100, wrapWithInnerHeader(buildVideoData(true, 1000, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 1 {
		t.Fatalf("got %d frames, want 1", fc.count())
	}

	// Simulate a gap that has already exceeded the resync threshold
	// (packet 101..103 lost in transit).
	c.lastVideoPacketTime = time.Now().Add(-time.Second)
	c.handleVideoMessage(1, 104, wrapWithInnerHeader(buildVideoData(false, 1200, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 2 {
		t.Fatalf("gap should trigger fast resync: got %d frames, want 2", fc.count())
	}

	c.mu.Lock()
	expected := c.expectedSeqNo[1]
	c.mu.Unlock()
	if expected != 105 {
		t.Fatalf("expected seq after resync = %d, want 105", expected)
	}

	// The late duplicate of a skipped packet must be dropped, not re-processed.
	c.handleVideoMessage(1, 102, wrapWithInnerHeader(buildVideoData(false, 1132, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 2 {
		t.Fatalf("late duplicate should be dropped: got %d frames, want 2", fc.count())
	}
}

func TestVideoSeqWraparound65535Accepted(t *testing.T) {
	c, fc := newVideoTestClient(t)

	c.handleVideoMessage(1, 65534, wrapWithInnerHeader(buildVideoData(true, 1000, testAnnexB), 22+len(testAnnexB), 0))
	c.handleVideoMessage(1, 65535, wrapWithInnerHeader(buildVideoData(false, 1066, testAnnexB), 22+len(testAnnexB), 0))
	c.handleVideoMessage(1, 0, wrapWithInnerHeader(buildVideoData(false, 1132, testAnnexB), 22+len(testAnnexB), 0))
	if fc.count() != 3 {
		t.Fatalf("wraparound packets must all be processed: got %d frames, want 3", fc.count())
	}
}

func bigTestNAL(n int) []byte {
	payload := make([]byte, n)
	copy(payload, testAnnexB)
	return payload
}

func TestProcessVideoPacketOvershootGuard(t *testing.T) {
	c, fc := newVideoTestClient(t)

	// Frame A declares 142 bytes of video data but only 60 arrive.
	full := buildVideoData(true, 1000, bigTestNAL(120))
	c.processVideoPacket(1, wrapWithInnerHeader(full[:60], len(full), 0))
	if fc.count() != 0 {
		t.Fatalf("partial frame must not be emitted yet, got %d frames", fc.count())
	}

	// A desynced continuation arrives with more bytes than the frame needs.
	// The old code would have appended and emitted a corrupted mixed frame.
	c.processVideoPacket(2, make([]byte, 100))
	if fc.count() != 0 {
		t.Fatalf("overshooting continuation must drop the frame, got %d frames", fc.count())
	}

	// The next well-formed frame must still be delivered cleanly.
	c.processVideoPacket(3, wrapWithInnerHeader(full, len(full), 0))
	if fc.count() != 1 {
		t.Fatalf("frame after desync not delivered: got %d frames, want 1", fc.count())
	}
}

func TestProcessVideoPacketDropsIncompleteFrameOnNewHeader(t *testing.T) {
	c, fc := newVideoTestClient(t)

	full := buildVideoData(true, 1000, bigTestNAL(120))
	c.processVideoPacket(1, wrapWithInnerHeader(full[:60], len(full), 0))

	// New frame starts while the previous one is still incomplete.
	full2 := buildVideoData(true, 2000, bigTestNAL(120))
	c.processVideoPacket(2, wrapWithInnerHeader(full2, len(full2), 0))
	if fc.count() != 1 {
		t.Fatalf("incomplete frame must be dropped, new frame delivered: got %d frames, want 1", fc.count())
	}
}
