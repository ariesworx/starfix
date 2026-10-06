# starfix design (draft 1)

6 Oct 2026. Status: accepted draft. Based on a survey of bd 1.2.2 (source, docs, changelog, open issues and PRs) and [database.md](database.md).

starfix is an issue tracker, shared memory and coordination layer for AI coding agents working across sessions and machines. It is one Go module and two binaries:

| Binary | Runs | Role |
|---|---|---|
| `starfix` | each developer machine | MCP server for agents (stdio), admin CLI for people, local cache and op log |
| `starfixd` | the shared server | the single authority: claims, leases, events, memory, sync |

It replaces bd and needs neither bd nor beads-remote. beads-remote's SSH, pinning and provisioning code moves in.

## 1. Principles

1. **MCP for agents, CLI for people.** An agent never needs a shell, and an admin never needs MCP.
2. **One authority.** There is no multi-master database merge. Clients send operations; `starfixd` orders them.
3. **The operation log is the truth.** Ready, blocked and progress are derived at read time and never stored.
4. **Every claim is fenced.** A claim returns a lease and an epoch. Expiry runs on the server clock, and a write carrying a stale epoch is refused.
5. **The harness does the chores.** The client renews leases and records session identity. The model never heartbeats.
6. **Fail closed and loud.** There is no silent empty database, no swallowed refusal, and exit codes are typed.
7. **Context is a budget.** Every agent-facing result has a compact default and a token cap.
8. **Non-invasive.** No git hooks, no data in the working tree, and one store per machine shared by all worktrees.
9. **Compatible one version back and one forward,** so a fleet on mixed versions keeps working.
10. **Nothing org-specific in the public repo.**

## 2. Architecture

```text
agent ──MCP stdio──> starfix ─────────────────┐
person ──CLI──────> (same binary)             │ compact RPC (JSON over HTTP/2)
                     ├ SQLite (WAL): read cache, op log, outbox
                     └ in-process SSH, host key pinned ═══ ssh:22 ═══> starfixd
                                                                         ├ leases, reaper (server clock)
                                                                         ├ event stream (SSE) and inboxes
                                                                         ├ gate runner, GitHub webhooks
                                                                         └ dolt sql-server (loopback)
```

**Transport.** `golang.org/x/crypto/ssh` runs inside the process: no system `ssh` and no port juggling. The developer's SSH key is their identity, and `starfixd` maps each key fingerprint to a principal, so shared Unix accounts no longer hide who did what.

**Database.** Dolt, behind `starfixd`; nothing else talks to it. Postgres is the fallback behind the same interface (see database-choice.md). Dolt's own history serves as a second audit trail, with `AS OF` queries available to admins.

**Store discovery.** A committed `.starfix.yaml` carries `project: <uuid>`, the server address and the pinned host key. It holds no secrets. The local store is `~/.local/share/starfix/<uuid>.db`, so every worktree and clone of a project on one machine shares it.

## 3. Data model

Each table has one copy. bd's mirror tables for ephemeral issues are replaced by a flag.

