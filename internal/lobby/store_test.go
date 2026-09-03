// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testEntry(host string, port uint16) Entry {
	return Entry{Name: "srv", Host: host, Port: port, Mode: "builtin:tdm", MaxPlayers: 16}
}

func TestUpsertCreatesThenRefreshes(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(10, 4, clk.Now)

	created, err := s.Upsert(testEntry("198.51.100.1", 4778), time.Minute)
	if err != nil || !created {
		t.Fatalf("first registration: created=%v err=%v, want true/nil", created, err)
	}
	created, err = s.Upsert(testEntry("198.51.100.1", 4778), time.Minute)
	if err != nil || created {
		t.Fatalf("heartbeat: created=%v err=%v, want false/nil", created, err)
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("entries=%d, want 1: a heartbeat must upsert, not duplicate", got)
	}
}

func TestKeyIsHostAndPort(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(10, 4, clk.Now)

	// Same host, different ports: two servers on one machine, which is legitimate.
	mustUpsert(t, s, testEntry("198.51.100.1", 4778), time.Minute)
	mustUpsert(t, s, testEntry("198.51.100.1", 4779), time.Minute)
	// Same port, different hosts: unrelated servers that must not collide.
	mustUpsert(t, s, testEntry("198.51.100.2", 4778), time.Minute)

	if got := s.Len(); got != 3 {
		t.Fatalf("entries=%d, want 3", got)
	}
}

func TestTTLExpiry(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(10, 4, clk.Now)
	ttl := 75 * time.Second // the contract default: 2.5 x 30s

	mustUpsert(t, s, testEntry("198.51.100.1", 4778), ttl)

	// Still live one tick before the deadline, and exactly at it.
	clk.Advance(ttl - time.Second)
	if got := len(s.List(-1)); got != 1 {
		t.Fatalf("before deadline: entries=%d, want 1", got)
	}
	clk.Advance(time.Second)
	if got := len(s.List(-1)); got != 1 {
		t.Fatalf("at deadline: entries=%d, want 1 (an entry is live up to its deadline)", got)
	}
	// Gone after it.
	clk.Advance(time.Nanosecond)
	if got := len(s.List(-1)); got != 0 {
		t.Fatalf("past deadline: entries=%d, want 0", got)
	}
}

func TestHeartbeatExtendsLifetime(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(10, 4, clk.Now)
	ttl := 75 * time.Second

	mustUpsert(t, s, testEntry("198.51.100.1", 4778), ttl)
	clk.Advance(60 * time.Second)
	mustUpsert(t, s, testEntry("198.51.100.1", 4778), ttl)
	clk.Advance(60 * time.Second)

	if got := len(s.List(-1)); got != 1 {
		t.Fatalf("entries=%d, want 1: a heartbeat inside the TTL must extend it", got)
	}
}

func TestDelete(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(10, 4, clk.Now)
	mustUpsert(t, s, testEntry("198.51.100.1", 4778), time.Minute)

	if !s.Delete("198.51.100.1", 4778) {
		t.Fatal("Delete reported nothing removed, want true")
	}
	if s.Delete("198.51.100.1", 4778) {
		t.Fatal("second Delete reported a removal, want false")
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("entries=%d, want 0", got)
	}
}

func TestCapacityRefusals(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(2, 1, clk.Now)

	mustUpsert(t, s, testEntry("198.51.100.1", 4778), time.Minute)
	// Second port from the same host trips the per-host cap before the table cap.
	if _, err := s.Upsert(testEntry("198.51.100.1", 4779), time.Minute); err != ErrHostFull {
		t.Fatalf("per-host cap: err=%v, want ErrHostFull", err)
	}
	mustUpsert(t, s, testEntry("198.51.100.2", 4778), time.Minute)
	if _, err := s.Upsert(testEntry("198.51.100.3", 4778), time.Minute); err != ErrLobbyFull {
		t.Fatalf("table cap: err=%v, want ErrLobbyFull", err)
	}
}

func TestFullLobbyStillAcceptsAHeartbeat(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(1, 4, clk.Now)
	mustUpsert(t, s, testEntry("198.51.100.1", 4778), time.Minute)

	// A server already listed must keep its listing when the lobby fills up around it. Refusing
	// its heartbeat would expire an established server in favour of nobody.
	if _, err := s.Upsert(testEntry("198.51.100.1", 4778), time.Minute); err != nil {
		t.Fatalf("heartbeat into a full lobby: err=%v, want nil", err)
	}
}

func TestExpiredEntriesFreeCapacity(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(1, 4, clk.Now)
	mustUpsert(t, s, testEntry("198.51.100.1", 4778), 10*time.Second)

	clk.Advance(11 * time.Second)
	// The dead entry must not hold a slot: registration sweeps before it checks capacity.
	if _, err := s.Upsert(testEntry("198.51.100.2", 4778), 10*time.Second); err != nil {
		t.Fatalf("registration after expiry: err=%v, want nil", err)
	}
}

func TestListIsBoundedAndOrdered(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(100, 100, clk.Now)
	for i := 0; i < 10; i++ {
		mustUpsert(t, s, testEntry("198.51.100.1", uint16(5000+i)), time.Minute)
	}
	got := s.List(4)
	if len(got) != 4 {
		t.Fatalf("List(4) returned %d rows, want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Port > got[i].Port {
			t.Fatalf("rows are not in a stable order: %v then %v", got[i-1].Port, got[i].Port)
		}
	}
}

func TestStringsAreTruncatedOnRuneBoundaries(t *testing.T) {
	clk := newFakeClock()
	s := NewStore(10, 4, clk.Now)

	e := testEntry("198.51.100.1", 4778)
	// Multi-byte runes straddling the cap: a byte-wise cut would leave a partial rune, which
	// encoding/json rewrites to U+FFFD and the player sees as a mangled name.
	e.Name = strings.Repeat("é", MaxStringBytes) // 2 bytes each
	mustUpsert(t, s, e, time.Minute)

	got := s.List(-1)[0].Name
	if len(got) > MaxStringBytes {
		t.Fatalf("name is %d bytes, want <= %d", len(got), MaxStringBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune and produced invalid UTF-8")
	}
}

func mustUpsert(t *testing.T, s *Store, e Entry, ttl time.Duration) {
	t.Helper()
	if _, err := s.Upsert(e, ttl); err != nil {
		t.Fatalf("Upsert(%s:%d): %v", e.Host, e.Port, err)
	}
}
