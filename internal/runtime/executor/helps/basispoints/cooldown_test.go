package basispoints

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCooldownWindowAndConcurrentRejections(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cooldown := NewCooldown(func() time.Time { return now })
	if !cooldown.PausedUntil().IsZero() {
		t.Fatal("new protocol state is paused")
	}
	var started atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Go(func() {
			until, first := cooldown.Pause()
			if !until.Equal(now.Add(30 * time.Minute)) {
				t.Errorf("wrong deadline: %v", until)
			}
			if first {
				started.Add(1)
			}
		})
	}
	wg.Wait()
	if started.Load() != 1 {
		t.Fatalf("concurrent requests started %d cooldowns", started.Load())
	}
	deadline := cooldown.PausedUntil()
	now = deadline.Add(-time.Nanosecond)
	if until, first := cooldown.Pause(); first || !until.Equal(deadline) {
		t.Fatal("an in-flight rejection prolonged the pause")
	}
	if cooldown.PausedUntil().IsZero() {
		t.Fatal("resumed before the deadline")
	}
	now = deadline
	if !cooldown.PausedUntil().IsZero() {
		t.Fatal("did not resume at the deadline")
	}
	if until, first := cooldown.Pause(); !first || !until.Equal(now.Add(30*time.Minute)) {
		t.Fatal("a new rejection after recovery did not start a new window")
	}
}
