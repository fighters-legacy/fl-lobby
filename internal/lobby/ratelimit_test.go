// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"testing"
	"time"
)

func TestLimiterBurstThenRefill(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter(1, 3, time.Minute, clk.Now)

	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	if l.allow("a") {
		t.Fatal("the burst did not run out")
	}

	clk.Advance(time.Second)
	if !l.allow("a") {
		t.Fatal("one second at 1/s did not refill a token")
	}
	if l.allow("a") {
		t.Fatal("one second refilled more than one token")
	}
}

func TestLimiterDoesNotAccumulateBeyondBurst(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter(1, 3, time.Minute, clk.Now)

	// A client silent for an hour gets a full bucket, not an hour's worth of credit.
	clk.Advance(time.Hour)
	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatalf("request %d refused, want the full burst", i)
		}
	}
	if l.allow("a") {
		t.Fatal("tokens accumulated past the burst ceiling")
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter(1, 1, time.Minute, clk.Now)

	if !l.allow("a") || !l.allow("b") {
		t.Fatal("two different keys shared one budget")
	}
	if l.allow("a") {
		t.Fatal("key a was not limited")
	}
}

func TestLimiterSweepsIdleBuckets(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter(1, 3, time.Minute, clk.Now)

	// THE POINT OF THIS TEST: the bucket map is keyed by client address. Without the sweep, a
	// lobby on the open internet grows one bucket per address that ever touched it, and the rate
	// limiter becomes the memory-exhaustion vector it exists to prevent.
	for i := 0; i < 500; i++ {
		l.allow("client-" + itoa(i))
	}
	if got := l.size(); got != 500 {
		t.Fatalf("buckets=%d, want 500", got)
	}

	clk.Advance(2 * time.Minute)
	l.allow("still-here")
	if got := l.sweep(); got != 500 {
		t.Fatalf("swept %d, want 500", got)
	}
	if got := l.size(); got != 1 {
		t.Fatalf("buckets=%d after sweep, want 1 (the active one survives)", got)
	}
}

func TestServiceSweepClearsExpiredEntries(t *testing.T) {
	svc, clk := newTestService(t, nil)
	do(t, svc, "POST", "/v1/servers", "203.0.113.7:51000", cppClientBody)

	clk.Advance(76 * time.Second)
	svc.Sweep()
	// Sweep must actually free the memory, not merely hide the entry from GET -- reads prune
	// lazily anyway, so a broken Sweep would be invisible without checking the table itself.
	if got := svc.Store().Len(); got != 0 {
		t.Fatalf("entries=%d after Sweep, want 0", got)
	}
}
