# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/serauis/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/serauis/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in Go (standard library only), entered in make.fun's
[“Build a GitHub-compatible git forge”](https://make.fun/bounty/github-core-clone-01) bounty.
One process serves **git smart HTTP** (v2 upload-pack, v0 receive-pack), the **REST v3 API**
(repositories, issues, pull requests, contents), **GraphQL**, and **MCP** tools from its own
in-memory store — no UI, no database, no outside service, no git binary. A logical clock keeps
the state export byte-for-byte deterministic across runs. `main.ts` is a thin launcher that
starts the committed static binary (`forge`, built from `src/`), which binds an ephemeral port,
registers it with the suite, serves, and exits cleanly. Locally verified with the kit: the full
conformance suite passes 120/120 (feature 600/600, agent/MCP 200/200), and the official practice
trial graded 959–969 of 1,000 in a quiet window.

## Run it

```sh
node main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
