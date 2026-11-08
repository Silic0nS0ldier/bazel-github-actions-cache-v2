package server

import (
	"context"
	"sync"
	"time"
)

// intervalLimiter spaces upload starts evenly to stay below GitHub's documented
// 200 cache uploads/minute limit. It deliberately has no burst allowance.
type intervalLimiter struct {
	mu       sync.Mutex
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
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return waited, err
		}
		delay := l.claimStart()
		if delay <= 0 {
			return waited, nil
		}
		waited = true
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true, ctx.Err()
		case <-timer.C:
		}
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
