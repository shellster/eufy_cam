package p2p

import (
	"testing"

	debuglog "github.com/shellster/eufy_cam/pkg/log"

	"github.com/shellster/eufy_cam/pkg/api"
)

// Regression: a lost video packet on a remote/relayed connection used to
// stall the pipeline permanently. The station go-back-N retransmits the
// packet BEFORE the lost one forever; every retransmit hit the
// "duplicate, drop it" branch, and because duplicates kept
// lastVideoPacketTime fresh, the 250ms loss-resync never fired. The stream
// froze on one frame until the stale monitor restarted the whole P2P
// session — the web app showed "one frame, then recovery, then one frame".
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

	// Camera sends 101, 102, 103 — but 101 is LOST in transit (gap opens at 101).
	c.handleVideoMessage(0, 102, wrapWithInnerHeader(video, len(video), 0))
	c.handleVideoMessage(0, 103, wrapWithInnerHeader(video, len(video), 0))

	// The station go-back-N retransmits the predecessor (100) over and over.
	// Under the old logic each of these was dropped and the resync clock kept
	// resetting, so the pipeline stalled forever here.
	for i := 0; i < 10; i++ {
		c.handleVideoMessage(0, 100, wrapWithInnerHeader(video, len(video), 0))
	}

	// The resync must have skipped the lost seq=101: expected advances past
	// it, so subsequent fresh packets (104+) are accepted again.
	c.handleVideoMessage(0, 104, wrapWithInnerHeader(video, len(video), 0))
	c.handleVideoMessage(0, 105, wrapWithInnerHeader(video, len(video), 0))

	c.mu.Lock()
	got := c.expectedSeqNo[0]
	c.mu.Unlock()
	if got != 106 {
		t.Fatalf("pipeline stalled: expectedSeqNo = %d, want 106 (must advance past lost packet)", got)
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
