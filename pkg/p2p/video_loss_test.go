package p2p

import (
	"bytes"
	"testing"

	debuglog "github.com/shellster/eufy_cam/pkg/log"

	"github.com/shellster/eufy_cam/pkg/api"
)

// The station runs go-back-N retransmit: whenever a packet (or its ACK) is
// lost it re-bursts its un-ACKed window, and the genuinely missing packet
// arrives up to ~2.7s after the gap opened. The receiver must hold the gap
// open (queue newer packets, never skip on duplicate streaks) so the
// retransmit fills the hole and frames assemble intact.
func TestVideoLossResyncOnDuplicateRetransmit(t *testing.T) {
	debuglog.SetDebug(true)
	station := &api.Station{StationSN: "TEST"}
	c := NewClient(station, "testkey", nil)
	fc := &frameCollector{}
	c.SetVideoCallback("DEV", 0, fc.cb)
	defer c.Close()

	video := buildVideoData(false, 1000, testAnnexB)

	// Feed packet seq=100 in-order to initialize the sequence tracker.
	c.handleVideoMessage(0, 100, wrapWithInnerHeader(video, len(video), 0))

	// Camera sends 102, 103 — but 101 was lost in transit (gap opens at 101).
	// They must be QUEUED, not skipped past.
	c.handleVideoMessage(0, 102, wrapWithInnerHeader(video, len(video), 0))
	c.handleVideoMessage(0, 103, wrapWithInnerHeader(video, len(video), 0))

	// The station go-back-N retransmits the predecessor (100) over and over.
	// Duplicates are normal during a re-burst and must NEVER cause the
	// client to skip the gap: the genuinely missing packet (101) is still in
	// flight — observed arriving ~2.7s after the gap opened on the relayed
	// path. Skipping here discards that retransmit and manufactures the hole.
	for i := 0; i < 10; i++ {
		c.handleVideoMessage(0, 100, wrapWithInnerHeader(video, len(video), 0))
	}

	// The gap must still be held open: expected has NOT advanced past 101.
	c.mu.Lock()
	got := c.expectedSeqNo[0]
	c.mu.Unlock()
	if got != 101 {
		t.Fatalf("duplicate burst must not skip the gap: expectedSeqNo = %d, want 101 (held for retransmit)", got)
	}

	// The station's go-back-N finally retransmits the missing packet itself.
	c.handleVideoMessage(0, 101, wrapWithInnerHeader(video, len(video), 0))

	// The queued run drains in order behind it — no hole, no lost frame.
	c.mu.Lock()
	got = c.expectedSeqNo[0]
	c.mu.Unlock()
	if got != 104 {
		t.Fatalf("pipeline stalled: expectedSeqNo = %d, want 104 (gap filled in order)", got)
	}

	// Subsequent fresh packets flow normally.
	c.handleVideoMessage(0, 104, wrapWithInnerHeader(video, len(video), 0))
	c.mu.Lock()
	got = c.expectedSeqNo[0]
	c.mu.Unlock()
	if got != 105 {
		t.Fatalf("expectedSeqNo = %d, want 105", got)
	}
}

// Reordered packets (gap fills within the threshold) must NOT trigger a
// loss resync — they are queued and consumed in order.
func TestVideoReorderStillBuffers(t *testing.T) {
	station := &api.Station{StationSN: "TEST"}
	c := NewClient(station, "testkey", nil)
	fc := &frameCollector{}
	c.SetVideoCallback("DEV", 0, fc.cb)
	defer c.Close()

	video := buildVideoData(false, 2000, testAnnexB)

	c.handleVideoMessage(0, 200, wrapWithInnerHeader(video, len(video), 0))
	// Out-of-order arrival: 202 first, then 201 fills the gap.
	c.handleVideoMessage(0, 202, wrapWithInnerHeader(video, len(video), 0))
	c.handleVideoMessage(0, 201, wrapWithInnerHeader(video, len(video), 0))

	c.mu.Lock()
	got := c.expectedSeqNo[0]
	c.mu.Unlock()
	if got != 203 {
		t.Fatalf("expectedSeqNo = %d, want 203 (reorder must drain in order)", got)
	}
}

// A frame split across packets whose continuation is lost must still
// assemble byte-complete once the station's retransmit fills the gap.
// Skipping the gap (the old dup-streak behavior) reset reassembly mid-frame
// and emitted truncated AUs — the downstream decoder symptom was broken
// reference chains and a gray picture.
func TestVideoGapRetransmitAssemblesFrameIntact(t *testing.T) {
	c, fc := newVideoTestClient(t)
	defer c.Close()

	// Frame 1 spans two packets: header packet (100) + continuation (101).
	bigPayload := bytes.Repeat(testAnnexB, 18) // 216 bytes
	frame1 := buildVideoData(false, 5000, bigPayload)
	split := 100                               // header packet carries the first 100 bytes
	frame2 := buildVideoData(false, 6000, testAnnexB)

	// seq 100: header packet of frame 1 (opens the frame, 100/N bytes read).
	c.handleVideoMessage(1, 100, wrapWithInnerHeader(frame1[:split], len(frame1), 0))
	if fc.count() != 0 {
		t.Fatalf("frame 1 must not emit before its continuation: got %d frames", fc.count())
	}

	// seq 101 is lost. seq 102 (all of frame 2) arrives and must be queued,
	// not consumed out of order.
	c.handleVideoMessage(1, 102, wrapWithInnerHeader(frame2, len(frame2), 0))
	if fc.count() != 0 {
		t.Fatalf("queued frame 2 must not emit while the gap at 101 is open: got %d frames", fc.count())
	}

	// go-back-N re-burst of already-delivered packets: duplicates must never
	// skip the gap or reset the half-open frame 1 reassembly.
	for i := 0; i < 6; i++ {
		c.handleVideoMessage(1, 100, wrapWithInnerHeader(frame1[:split], len(frame1), 0))
	}

	// The station retransmits the missing continuation. On the wire a
	// continuation is a bare payload chunk (no inner header).
	c.handleVideoMessage(1, 101, frame1[split:])

	if fc.count() != 2 {
		t.Fatalf("gap fill must drain both frames in order: got %d, want 2", fc.count())
	}
	// The collector receives the assembled AU after the 22-byte eufy
	// video header is stripped.
	if fc.frames[0] != len(bigPayload) {
		t.Fatalf("frame 1 assembled %d bytes, want %d (continuation must append intact)", fc.frames[0], len(bigPayload))
	}
	if fc.frames[1] != len(testAnnexB) {
		t.Fatalf("frame 2 assembled %d bytes, want %d", fc.frames[1], len(testAnnexB))
	}
	ts := fc.timestamps()
	if ts[0] != 5000 || ts[1] != 6000 {
		t.Fatalf("frames emitted out of order: timestamps %v, want [5000 6000]", ts)
	}

	c.mu.Lock()
	st := c.videoStates[0]
	c.mu.Unlock()
	if st != nil && st.bytesToRead > 0 {
		t.Fatalf("frame reassembly left open: %d/%d", st.bytesRead, st.bytesToRead)
	}
}
