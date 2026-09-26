package basispoints

import (
	"sync"
	"time"
)

const ForbiddenCooldown = 30 * time.Minute

// SharedCooldown survives executor replacement during configuration reloads.
// It controls protocol selection, never credential availability or config values.
var SharedCooldown = NewCooldown(nil)

type Cooldown struct {
	mu    sync.Mutex
	until time.Time
	now   func() time.Time
}

func NewCooldown(now func() time.Time) *Cooldown {
	if now == nil {
		now = time.Now
	}
	return &Cooldown{now: now}
}

// Pause starts one cooldown window. Concurrent rejections from requests already
// in flight do not prolong an active window.
func (c *Cooldown) Pause() (until time.Time, started bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.until.After(now) {
		return c.until, false
	}
	c.until = now.Add(ForbiddenCooldown)
	return c.until, true
}

func (c *Cooldown) PausedUntil() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until.After(c.now()) {
		return c.until
	}
	return time.Time{}
}
