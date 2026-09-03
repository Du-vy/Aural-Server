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
	ok, _, _ := r.take()
	return ok
}

// take is allow with the numbers behind the answer: how many tokens are left,
// and how long until the next one refills.
//
// It exists for the one endpoint that has to publish its own limit rather than
// merely enforce it. A webhook is called by somebody else's software, which
// backs off by reading the headers a rejection carries, so a bare yes or no is
// not enough to answer with.
func (r *rateLimiter) take() (ok bool, remaining int, retryAfter float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.perSec
	r.last = now
	if r.tokens > r.capacity {
		r.tokens = r.capacity
	}
	if r.tokens < 1 {
		// How long the caller has to wait for the token they just asked for.
		wait := 0.0
		if r.perSec > 0 {
			wait = (1 - r.tokens) / r.perSec
		}
		return false, 0, wait
	}
	r.tokens--

	// The refill is continuous, so "when does the bucket have room again" is
	// only meaningful once it is empty; while tokens remain the answer is the
	// time to the next whole one.
	wait := 0.0
	if r.perSec > 0 && r.tokens < r.capacity {
		wait = (1 - r.tokens + float64(int(r.tokens))) / r.perSec
	}
	return true, int(r.tokens), wait
}

// spent reports whether the bucket has been idle for at least idle and has
// refilled completely, which together mean it is holding no decision. Such a
// bucket answers every question exactly as a fresh one would, so discarding it
// is free.
func (r *rateLimiter) spent(idle time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.last) < idle {
		return false
	}
	return r.tokens+time.Since(r.last).Seconds()*r.perSec >= r.capacity
}
