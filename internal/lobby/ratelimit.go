// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"sync"
	"time"
)

// limiter is a per-key token bucket. Stdlib only, so golang.org/x/time/rate is not available and
// this is the whole of it: tokens refill at rate per second up to burst, and a request costs one.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	rate  float64
	burst float64

	// idleTTL is how long a silent key is kept before it is swept. THE SWEEP IS NOT OPTIONAL: the
	// map is keyed by client address, so without it a lobby facing the open internet accumulates a
	// bucket per address that ever touched it, and the rate limiter becomes the memory-exhaustion
	// vector it exists to prevent.
	idleTTL time.Duration

	now func() time.Time
}

type bucket struct {
	tokens   float64
	lastFill time.Time
	lastSeen time.Time
}

func newLimiter(rate float64, burst int, idleTTL time.Duration, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   float64(burst),
		idleTTL: idleTTL,
		now:     now,
	}
}

// allow spends a token for key, reporting whether the request may proceed.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		// A first-time key starts full, minus the request it is making. Starting empty would
		// refuse every client's first heartbeat, which is the one that matters most.
		l.buckets[key] = &bucket{tokens: l.burst - 1, lastFill: now, lastSeen: now}
		return true
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.lastFill = now
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets untouched for longer than idleTTL. A bucket that has had time to refill
// completely carries no state worth keeping, so dropping it is equivalent to keeping it.
func (l *limiter) sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	n := 0
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.idleTTL {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
