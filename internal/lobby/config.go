// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"fmt"
	"net/netip"
	"time"
)

// Contract constants from docs/server-ops/lobby-api.md in the fighters-legacy repo. They are the
// frozen v1 numbers, so they are named here rather than spelled inline at each use.
const (
	// MaxListEntries bounds GET /v1/servers. The C++ browser caps its own row count at the same
	// number, so a lobby that returned more would only be wasting bandwidth.
	MaxListEntries = 1024
	// MaxListBytes bounds the GET body. The client stops accumulating at 1 MiB and would parse a
	// truncated tail as garbage, so the lobby must not hand it one.
	MaxListBytes = 1 << 20
	// MaxStringBytes bounds every per-entry string. The client truncates at this length; doing it
	// here too means what an operator sees listed is what a client will show.
	MaxStringBytes = 256
	// MaxRequestBytes bounds a POST/DELETE body. Registration payloads are a few hundred bytes;
	// this is generous and still refuses a body meant to exhaust memory.
	MaxRequestBytes = 8 << 10

	// MinHeartbeatSeconds and MaxHeartbeatSeconds mirror the clamp the C++ registration client
	// applies to its own interval (LobbyRegistration::configure). A client cannot heartbeat outside
	// this range, so a self-reported interval outside it is not believed either.
	MinHeartbeatSeconds = 5
	MaxHeartbeatSeconds = 300
)

// Config is the whole of the service's tunable surface. Every field has a working default; a lobby
// run with no flags at all is a correct lobby.
type Config struct {
	// Addr is the listen address for the HTTP server.
	Addr string

	// Heartbeat is the interval the lobby ASSUMES a server registers on when that server does not
	// say. See TTLMultiplier.
	Heartbeat time.Duration
	// TTLMultiplier scales a heartbeat into an entry lifetime. The contract's freshness rule is
	// "live for 2.5 x the heartbeat", which tolerates one missed POST and expires on two.
	TTLMultiplier float64

	// MaxEntries caps the whole table. MaxPerHost caps how many ports one source IP may claim, so
	// a single host cannot fill the table on its own.
	MaxEntries int
	MaxPerHost int

	// WriteRate/WriteBurst limit POST and DELETE per source IP; ReadRate/ReadBurst limit GET.
	// Writes are the expensive side and the abusable one, so they get the tighter budget.
	WriteRate  float64
	WriteBurst int
	ReadRate   float64
	ReadBurst  int

	// TrustedProxies are CIDRs whose X-Forwarded-For header the lobby will believe. EMPTY BY
	// DEFAULT, and that default is the safe one: every entry is keyed on the address a client will
	// actually connect to, so believing a spoofable header by default would let anyone list a
	// server at any address they liked.
	TrustedProxies []netip.Prefix

	// PruneInterval is how often expired entries are swept in the background. Reads prune lazily
	// as well, so this only bounds how long a dead entry occupies memory on an idle lobby.
	PruneInterval time.Duration
}

// DefaultConfig returns the configuration a lobby runs with when nothing is specified.
func DefaultConfig() Config {
	return Config{
		Addr:           ":8080",
		Heartbeat:      30 * time.Second,
		TTLMultiplier:  2.5,
		MaxEntries:     MaxListEntries,
		MaxPerHost:     16,
		WriteRate:      1,
		WriteBurst:     5,
		ReadRate:       5,
		ReadBurst:      20,
		TrustedProxies: nil,
		PruneInterval:  30 * time.Second,
	}
}

// Validate rejects a configuration that would make the service behave incoherently, rather than
// letting it start and misbehave quietly.
func (c Config) Validate() error {
	switch {
	case c.Addr == "":
		return fmt.Errorf("listen address is empty")
	case c.Heartbeat < MinHeartbeatSeconds*time.Second || c.Heartbeat > MaxHeartbeatSeconds*time.Second:
		return fmt.Errorf("heartbeat %v out of range [%ds, %ds]", c.Heartbeat, MinHeartbeatSeconds, MaxHeartbeatSeconds)
	case c.TTLMultiplier < 1:
		return fmt.Errorf("ttl multiplier %v is below 1: an entry would expire before its next heartbeat", c.TTLMultiplier)
	case c.MaxEntries < 1 || c.MaxEntries > MaxListEntries:
		return fmt.Errorf("max entries %d out of range [1, %d]", c.MaxEntries, MaxListEntries)
	case c.MaxPerHost < 1:
		return fmt.Errorf("max per host %d is below 1", c.MaxPerHost)
	case c.WriteRate <= 0 || c.ReadRate <= 0:
		return fmt.Errorf("rate limits must be positive")
	case c.WriteBurst < 1 || c.ReadBurst < 1:
		return fmt.Errorf("rate limit bursts must be at least 1")
	case c.PruneInterval <= 0:
		return fmt.Errorf("prune interval must be positive")
	}
	return nil
}

// ttlFor turns a heartbeat interval into an entry lifetime.
func (c Config) ttlFor(heartbeat time.Duration) time.Duration {
	return time.Duration(float64(heartbeat) * c.TTLMultiplier)
}
