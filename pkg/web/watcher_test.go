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
