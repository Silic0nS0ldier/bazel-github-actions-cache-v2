package server

import (
	"context"
	"sync"
	"time"
)

// intervalLimiter spaces reservations evenly to stay below GitHub's documented
// 200 cache uploads/minute limit. It deliberately has no burst allowance.
type intervalLimiter struct {
	mu       sync.Mutex
	next     time.Time
	last     time.Time
	interval time.Duration
}

func newIntervalLimiter(perMinute int) *intervalLimiter {
	if perMinute <= 0 {
		return &intervalLimiter{}
	}
	return &intervalLimiter{interval: time.Minute / time.Duration(perMinute)}
}

func (l *intervalLimiter) wait(ctx context.Context) (bool, error) {
	if l.interval == 0 {
		return false, nil
	}
	l.mu.Lock()
	now := time.Now()
	slot := now
	if l.next.After(now) {
		slot = l.next
	}
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		return false, nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}

func (l *intervalLimiter) claimStart() time.Duration {
	if l.interval == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	eligible := l.last.Add(l.interval)
	if !l.last.IsZero() && now.Before(eligible) {
		return time.Until(eligible)
	}
	l.last = now
	return 0
}
