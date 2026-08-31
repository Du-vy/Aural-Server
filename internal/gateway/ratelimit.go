package gateway

import (
	"sync"
	"time"
)

// rateLimiter is a token bucket. Tokens refill continuously, so a burst is
// allowed after a quiet spell but a sustained flood is not.
//
// It is deliberately per-session rather than per-user or per-IP: the point is
// to bound what one connection can do to the server and to everybody's
// scrollback, and a session is what holds a connection.
type rateLimiter struct {
	mu       sync.Mutex
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
}

func newRateLimiter(capacity int, perSec float64) *rateLimiter {
	return &rateLimiter{
		capacity: float64(capacity),
		perSec:   perSec,
		tokens:   float64(capacity),
		last:     time.Now(),
	}
}

// allow takes one token, reporting whether there was one to take.
func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.perSec
	r.last = now
	if r.tokens > r.capacity {
		r.tokens = r.capacity
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
