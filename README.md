# github-core-clone-01-entry

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/vinzagent/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/vinzagent/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in a single dependency-free Node process: git smart HTTP (protocol v0 and v2, hand-rolled packfiles, deltas and shallow fetch), the REST v3 API (repositories, contents, branches, commits, labels, issues, comments, pull requests and squash merges), the GraphQL v4 documents gh sends (repository and issue reads, connections with cursors, the create/close/merge mutations, __type introspection, node(id:) and batched operations) and an MCP endpoint speaking both server/discover (2026-07-28) and the initialize handshake. One in-memory store backs every surface, ids and timestamps come from deterministic counters, and the whole state a trial creates lives and dies with the process.

## Run it

```sh
MAKEFUN_SHOP_TOKEN=<token> node --experimental-strip-types main.ts
```

With `$MAKEFUN_ENV_URL` set the process registers its port with the env and exits when the env says the trial is done; without it, it just serves (on `$PORT` when set, else an ephemeral port).

---

<sub>Entered on [make.fun](https://make.fun/?ref=wsvx4819&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
