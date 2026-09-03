# Security policy

## Reporting

Report a vulnerability privately through
[GitHub Security Advisories](https://github.com/fighters-legacy/fl-lobby/security/advisories/new),
not as a public issue.

## What fl-lobby does and does not defend

**There is no authentication in v1.** This is by design: a lobby is a public listing board, and
anyone who can reach it can register a server. The threat model is therefore narrow, and what the
service does guarantee is worth stating precisely.

**It does defend:**

- **Listing integrity.** An entry is keyed and listed at the address the lobby *observed*, never one
  the registrant claimed, so no one can list a server at someone else's address. `X-Forwarded-For`
  is believed only from a configured trusted proxy, and the chain is walked from the right.
- **Deregistration.** A `DELETE` only matches the sender's own address, so a third party cannot
  unlist a server by guessing its port.
- **Availability, within reason.** Per-address rate limits on reads and writes, a per-address entry
  cap, a total entry cap, a bounded request body, a bounded response body, and server-side
  read/write/idle timeouts. Rate-limiter state is swept, so the limiter cannot itself be grown into
  a memory-exhaustion vector.

**It does not defend:**

- **Truthfulness of a listing.** A registrant may claim any name, player count, mode or mission.
  Nothing verifies these, and nothing can in a design with no authentication. A client should treat
  every listed field except `host`/`port` as untrusted display text.
- **Registration of servers that do not exist.** Anyone may register any port at their own address.
  The caps bound how many.
- **Confidentiality.** Run it behind TLS if you care; the service speaks plain HTTP and expects a
  reverse proxy to terminate. If you do that, set `-trusted-proxies` — see the README.

If you find something that breaks one of the guarantees in the first list, that is a vulnerability.
If it is in the second list, it is the documented posture — but an issue arguing the posture is
wrong is still welcome.
