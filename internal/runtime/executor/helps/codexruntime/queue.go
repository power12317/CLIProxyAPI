package codexruntime

import (
	"context"
	"sync"
)

// notificationQueue separates socket receipt and diagnostic consumption from
// business delivery. It introduces no request or event capacity limit.
type notificationQueue[T any] struct {
	mu     sync.Mutex
	items  []T
	closed bool
	wake   chan struct{}
}

func newNotificationQueue[T any]() *notificationQueue[T] {
	return &notificationQueue[T]{wake: make(chan struct{}, 1)}
}

func (q *notificationQueue[T]) push(item T) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	q.signal()
}

func (q *notificationQueue[T]) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *notificationQueue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *notificationQueue[T]) next(ctx context.Context) (T, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			var zero T
			q.items[0] = zero
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			var zero T
			return zero, false
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			var zero T
			return zero, false
		}
	}
}
