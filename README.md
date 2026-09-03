# fl-lobby

The self-hostable matchmaking and server-listing service for
[Fighters Legacy](https://github.com/fighters-legacy/fighters-legacy).

A dedicated server registers itself here on a heartbeat; the in-game server browser lists what is
registered. That is the whole job. One static binary, no database, no accounts, no external
dependencies — the service is written against the Go standard library alone.

**Hosting is self-host only and federated.** There is no central registry and no "official" lobby.
Anyone may run one, and players opt in by adding its URL to `[client] lobby_urls` in their game
config. LAN discovery works with no lobby at all.

## Running one

    go build ./cmd/fl-lobby
    ./fl-lobby -addr :8080

Or with the container image:

    podman run -p 8080:8080 ghcr.io/fighters-legacy/fl-lobby:latest

Then point a dedicated server at it, in `server.toml`:

    [lobby]
    register   = true
    url        = "https://lobby.example.org"
    visibility = "public"

and a player at it, in their client config:

    [client]
    lobby_urls = "https://lobby.example.org"

## Configuration

Every option has a working default; a lobby run with no flags is a correct lobby. Any flag may also
be given as an environment variable — `FL_LOBBY_` plus the flag name uppercased with `-` as `_`, so
`-max-per-host` is `FL_LOBBY_MAX_PER_HOST`. An explicit flag beats the environment.

| Flag | Default | What it does |
|---|---|---|
| `-addr` | `:8080` | Listen address. |
| `-heartbeat` | `30s` | The registration interval the lobby **assumes** when a server does not state one. |
| `-ttl-multiplier` | `2.5` | Entry lifetime as a multiple of the heartbeat. The default tolerates one missed heartbeat and expires on two. |
| `-max-entries` | `1024` | Total listed servers. The contract caps a listing at 1024, so raising this achieves nothing. |
| `-max-per-host` | `16` | Listed servers from one source address, so one host cannot fill the table. |
| `-write-rate` / `-write-burst` | `1/s`, `5` | Per-address budget for `POST` and `DELETE`. |
| `-read-rate` / `-read-burst` | `5/s`, `20` | Per-address budget for `GET`. |
| `-trusted-proxies` | *(empty)* | CIDRs whose `X-Forwarded-For` is believed. **See below.** |
| `-prune-interval` | `30s` | Background sweep for expired entries. |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error`. |

### Running behind a reverse proxy

An entry is keyed and listed at **the address the lobby observed the registration coming from**,
never one the registrant claimed. With no authentication in v1, that is the only thing stopping
anyone listing a server at someone else's address.

`X-Forwarded-For` is a header any client can write, so it is ignored unless the immediate peer is
inside `-trusted-proxies`. If you terminate TLS in nginx or Caddy on the same host, you need:

    ./fl-lobby -trusted-proxies 127.0.0.1/32,::1/128

**Without it every server registers as `127.0.0.1` and nothing in your lobby is joinable.** With a
proxy configured, the forwarded chain is walked from the right and the first non-proxy hop wins, so
a spoofed value prepended by a client is discarded.

## API

The wire contract is
[`docs/server-ops/lobby-api.md`](https://github.com/fighters-legacy/fighters-legacy/blob/main/docs/server-ops/lobby-api.md)
in the engine repo — that document is the source of truth, and the C++ halves
(`engine/net/LobbyRegistration`, `engine/net/LobbyListClient`) already speak it.

| Endpoint | Purpose |
|---|---|
| `POST /v1/servers` | Register / heartbeat. Upsert keyed on (source IP, advertised port). `201` first time, `200` after. |
| `DELETE /v1/servers` | Deregister. Port in the body or as `?port=N`. Always `204`; a missing entry is not an error. |
| `GET /v1/servers` | List live servers as a JSON array. |
| `GET /healthz` | Liveness, for a container runtime. Not part of the versioned contract. |

Where this service is deliberately more tolerant or more careful than the document requires, and
the two places where the document leaves a gap, are written up in
[`docs/contract-notes.md`](docs/contract-notes.md).

## Development

    go build ./...
    go vet ./...
    go test -race ./...

No external modules, and it should stay that way: a lobby an operator can deploy as one static
binary with no supply chain behind it is worth more than any convenience a dependency would buy.

Contributions need a `Signed-off-by` line (`git commit -s`) and a conventional-commit PR title —
see [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

AGPL-3.0-or-later. fl-lobby is a network service, so the AGPL's network clause is the point: if you
run a modified lobby for others, they are entitled to your changes. Anything that links the engine
stays GPLv3.
