# Contract notes

The wire contract is `docs/server-ops/lobby-api.md` in the engine repo, and it is the source of
truth: the C++ registration client and server browser were written first and this service was
written to them. This page records where fl-lobby is deliberately more tolerant than the document,
and — more importantly — **two places where the document asks for something the request does not
carry enough information to provide.**

Both gaps are recorded here rather than papered over, because v1 freezes when this service ships.

---

## Gap 1 — `passworded` is listed but never registered

`GET /v1/servers` returns a `passworded` boolean per row, and the browser reads it
(`LobbyListClient.cpp`, `json::boolField(obj, "passworded")`). It is what puts the padlock next to a
server in the list.

`POST /v1/servers` has no such field. `LobbyRegistration::buildBody()` sends `name`, `port`,
`players`, `max_players`, `mode`, `mission` and `visibility` — and nothing else.

So a lobby has no way to know whether a server is passworded, and the only honest value it can emit
is `false`. **Every passworded server currently lists as open**, and a player discovers the password
only when the join fails.

**What this service does:** accepts an optional `passworded` boolean on `POST` and echoes it on
`GET`, defaulting to `false` when absent. Nothing sends it yet, so behaviour today is unchanged —
but the field is in place, and the fix on the C++ side is one line in `buildBody()`.

## Gap 2 — the TTL depends on a number the request does not carry

The freshness rule is *"an entry is live for 2.5 × the server's heartbeat interval"*. The lobby
cannot apply that rule, because the heartbeat interval is the **server's** configuration
(`[lobby]`-side, clamped to `[5s, 300s]` in `LobbyRegistration::configure`) and it is not in the
request body.

A lobby can only assume the default 30 s, giving a 75 s TTL. That is wrong at both ends of the
clamp:

- A server configured with a 300 s heartbeat **is dropped from the listing after 75 s** and
  reappears for a moment every five minutes.
- A server configured with a 5 s heartbeat lingers for 75 s after it dies, when 12.5 s was intended.

Worse, the failure is silent and looks like a lobby bug from the operator's side.

**What this service does:** accepts an optional `heartbeat_s` integer on `POST`, clamps it to the
same `[5, 300]` range the C++ client clamps its own interval to, and derives that entry's TTL from
it. Absent — which is every client today — it falls back to the lobby's configured `-heartbeat`.
So an operator whose community runs long heartbeats can at least configure the lobby to match,
and once the client sends the field the per-entry TTL becomes correct with no lobby change.

Again the C++ side is a one-line addition to `buildBody()`.

---

## Deliberate tolerances

- **Unknown fields are ignored, not rejected.** The client's own reader is described as
  deliberately tolerant; a lobby that refused a field a newer server added would delist that server
  entirely rather than ignore one value.
- **`max_players` and `maxPlayers` are both accepted on input.** The browser reads both spellings;
  accepting both on the way in costs nothing and keeps the two directions symmetric. Output always
  uses the canonical `max_players`.
- **`DELETE` accepts the port in the body or as `?port=N`.** The contract document specifies a
  body; issue #999's checklist writes it as a query parameter. Rather than pick one and make the
  other a silent no-op, both work — the body wins when both are present.
- **Absent `visibility` is treated as public.** An explicit non-`"public"` value is refused with
  `400` rather than listed: an operator who believes their server is private and finds it listed is
  the worse of the two outcomes.
- **Negative player counts are clamped to zero** rather than refused. That is nonsense, not an
  attack.

## Deliberate strictnesses

- **The listed `host` is always the observed source address.** Any `host` or `address` field in a
  registration body is ignored. With no auth in v1 this is the only thing that makes a listing
  trustworthy, and there is a test named for it.
- **`X-Forwarded-For` is ignored unless the peer is a configured trusted proxy**, and the chain is
  then walked from the right. See the README.
- **A `DELETE` only ever matches the sender's own address**, so a third party cannot unlist a server
  by guessing its port.
- **A self-reported `heartbeat_s` is clamped**, so a registrant cannot buy itself an arbitrarily
  long TTL.
- **The `GET` body is bounded by bytes actually emitted, not by an estimated row count.** The client
  stops reading at 1 MiB and would be left parsing a severed array, so a row that would cross the
  line is dropped and the array is closed properly.

## Capacity behaviour

- A **new** registration into a full table is refused with `503` (the registrant did nothing wrong,
  and the C++ client backs off on any non-2xx). Too many registrations from one address is `429`.
- A **heartbeat for an entry that is already listed always succeeds**, even when the table is full.
  An established server does not lose its listing because the lobby filled up around it.
- Expired entries are swept before capacity is checked, so dead entries never hold slots.