| Table | Key columns | Notes |
|---|---|---|
| `issues` | `id`, `project`, `parent_id`, `title`, `body`, `design`, `acceptance`, `notes`, `status`, `priority`, `type`, `assignee`, `owner`, `due_at`, `defer_until`, `ephemeral`, `expires_at`, `pinned`, `template`, `metadata` JSON, `rev` | `rev` increments on every change, and `write_id` takes a value unique to each write; together they make a compare-and-swap that Dolt actually enforces (stage 0). No stored `is_blocked`. No orchestrator columns. |
| `deps` | (`from`, `to`, `type`), `metadata` JSON | Keying on all three allows several edge types between the same two issues, which bd forbids. `to` may be another project's issue or capability. |
| `labels` | (`issue`, `label`) | `dim:value` labels are state dimensions. |
| `comments` | `id`, `issue`, `author`, `session`, `body`, `at` | Append-only, so concurrent writes never conflict. |
| `claims` | `issue`, `holder` (principal, session, machine), `epoch`, `expires_at`, `provisional` | One row per issue; the epoch only ever increases. |
| `agents` | `principal`, `session`, `machine`, `harness`, `started`, `last_seen`, `labels` | A live registry, so `who` shows everyone at work. |
| `memories` | `id`, `scope` (`team` / `user:<p>` / `project`), `key`, `body`, `tags[]`, `issue`, `author`, `pinned`, `created`, `updated`, `rev` | See §6. |
| `events` | `seq` (gapless), `at` (server), `hlc`, `principal`, `session`, `machine`, `op`, `target`, `before`, `after`, `idem_key` | Always on and append-only. Drives history, sync and notifications. |
| `inbox` | `id`, `to`, `kind`, `ref`, `body`, `read_at` | Covers mentions, assignments, revoked claims, gates that opened and handoffs. |
| `locks` | `name`, `holder`, `epoch`, `expires_at`, `capacity`, `queue[]` | A mutex or a semaphore, with a first-in-first-out queue. |
| `reservations` | `path_glob`, `holder`, `issue`, `expires_at`, `mode` (exclusive/shared) | File and path reservations. |
| `gates` | `id`, `kind` (human/timer/gh/issue/capability), `spec`, `state` | Evaluated on the server, not polled by clients. |
| `handoffs` | `issue`, `from`, `state`, `next`, `branch`, `worktree`, `at` | Structured handoff record. |
| `embeddings` | (`kind`, `id`, `model`), `vec`, `hash` | Optional; see §10. |
| `settings` | `key`, `value`, `source` | One table, typed keys, `show` reports where each value came from. Custom statuses carry categories. |
| `compactions` | `issue`, `level`, `original` JSON | Summarizing an issue never destroys the original, which can be restored. |

**IDs.** `<prefix>-<base32 of 40 random bits>`, generated by the client so they are safe to create offline, and shortened for display to the shortest unique prefix. Children get their own ID plus `parent_id`; a dotted `.N` form is display-only, which avoids bd's collisions on child numbers. bd IDs survive import unchanged.

## 4. bd feature parity

You asked to port every feature. Every bd feature is listed here as **Port** (same behavior), **Redesign** (same outcome, a better mechanism) or **Replace** (the problem it solved is solved differently). Nothing is listed as dropped. The four Replace rows are the only places starfix has no equivalent command; §12 asks you to confirm them.

