package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestExecutionSessionPoolIsolationAndEviction(t *testing.T) {
	closed := make(map[string]int)
	pool := NewExecutionSessionPool(func(id string) { closed[id]++ })
	first, releaseFirst := pool.Acquire("caller-a")
	concurrent, releaseConcurrent := pool.Acquire("caller-a")
	if first == concurrent {
		t.Fatal("active requests shared a socket")
	}
	releaseFirst()
	releaseFirst()
	reused, releaseReused := pool.Acquire("caller-a")
	if reused != first {
		t.Fatal("idle socket was not reused")
	}
	other, releaseOther := pool.Acquire("caller-b")
	if other == first || other == concurrent {
		t.Fatal("callers shared a socket")
	}
	releaseReused()
	releaseConcurrent()
	releaseOther()
	for i := 0; i < maxIdleExecutionSessions; i++ {
		_, release := pool.Acquire(fmt.Sprintf("caller-%d", i))
		release()
	}
	if len(pool.idle) != maxIdleExecutionSessions || closed[first] != 1 {
		t.Fatalf("idle=%d closes=%v", len(pool.idle), closed)
	}
	id, release := pool.Acquire("")
	release()
	if closed[id] != 1 {
		t.Fatal("unscoped connection was retained")
	}
}

func TestExecutionSessionLeaseReleasedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	released := make(chan struct{})
	var once sync.Once
	result := ReleaseSessionAfterStream(ctx, &StreamResult{Chunks: make(chan StreamChunk)}, func() { once.Do(func() { close(released) }) })
	cancel()
	for range result.Chunks {
	}
	<-released
}

func TestCodexLogicalRequestBudgetSurvivesOptionsCopy(t *testing.T) {
	opts := Options{}
	state := EnsureCodexTransport(&opts)
	state.Failures.Add(4)
	copyOpts := opts
	if got := EnsureCodexTransport(&copyOpts); got != state || got.Failures.Load() != 4 {
		t.Fatal("retry received a fresh budget")
	}
	newRequest := Options{}
	if EnsureCodexTransport(&newRequest).Failures.Load() != 0 {
		t.Fatal("next request inherited failures")
	}
}

func TestExecutionSessionPoolDrainWaitsForActiveLease(t *testing.T) {
	closed := make(map[string]bool)
	pool := NewExecutionSessionPool(func(id string) { closed[id] = true })
	idle, releaseIdle := pool.Acquire("idle")
	active, releaseActive := pool.Acquire("active")
	releaseIdle()
	pool.Drain()
	if !closed[idle] || closed[active] {
		t.Fatal("drain interrupted an active request or kept idle resources")
	}
	releaseActive()
	if !closed[active] || len(pool.idle) != 0 {
		t.Fatal("active session was retained after drain")
	}
	fresh, releaseFresh := pool.Acquire("active")
	defer releaseFresh()
	if fresh == active {
		t.Fatal("drained lease was reused")
	}
}
