package http

import (
	"math"
	"sync"
	"time"
)

// RateLimit is a token bucket setting: Rate tokens per second, up to Burst.
type RateLimit struct {
	Rate  float64
	Burst float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter keeps one bucket per source name.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}, now: time.Now} }

// allow takes a token for key, or says how long until the next one.
func (l *limiter) allow(key string, rl RateLimit) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 10000 { // fallback sources could spray names, keep memory bounded
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: rl.Burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(rl.Burst, b.tokens+now.Sub(b.last).Seconds()*rl.Rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rl.Rate * float64(time.Second))
	return false, wait
}
