# Design documents

These documents say why starfix is built the way it is. The user guides in
[docs/](../../README.md#documentation) say how to use it.

| Document | Covers |
|---|---|
| [starfix.md](starfix.md) | The plan: principles, architecture, data model, bd parity, MCP tools, memory, coordination, offline use, upgrades, developer experience, cost tracking, the stages and the decisions |
| [database.md](database.md) | Why Dolt; the Postgres fallback; vector search and embeddings |
| [bearings.md](bearings.md) | Bearings, the orchestrator planned on top of starfix, for Linux and macOS (Windows through WSL2): components, defaults, plan, decisions and open questions |
| [spike/dolt/RESULTS.md](../../spike/dolt/RESULTS.md) | Stage 0: how Dolt behaves under concurrent writers, and the rules starfixd follows because of it |

## How the documents change

- **The plan stays a plan.** Where the build departs from a design, the
  section gets a dated **As built** note instead of a rewrite, so the
  reasoning stays readable ([AGENTS.md](../../AGENTS.md#rules), rule 17).
- **The status lives in the README.** The [roadmap](../../README.md#roadmap)
  says which stage is in progress; `starfix.md` §13 holds the plan behind
  it.
- **Decisions are numbered.** `starfix.md` §14 and `bearings.md` §7 list
  them, and code comments cite them, for example "decision D2".
