package breaker

import (
	"testing"
	"time"
)

// The window is a ring of 10 slots, one per Window/10. Cases are pinned to
// slot boundaries so they do not depend on the exact instant of a call.

func TestWindowExpiresByTime(t *testing.T) {
	w := newWindow(100 * time.Millisecond) // 10 slots of 10ms
	base := time.Unix(1000, 0)

	w.add(base, false)
	w.add(base.Add(5*time.Millisecond), true) // same slot
	if w.total != 2 || w.failures != 1 {
		t.Fatalf("total=%d failures=%d", w.total, w.failures)
	}

	// Nine slots later the first outcomes are still inside the window.
	w.add(base.Add(99*time.Millisecond), true)
	if w.total != 3 || w.failures != 1 {
		t.Fatalf("total=%d failures=%d; outcomes inside the window were dropped", w.total, w.failures)
	}

	// Rolling into the next slot pushes the oldest one (the two first
	// outcomes) out.
	w.add(base.Add(100*time.Millisecond), true)
	if w.total != 2 || w.failures != 0 {
		t.Fatalf("total=%d failures=%d, want the recent outcomes only", w.total, w.failures)
	}

	// After ten more rolls nothing from before remains.
	w.add(base.Add(200*time.Millisecond), true)
	if w.total != 1 || w.failures != 0 {
		t.Fatalf("total=%d failures=%d, want only the fresh outcome", w.total, w.failures)
	}
}

func TestWindowClear(t *testing.T) {
	w := newWindow(time.Minute)
	w.add(time.Unix(0, 0), false)
	w.clear()
	if over, n := w.tripped(0, 0); over || n != 0 {
		t.Fatalf("clear: over=%v n=%d", over, n)
	}
}

func TestWindowTripped(t *testing.T) {
	w := newWindow(100 * time.Millisecond)
	base := time.Unix(1000, 0)
	for i := 0; i < 10; i++ {
		w.add(base, i%2 == 0) // 5 of 10 fail
	}
	if over, n := w.tripped(0.5, 10); !over || n != 10 {
		t.Fatalf("tripped = %v with %d samples, want true at 50%%", over, n)
	}
	if over, _ := w.tripped(0.5, 11); over {
		t.Fatal("tripped below MinSamples")
	}
	if over, _ := w.tripped(0.6, 10); over {
		t.Fatal("tripped at 50%% with a 60%% threshold")
	}
}
