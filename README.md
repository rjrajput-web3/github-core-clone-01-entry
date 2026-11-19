# github-core-clone-01-entry

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A self-contained local service that implements the GitHub core API surface: repository CRUD, issues, pull requests, git object operations over the smart HTTP protocol (push / clone / fetch, protocol v2 and v0), the GraphQL v4 API, and an MCP server over streamable HTTP.

## Run it

```sh
./build.sh
MAKEFUN_SHOP_TOKEN=test-token ./forge
```

---

Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards

## Layout

- `main.ts` — Node launcher: starts the forge, registers its port with the
  trial environment, and waits until the trial is done.
- `main.go` — server bootstrap and routing.
- `store.go` — the in-memory store (repositories, issues, pull requests,
  labels, comments, deterministic ordering).
- `gitobj.go` — git plumbing: SHA-1 ids, pkt-line framing, packfiles,
  trees, commits, reachability and merge-base.
- `githttp.go` — git smart HTTP: info/refs, upload-pack (v2/v0), receive-pack.
- `rest.go` — the GitHub REST v3 API.
- `graphql.go` — a hand-rolled GraphQL parser and executor.
- `mcp.go` — MCP over JSON-RPC 2.0.
- `build.sh` — builds the static `forge` binary.

## Design notes

- Everything is in memory: no disk writes, so the timed workloads stay fast.
- Ordering and timestamps come from a deterministic sequence counter, so two
  runs that receive the same requests build the same state.
- Pushes are fast-forward only, with per-ref report-status; squash merges
  use a three-way merge-base diff and refuse on conflicts.
- The viewer identity is the fixed scenario owner; bearer tokens are checked
  against `MAKEFUN_SHOP_TOKEN` (any non-empty bearer passes when it is unset,
  for local development).
