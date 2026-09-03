// SPDX-FileCopyrightText: 2026 MKZ Systems LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package lobby

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP resolves the address an entry is keyed on and listed at.
//
// THIS IS THE SECURITY-LOAD-BEARING FUNCTION IN THE SERVICE. The contract has no authentication:
// the only thing stopping one host from listing a server at another host's address is that the
// lobby uses the address it observed rather than one it was told. X-Forwarded-For is a header any
// client can write, so it is believed ONLY when the immediate peer is a configured trusted proxy.
// With no proxies configured -- the default -- the header is ignored entirely.
func clientIP(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer, ok := parseHostPort(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if len(trusted) == 0 || !inPrefixes(peer, trusted) {
		return peer, true
	}

	// The peer is a trusted proxy, so walk X-Forwarded-For from the RIGHT: entries are appended in
	// order, so the rightmost ones are the closest hops and the ones a spoofer cannot control. The
	// first address that is not itself a trusted proxy is the real client. Anything to the left of
	// it was written by something we do not trust and is discarded.
	forwarded := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, v := range forwarded {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			// A malformed hop means the chain cannot be trusted past this point. Fall back to the
			// peer rather than guessing.
			break
		}
		addr = addr.Unmap()
		if !inPrefixes(addr, trusted) {
			return addr, true
		}
	}
	return peer, true
}

func parseHostPort(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		// Not every transport hands us host:port (a unix socket, a test harness). Try the whole
		// string as a bare address before giving up.
		if addr, perr := netip.ParseAddr(remote); perr == nil {
			return addr.Unmap(), true
		}
		return netip.Addr{}, false
	}
	// A scoped literal ("fe80::1%eth0") parses with a zone; the zone is not part of the identity a
	// client would connect to, so it is dropped.
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func inPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
