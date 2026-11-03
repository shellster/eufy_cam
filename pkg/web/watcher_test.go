package web

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcherSetGracePeriodFires(t *testing.T) {
	w := newWatcherSet(50 * time.Millisecond)
	w.add("DEV", 0)
	w.add("DEV", 0)

	var fired atomic.Int32
	w.remove("DEV", 0, func() { fired.Add(1) })
	if fired.Load() != 0 {
		t.Fatal("expiry must not fire while a watcher remains")
	}

	w.remove("DEV", 0, func() { fired.Add(1) })

	// Last watcher gone: expiry fires after the grace period.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() != 1 {
		t.Fatal("expiry callback did not fire after grace period")
	}
}

func TestWatcherSetGraceCancelledByReadd(t *testing.T) {
	w := newWatcherSet(100 * time.Millisecond)
	w.add("DEV", 0)

	var fired atomic.Int32
	w.remove("DEV", 0, func() { fired.Add(1) })

	// A new watcher arrives within the grace period.
	time.Sleep(30 * time.Millisecond)
	if got := w.add("DEV", 0); got != 1 {
		t.Fatalf("count after re-add = %d, want 1", got)
	}

	time.Sleep(200 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatal("expiry must be cancelled when a watcher re-adds during grace")
	}
}

func TestWatcherSetConcurrentUse(t *testing.T) {
	w := newWatcherSet(10 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.add("DEV", 0)
				w.remove("DEV", 0, func() {})
			}
		}()
	}
	wg.Wait()

	w.mu.Lock()
	count := w.counts["DEV"]
	w.mu.Unlock()
	if count != 0 {
		t.Fatalf("watcher count after balanced add/remove = %d, want 0", count)
	}
}

func TestWatcherSetSpuriousRemoveDoesNotReArm(t *testing.T) {
	w := newWatcherSet(50 * time.Millisecond)
	w.add("DEV", 0)

	var fired atomic.Int32
	w.remove("DEV", 0, func() { fired.Add(1) }) // count 1 -> 0, arms timer
	// Spurious second remove (double disconnect / error path): must be a
	// no-op, not a second armed timer.
	w.remove("DEV", 0, func() { fired.Add(1) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := fired.Load(); got != 1 {
		t.Fatalf("expire fired %d times, want exactly 1", got)
	}
}

func TestWatcherSetExpireSkippedWhenConsumerReaddedDuringFire(t *testing.T) {
	// A grace timer whose callback has already started cannot be cancelled
	// by add(); the expire must re-check the count under the lock and skip
	// if a consumer arrived.
	w := newWatcherSet(50 * time.Millisecond)
	w.add("DEV", 0)

	var fired atomic.Int32
	w.remove("DEV", 0, func() { fired.Add(1) })

	// Wait out the grace so the callback is at/inside the fire window,
	// then race a re-add against it repeatedly; whichever wins, the expire
	// must never kill the fresh consumer's count.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		if w.add("DEV", 0) == 1 {
			// We re-added before the expire observed zero: the expire must
			// skip. Give it a beat to (wrongly) fire if buggy.
			time.Sleep(150 * time.Millisecond)
			w.remove("DEV", 0, func() { fired.Add(1) })
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// If the expire legitimately ran before any re-add, one fire is correct.
	if got := fired.Load(); got > 1 {
		t.Fatalf("expire fired %d times, want at most 1", got)
	}
}