| bd feature | starfix | How |
|---|---|---|
| Issue fields, types, statuses, custom statuses/types | Port | Status categories kept; custom values live in `settings` (one source, not three) |
| Hash IDs, adaptive length, counter mode | Redesign | Random client IDs plus a shortest-unique display form; counter mode kept as an option |
| Labels, `label-any`, state dimensions (`set-state`, `state`) | Redesign | A state change is a label patch plus an event, not a child issue, in one transaction |
| Dependencies (19 types), `dep tree/cycles/relate/link`, bulk add | Port | Several edge types per pair; cycle check covers `waits-for` too |
| `conditional-blocks`, `waits-for` gates | Redesign | Implemented as documented: conditional runs only if the target closed as failed |
| `ready`, `blocked`, `--explain`, sorts, `--claim` | Redesign | Recursive CTE at read time; reads never write; `ready --claim` is atomic |
| Claims, `--if-*` guards | Redesign | Compare-and-swap on `rev` always; fenced claim epoch |
| Leases, heartbeat, `reclaim` (1.3) | Redesign | Client renews automatically; server reaper; expired claims become *stalled* with a recovery packet |
| Epics, children, `epic status/close-eligible`, `graph`, mermaid/dot | Port | `parent_id` column; graph output in mermaid, dot and text |
| `duplicate`, `supersede`, `duplicates --auto-merge`, `find-duplicates` | Port | One transaction each; plus a duplicate check at create (§7) |
| `defer`/`undefer` | Redesign | `defer_until` evaluated at read time; nothing is woken by writing |
| `todo`, `q`, `note`, `human` | Port | `human` becomes an inbox item addressed to people |
| `query` language, `search`, `count`, `stale`, `orphans` | Port | `query` compiles fully to SQL, so paging works; `search` adds full-text, and vectors later |
| `lint`, `--validate` | Port | Rules come from settings, not code |
| `history`, `diff` | Redesign | From `events`, fast, per issue or per field |
| `vc`, `branch`, `backup`, `compact` (Dolt), `flatten`, `gc` | Replace | The server owns history and backups (server-side backups); no client can rewrite it |
| Semantic compaction, `restore` | Port | Non-destructive: original kept in `compactions` |
| `prune`, `purge`, `delete`, `rename`, `rename-prefix` | Port | Admin CLI only; each one transaction; delete is a tombstone plus an admin purge |
| JSONL import/export | Port | bd-compatible both ways, so migration is `starfix import < bd.jsonl` |
| `batch` | Port | MCP `batch` tool and CLI; one transaction, one round trip |
| `kv`, `config`, `metadata` | Port | `settings` table plus issue `metadata` JSON |
| Memories (`remember`, `recall`, `forget`, `memories`) | Redesign | Scoped, tagged, authored records (§6) |
| `prime`, `onboard`, `quickstart`, `setup <agent>`, `rules`, `preflight` | Redesign | MCP `instructions` plus a capped `prime` resource; `starfix setup <agent>` (§5) |
| Git hooks (`bd hooks`) | Replace | No hooks. An optional `prepare-commit-msg` snippet that adds a `Starfix:` trailer, installed only on request |
| Merge slot | Redesign | General leased mutex/semaphore with FIFO handoff |
| Gates (human, timer, gh, bead) | Redesign | Server gate runner; GitHub by webhook; cross-project works |
| Formulas, cook, pour, molecules, bond, protos, `mol *` | Port | As templates of issue graphs; the steps map to PRC procedures |
| Wisps | Redesign | `ephemeral` flag plus `expires_at`, purged by the server |
| Swarm validate/status | Port | Waves and maximum parallelism from the dependency graph |
| Mail (delegated to `gt`) | Redesign | Built-in inbox with push |
| Sessions (`closed_by_session`) | Redesign | Session on every event and every claim |
| Routing, planning DBs, hydration (`repo add/sync`) | Replace | Projects on one server; cross-project edges are rows; a personal scope instead of a planning DB |
| `ship`, `external:` deps | Redesign | Capability rows on the server, with a notification when one ships |
| Federation, `bd sync`, `dolt push/pull` | Replace | One authority plus offline op log (§8) |
| Audit, journal, provenance | Port | One `events` table, always on; provenance links kept |
| Jira, Linear, ADO, Notion, GitLab sync | Port, later | One sync engine; GitHub first |
| `doctor`, `recompute-blocked`, repair migrations | Replace | Nothing derived is stored, so nothing drifts; `starfix check` verifies connection and schema |
| OTEL metrics | Port, later | |

## 5. Agent interface (MCP)

**Interoperability.** Claude Code, Codex, Gemini CLI, Cursor, VS Code Copilot, Junie, Amp and Claude Desktop all speak MCP over stdio. One server serves them all. Agents that only read instruction files (Aider, Windsurf, Kilo, Kiro, Cody, OpenCode, Factory) get a pointer line, plus the CLI in read-only mode.

`starfix setup <agent> [--check|--remove]` writes three things:
- the agent's MCP config;
- a marker-delimited pointer in AGENTS.md, CLAUDE.md, GEMINI.md or the agent's rules file;
- where the agent supports hooks, a SessionStart hook that runs `starfix prime --hook`, which also registers the session.

**Tools.** A small verb set, about 2k tokens of schema in total. bd's beads-mcp costs 10 to 50k.

| Group | Tools |
|---|---|
| Work | `ready`, `claim`, `show`, `update`, `close`, `handoff` |
| Issues | `create`, `list`, `search`, `dep`, `comment`, `batch` |
| Memory | `remember`, `recall`, `forget` |
| Coordination | `inbox`, `lock`, `reserve`, `who` |
| Sync | `conflicts` (only listed when there are unresolved conflicts) |
| Reporting | `digest(since, by?, label?)`: structured summary of work closed, in progress, stalled, blocked and handed off, from `events`; capped at ~1.5k tokens. The calling agent writes any narrative; starfix runs no model |
| Cost | `cost(by?, since?, account?, issue?)`: read-only token and cost totals (list price and, for subscriptions, amortized), grouped by account, issue, epic, person or model; capped like `digest`. Setting prices and plan fees stays in the admin CLI |

- **No admin tools:** delete, purge, rename, import, setup, settings and provisioning are CLI only.
- **The session is implicit.** The server learns the principal from the SSH key. The client generates the session ID, or takes `CLAUDE_SESSION_ID` or its equivalent, and records the machine. An agent never passes identity.
- **Prime** comes as MCP `instructions` plus a `starfix://prime` resource, capped at 1.5k tokens. It contains:
  - your claims and your inbox count;
  - the top 5 ready issues;
  - pinned memories, then the most recent relevant ones.

## 6. Memory

| Scope | Visible to | Typical use |
|---|---|---|
| `team` | everyone on the server | conventions, decisions, gotchas |
| `project` | the project's members | repository facts |
| `user` | the author only (all their machines and agents) | personal preferences, working notes |

- Each memory record carries tags, an optional link to an issue, a pin flag, its author and a revision.
- `remember` defaults to `project` scope. The agent picks `user` when the memory is a personal preference (style, tools, how someone likes to work) and asks the person when that is borderline; the tool description says so. `team` is chosen explicitly.
- `recall` searches by key, tag or text, and later by meaning. It returns the newest first, with pinned memories ahead.
- Prime injects pinned memories plus the top N by relevance and recency, within the cap. It never sorts alphabetically, which is how bd ends up dropping arbitrary memories.
- Concurrent edits to the same key become a conflict (§8). bd silently keeps whichever side merged last.
- `user` scope is enforced by the server, not by a naming convention.
- A secrets lint refuses anything that looks like a key or password, in every scope.

## 7. Coordination beyond bd

| Gap in bd | starfix |
|---|---|
| Claim holder is a display name; parallel sessions share it | Holder is (principal, session, machine) |
| No fencing; a reclaimed worker's late close succeeds | Epoch on every claim; stale epoch refused |
| Leases expire mid-turn because nothing renews them | Client renews every 60 s while the session lives; TTL 15 min |
| Reaper needs an operator and client clocks | Server reaper on server time |
| Everything polls | Server-sent event stream; per-agent inbox |
| Merge slot has no TTL and a decorative queue | Leased mutex/semaphore, FIFO handoff, waiter expiry |
| No registry or presence | `agents` table; `who` |
| Two agents edit the same files | `reserve <glob>`; a conflicting reservation is refused, with the holder named |
| Handoff is free-text comments | `handoff` record delivered to the next claimant |
| A retried tool call creates a duplicate | Idempotency key on every create |
| Agents file near-duplicates | Create-time check returns candidates; `--new` overrides |
| First-come work stealing | WIP limit per agent, label affinity, priority aging, steal only after expiry |
| Cross-project deps need a local checkout | Rows on the server, notified on change |
| Gates are polled and cross-project gates never resolve | Server gate runner; GitHub webhooks |

## 8. Offline and conflicts

The client keeps a SQLite read cache of everything you can see, plus an outbox of operations. Each operation carries the `rev` it was based on, a hybrid logical clock stamp and an idempotency key. On reconnect the outbox replays in order; the server applies each operation or parks it as a conflict. Sync never blocks on a conflict.

| Change | Rule |
|---|---|
| Comments, labels, deps, new issues, memories with new keys | Merge (set-like, never conflict) |
| Different fields of one issue | Both apply (per-field three-way merge) |
| Same scalar field, both changed | Conflict, parked with base, mine and theirs; the server value stays until resolved |
| Long text (body, design, notes) | diff3 merge; overlapping hunks park a conflict |
| Status and claim | Server wins. Offline claims are *provisional*: if someone else claimed it, your claim loses, you get an inbox item, and your offline changes are kept as a comment |
| Close vs. edit | Close wins; the edit is kept as a comment |
| Delete vs. edit | Delete wins; the edit is kept on the tombstone and is restorable |

- `starfix away 4h` extends your leases before you go offline, so your claims hold.
- Conflicts show in `starfix conflicts` and in the MCP `conflicts` tool. Each one lists three choices: mine, theirs, or merged text. Resolving one is a normal operation, so it is audited too.
- A recommendation comes with each conflict. For example: "theirs is newer and from the claim holder; take theirs".

## 9. Bandwidth and verbosity

- Lists return `id, title, status, priority` plus only the fields asked for. `show` defaults to compact, and full text comes on request.
- Write results return `{id, rev}` only.
- Results are paged with a cursor; the default limit is 10 for agents.
- Sync is incremental by event sequence, compressed, and sends deltas, not rows.
- Errors take one line, with a `fix:` hint.
- Cap the prime payload and the MCP tool schemas; CI fails on a budget breach.

