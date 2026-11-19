# github-core-clone-01-entry

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in one process: git smart HTTP (upload-pack protocol v2, receive-pack v0), the REST v3 API for repositories, issues, pull requests and contents, the GraphQL v4 API, and an MCP server — all served from a deterministic in-memory store with no UI, no outside service and no git binary. A Go binary does the serving; a small Node launcher starts it and owns the trial lifecycle (bind an ephemeral port, register it with the suite, poll for done, exit 0).

## Run it

```sh
./build.sh
node main.ts
```

`main.ts` spawns the committed `./forge` binary (building it once from source if it is missing). The forge binds port 0, POSTs `{"port": N}` to `$MAKEFUN_ENV_URL/register`, then polls `$MAKEFUN_ENV_URL/done` until the suite finishes.

## Layout

- `main.ts` — Node launcher for the forge binary.
- `main.go` — entrypoint: listener, lifecycle, routing.
- `store.go` — in-memory store with deterministic sequence-based timestamps and the REST/GraphQL views.
- `gitobj.go` — git objects (blobs, trees, commits, tags), packfile read/write with delta resolution, pkt-lines.
- `githttp.go` — git smart HTTP: v2 ls-refs/fetch (tree-only shallow closure), v0 upload-pack, receive-pack with per-ref report-status.
- `rest.go` — REST v3: repos, branches, commits, contents, labels, issues, comments, pull requests and squash merges.
- `graphql.go` — GraphQL v4: the documents `gh` and `github-mcp-server` send, plus introspection.
- `mcp.go` — MCP over Streamable HTTP: `server/discover`, `initialize`, `tools/list`, `tools/call`.
- `build.sh` — builds the statically linked `./forge` binary.

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
