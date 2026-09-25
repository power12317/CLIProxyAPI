package logging

import (
	"encoding/hex"
	"sync"
	"testing"
)

func TestGenerateRequestID_Concurrency(t *testing.T) {
	const total = 1000
	var wg sync.WaitGroup
	ids := make(chan string, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ids <- GenerateRequestID() }()
	}
	wg.Wait()
	close(ids)
	count := 0
	for id := range ids {
		if len(id) != 8 {
			t.Fatalf("expected 8-character request ID, got %q", id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("invalid hexadecimal request ID %q: %v", id, err)
		}
		count++
	}
	if count != total {
		t.Fatalf("expected %d IDs, got %d", total, count)
	}
}
