// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"errors"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// Errors a registration can fail with. Both are capacity refusals, kept distinct because they mean
// different things to an operator: one lobby is full, one host is greedy.
var (
	ErrLobbyFull = errors.New("lobby is at capacity")
	ErrHostFull  = errors.New("host has registered too many servers")
)

// Entry is one listed server. Host is always the address the lobby OBSERVED the registration
// coming from, never anything the server claimed -- that is what makes a listing joinable and what
// stops a registrant listing someone else's address.
type Entry struct {
	Name       string
	Host       string
	Port       uint16
	Mode       string
	Mission    string
	Players    int
	MaxPlayers int
	Passworded bool

	expiresAt time.Time
}

type key struct {
	host string
	port uint16
}

// Store is the whole of the lobby's state: an in-memory table of live servers. Nothing is
// persisted, deliberately -- a lobby that restarts is repopulated by the next heartbeat from every
// server that still wants to be listed, which is at most one heartbeat of missing listings and
// needs no durability story at all.
type Store struct {
	mu      sync.Mutex
	entries map[key]Entry

	maxEntries int
	maxPerHost int

	// now is the clock, injectable so the TTL tests do not sleep.
	now func() time.Time
}

// NewStore builds an empty store.
func NewStore(maxEntries, maxPerHost int, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		entries:    make(map[key]Entry),
		maxEntries: maxEntries,
		maxPerHost: maxPerHost,
		now:        now,
	}
}

// Upsert registers or refreshes an entry, returning true when this created a new listing (which is
// what separates a 201 from a 200). Expired entries are swept first, so a lobby at capacity with
// dead entries still accepts a new registration.
func (s *Store) Upsert(e Entry, ttl time.Duration) (created bool, err error) {
	e.Name = truncate(e.Name, MaxStringBytes)
	e.Mode = truncate(e.Mode, MaxStringBytes)
	e.Mission = truncate(e.Mission, MaxStringBytes)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneLocked(now)

	k := key{host: e.Host, port: e.Port}
	_, exists := s.entries[k]

	if !exists {
		// Capacity is only ever checked for a NEW listing. A refresh of an entry that is already
		// in the table must always succeed: an established server does not get dropped because
		// the lobby filled up around it.
		if len(s.entries) >= s.maxEntries {
			return false, ErrLobbyFull
		}
		if s.countHostLocked(e.Host) >= s.maxPerHost {
			return false, ErrHostFull
		}
	}

	e.expiresAt = now.Add(ttl)
	s.entries[k] = e
	return !exists, nil
}

// Delete drops one entry. It answers whether anything was there, but a caller should not turn a
// false into an error: the contract says a missing entry is not a failure, and a DELETE that
// arrives just after the TTL swept the entry is the normal case, not a fault.
func (s *Store) Delete(host string, port uint16) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key{host: host, port: port}
	if _, ok := s.entries[k]; !ok {
		return false
	}
	delete(s.entries, k)
	return true
}

// List returns the live entries, oldest-expiring first so the order is stable and independent of
// Go's map iteration. At most limit rows come back.
func (s *Store) List(limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(s.now())

	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	// A deterministic order matters more than which order: a browser that re-sorts anyway should
	// still see a list that does not shuffle between refreshes for no reason.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Port < out[j].Port
	})

	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Prune sweeps expired entries and reports how many went. Called on a ticker; reads prune anyway.
func (s *Store) Prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(s.now())
}

// Len is the current live count, after a sweep.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	return len(s.entries)
}

func (s *Store) pruneLocked(now time.Time) int {
	n := 0
	for k, e := range s.entries {
		// Expiry is a strict comparison against the deadline: an entry is live right up to it.
		if now.After(e.expiresAt) {
			delete(s.entries, k)
			n++
		}
	}
	return n
}

func (s *Store) countHostLocked(host string) int {
	n := 0
	for k := range s.entries {
		if k.host == host {
			n++
		}
	}
	return n
}

// truncate cuts a string to at most n bytes WITHOUT splitting a rune. A byte-wise cut through a
// multi-byte character would emit invalid UTF-8, which encoding/json then rewrites to U+FFFD --
// so a server named in Cyrillic or Japanese would list with a mangled tail rather than a short one.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
