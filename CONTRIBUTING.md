# Contributing to fl-lobby

fl-lobby is the matchmaking/listing service for
[Fighters Legacy](https://github.com/fighters-legacy/fighters-legacy). The engine repo's
[CONTRIBUTING.md](https://github.com/fighters-legacy/fighters-legacy/blob/main/CONTRIBUTING.md) and
[GOVERNANCE.md](https://github.com/fighters-legacy/fighters-legacy/blob/main/GOVERNANCE.md) apply
here too; this page is the Go-specific difference.

## Before you open a PR

    gofmt -l .          # must print nothing
    go vet ./...
    go test -race ./...
    ./scripts/smoke.sh  # drives the real binary over a real socket

## The rules that CI enforces

- **Conventional-commit PR title, lowercase subject.** The title is the changelog line.
- **`Signed-off-by` on every commit** (`git commit -s`) — the DCO check is not advisory.
- **SPDX header on every new file**, `AGPL-3.0-or-later`. REUSE runs on every PR.
- **Standard library only.** CI fails if `go.sum` appears or `go.mod` grows a `require` block. This
  is a deliberate constraint, not an oversight: a lobby an operator can deploy as one static binary
  with no supply chain behind it is worth more than any convenience a dependency would buy. If you
  think you need one, open an issue and argue it first.

## Licence

AGPL-3.0-or-later, and note that this differs from the engine. fl-lobby is a network service, so
the AGPL's network clause is the point: someone running a modified lobby for other people owes
those people their changes. Anything that links the engine stays GPLv3. By contributing you agree
your work is licensed on those terms.

## Changing the wire contract

`docs/server-ops/lobby-api.md` **in the engine repo** is the source of truth for the API, not this
repo — the C++ registration client and server browser were written to it first. v1 is frozen once
this service ships, so:

- An **additive, optional** field is fine. Update the contract document in the engine repo in the
  same change, and note it in [`docs/contract-notes.md`](docs/contract-notes.md).
- A **breaking** change is a `/v2` path, not an edit to `/v1`.
- Read `docs/contract-notes.md` before assuming a gap is an oversight. Two of them are known and
  written up.
