package stream

import "testing"

func TestStopStreamIfID(t *testing.T) {
	t.Cleanup(func() { _ = StopStream("TEST-GUARD", 0) })
	s := StartStream("TEST-GUARD", "STATION", 0, 0)

	// Stopping with the LIVE session's ID removes the session.
	if !StopStreamIfID("TEST-GUARD", 0, s.ID) {
		t.Fatal("matching ID must stop the stream")
	}
	if GetStream("TEST-GUARD", 0) != nil {
		t.Fatal("stream must be gone after StopStreamIfID")
	}

	// A stale cleanup (old ID) against a newly started stream must NOT
	// stop it — this is the guard that protects a fresh consumer's session
	// from a grace timer armed before it arrived.
	s2 := StartStream("TEST-GUARD", "STATION", 0, 0)
	if s2 == nil {
		t.Fatal("replacement stream must exist")
	}
	if StopStreamIfID("TEST-GUARD", 0, s.ID) {
		t.Fatal("stale ID must not stop the replacement stream")
	}
	if GetStream("TEST-GUARD", 0) == nil {
		t.Fatal("replacement stream must survive a stale-ID stop")
	}
}
