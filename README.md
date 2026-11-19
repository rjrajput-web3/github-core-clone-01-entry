# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in one Node.js process: git smart HTTP (upload-pack protocol v2, receive-pack v0), the REST v3 API, GraphQL v4, and an MCP server, all backed by a fast in-memory store with deterministic sequence-based timestamps.

## Run it

```sh
node main.ts
```

The forge binds an ephemeral port, registers it with the suite at `$MAKEFUN_ENV_URL`, serves until the suite reports done, then exits 0. It needs no install step, no git binary, and no outside service: only the Node.js standard library.

## Layout

- `main.ts` — entrypoint: bind, register, poll for done.
- `app.ts` — HTTP server and the REST v3 surface (repos, issues, pulls, contents, labels, branches, commits).
- `git.ts` — pkt-line framing, packfiles, and the smart-HTTP git protocol (v2 ls-refs/fetch with shallow support, v0 receive-pack with per-ref report-status).
- `gql.ts` — the GraphQL executor and the MCP endpoint (server/discover, tools/list, tools/call).
- `store.ts` — the in-memory store, git object model, and the shared pull-request helpers.

---

Entered on [make.fun](https://make.fun/?ref=rjrajput-web3&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards
