package web

import "testing"

func TestPTSRemapperStartsNearZero(t *testing.T) {
	p := &ptsRemapper{}
	// A viewer joining a long-running session whose PTS is minutes in.
	if got := p.Remap(300000); got != 1000 {
		t.Fatalf("first remapped PTS = %d, want 1000", got)
	}
	if got := p.Remap(300066); got != 1066 {
		t.Fatalf("second remapped PTS = %d, want 1066", got)
	}
}

func TestPTSRemapperMonotonicAcrossSessionReplacement(t *testing.T) {
	p := &ptsRemapper{}
	var last uint64
	for pts := uint64(300000); pts < 300000+100*66; pts += 66 {
		out := p.Remap(pts)
		if out <= last {
			t.Fatalf("non-monotonic output %d after %d", out, last)
		}
		last = out
	}

	// Mid-connection session replacement: the new session's clock restarts
	// near 1000. Without a re-based clamp the difference underflows uint64
	// (~1.8e19) and the muxer emits garbage timestamps.
	p.Reset()
	for i := 0; i < 10; i++ {
		out := p.Remap(uint64(1000 + i*66))
		if out > 1<<40 {
			t.Fatalf("underflowed remapped PTS %d after replacement", out)
		}
		if out <= last {
			t.Fatalf("timestamp regressed across replacement: %d after %d", out, last)
		}
		last = out
	}
}