## 10. Search and vectors (later)

- An `embeddings` table keyed by (kind, id, model), filled when an issue, comment or memory changes, and skipped when its content hash is unchanged.
- The model runs locally on the server (nomic-embed-text by default), configurable per deployment to a hosted model. Store and model trade-offs: [database.md](database.md#vectors-later).
- Below about 50k rows, brute-force cosine in Go is fast enough. Above that, a Dolt vector index, or pgvector on the Postgres fallback.
- Uses:
  - `search --similar`;
  - the duplicate check at create;
  - ranking memories for prime.

## 11. Versions and upgrades

Dolt stays the backend (decided 6 Oct 2026). starfix and starfixd ship as one signed release: cosign keyless signature on the checksums, plus a build provenance attestation.

**`starfix upgrade [--check] [--rollback]`** (each developer machine)
- Reads the latest release, verifies the signature and checksum, and replaces the binary atomically. The previous binary is kept for `--rollback`.
- `--check` prints one line and changes nothing.
- Nothing upgrades itself without the command.

**`starfixd upgrade [--check] [--to vX.Y.Z] [--rollback]`** (on the server, as admin, or remotely as `starfix admin server upgrade`)
1. Verify the release, as above.
2. Back up first: a SQL dump and a Dolt tag `starfix-<old version>`.
3. Drain: refuse new writes, flush the commit batcher.
4. Swap the binary, run schema migrations, restart through systemd.
5. Health check. On failure, restore the old binary and report. Migrations are additive for one release, so the old binary still runs against the new schema.

**Dolt version.** Each starfix release pins the Dolt version it was tested with. `starfixd upgrade` installs that version (checksum verified), not simply the newest Dolt, because Dolt releases about weekly and an untested server/client mix is how bd lost data. `starfixd check` warns when the running Dolt differs from the pin.

**Version handshake.** Every response carries the server version, its protocol range and the latest release it knows of. starfixd checks for releases at most once a day; this can be turned off for air-gapped servers, where the admin sets the latest version by hand.

| Situation | Who is told | How |
|---|---|---|
| Server behind the latest release | every client and the admin | CLI: one stderr line, at most once a day per machine. MCP: one line in prime, never in tool results. `starfixd check` fails with a `fix:` line |
| Server behind a security release | everyone | as above, but on every CLI command and every session start until upgraded |
| Client behind the server | that client | one line: `starfix upgrade` |
| Client outside the server's protocol range | that client | refused, with a typed exit code and a `fix:` line (fail closed) |

The protocol supports one version back and one forward (principle 9), so the server and clients can be upgraded independently.

## 12. Developer experience

Agreed 6 Oct 2026; each item lands in the stage shown in §13.

| # | Feature | Agent or person sees |
|---|---|---|
| 1 | **`start` and `finish`** | `start` claims the top ready issue (or a named one) and returns it with acceptance criteria, the last handoff and the relevant memories. `finish` closes it, writes the handoff and files discovered work linked `discovered-from`. A typical session is two calls, not six. |
| 2 | **Git awareness** | `claim`/`start` can create the branch or worktree (`<type>/<id>-<slug>`). Commits and PRs link to the issue by branch name or a `Starfix:` trailer. A merged PR closes the issue through the GitHub webhook. No git hooks are installed. |
| 3 | **Files to issues** | The server records which paths each issue's commits touched. `ready` ranks down work that overlaps paths held by another claim or reservation; `show` lists likely files. |
| 4 | **Acceptance checklist** | Acceptance criteria are items, not prose. The agent ticks them; `close` refuses until all are ticked or waived with a reason. |
| 5 | **Errors that say what to do next** | Every refusal names the cause and the next action, for example `sf-a1b2 claimed by ed/codex 3m ago; next ready: sf-c3d4`. Typed error codes for programs. |
| 6 | **Similar closed issues** | `show` and `create` list up to three similar closed issues: full-text first, vectors when §10 lands. |
| 7 | **Live board** | `starfix tui` (terminal) and an optional read-only web view served by `starfixd`, both driven by the event stream. |
| 8 | **`starfixd --dev`** | A throwaway local server with a temporary Dolt database and seeded sample data, for trying starfix, demos and agent tests. |
| 9 | **Token budget in CI** | Scripted agent sessions measure tokens for each tool schema, each result shape and prime; CI fails when one exceeds its budget. |
| 10 | **Time and token reporting** | Optional: the client records wall time per claim and, where the harness exposes it, tokens per issue. Shown in `show` and `digest`. |

### 12.1 Token and cost tracking

Agreed 6 Oct 2026. Extends item 10.

- **Account.** Every project has an `account`: a client's engagement code name or an internal department. It defaults to `internal`. An epic or issue can override it, and children inherit it. Code names only; the map to real clients lives outside starfix.
- **What is recorded, per issue and per session:** model, input tokens, output tokens, cache-write tokens, cache-read tokens, wall time, harness. Cache tokens are separate because they are priced differently.
- **Where the numbers come from:** the harness, never the model's own report. The client reads whatever the harness exposes (for example Claude Code's OpenTelemetry usage metrics or hook payloads, and Codex's token-usage log) and attributes each delta to the issue the session held at that moment. When a session holds several issues, the delta is split by time held, and the record says it was split. A harness that exposes nothing records wall time only, marked as such.
- **Prices.** A `prices` table keyed by (model, effective date) with input, output, cache-write and cache-read rates. Cost is computed when a report runs, never stored, so a price change never rewrites history. Admins update prices with `starfix admin prices set`.
- **Subscriptions.** Reports always show the **list-price equivalent** (tokens at API rates). For a flat-rate plan, an admin records the plan's monthly fee and its seats; reports then also show the **amortized cost**: the month's fee split across all issues in proportion to their tokens.
- **Human time.** `starfix log 1.5h <id>` records a person's hours against the same account.
- **Reports.** `starfix cost --by account|issue|epic|person|model --since <date>` on the CLI, the MCP tool `cost` (below), and a cost line in `digest`. Agents see their current issue's running total in `show`, so they can notice when an issue gets expensive.
- **Limits.** Attribution is approximate when a session switches issues, and harnesses differ in what they expose. Invoicing stays outside starfix.

## 13. Plan

| Stage | Delivers | Gate |
|---|---|---|
| 0 | One-day Dolt spike: 20–50 concurrent claimers with compare-and-swap; commits per request vs. batched | **Done:** Dolt OK with conditions (`write_id`, serialized claims, batched commits) |
| 1 | Schema, `starfixd` core, SSH transport, issues/deps/labels/comments, ready via CTE, events, CLI CRUD, bd JSONL import, version handshake | Real bd backlogs imported and round-tripped |
| 2 | MCP server (work and issue tools), `start`/`finish`, git awareness, next-step errors, `starfixd --dev`, token budget in CI, `digest` (MCP and CLI), prime, `starfix upgrade` and `starfixd upgrade`, `setup` for Claude Code, Codex, Gemini, Cursor, VS Code | Agents use it daily on a real project |
| 3 | Claims with leases, epochs, reaper, agents registry, inbox, SSE, handoff, idempotency, files to issues, acceptance checklist, similar closed issues (full-text), live board, time and token reporting, `account` and token capture (§12.1) | Multi-session soak test |
| 4 | Memory with scopes and tags, migrated from bd `kv.memory.*`; prices, `starfix cost` and `starfix log` (§12.1) | |
| 5 | Offline cache, outbox, conflict parking and resolution | Partition tests |
| 6 | Locks, reservations, gates, molecules/formulas, swarm, cross-project | |
| 7 | Scheduled digests (draft only), GitHub sync, compaction, duplicate check, vectors, other trackers | |

bd stays in use until stage 2 passes on a real project.

## 14. Decisions

Maintainer, 6 Oct 2026:

1. The four Replace rows in §4 are accepted.
2. Memory defaults to `project` scope; the agent decides when a memory is a personal preference (`user`) and asks when borderline.
3. Embeddings: open; recommendation is a local model on the server (see §10).
4. License: Apache-2.0.
5. Dolt stays the backend. `starfix upgrade` and `starfixd upgrade` exist, and clients are warned when the server is out of date (§11).
6. All ten developer-experience features in §12 are in scope, and `digest` is an agent tool.
7. Track input and output tokens per issue, with cost estimates even on subscription plans (§12.1); accounts default to `internal`.
