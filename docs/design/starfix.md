# starfix design (draft 1)

6 Oct 2026. Status: accepted draft. Based on a survey of bd 1.2.2 (source, docs, changelog, open issues and PRs) and [database.md](database.md).

starfix is an issue tracker, shared memory and coordination layer for AI coding agents working across sessions and machines. It is one Go module and two binaries; the client command is `sfx`, short for starfix:

| Binary | Runs | Role |
|---|---|---|
| `sfx` | each developer machine | MCP server for agents (stdio), admin CLI for people, local cache and op log |
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
agent ──MCP stdio──> starfix
person ──CLI──────> (same binary)
                     ├ SQLite (WAL): read cache, op log, outbox
                     └ in-process SSH, host key pinned
                          ║ ssh:22, one session: newline-delimited JSON frames
                          ▼
                     sshd ── forced command: starfixd stdio --principal <name>
                          │ unix socket (0700 directory, same Unix user)
                          ▼
                     starfixd serve
                       ├ leases, reaper (server clock)
                       ├ event stream (pushed frames) and inboxes
                       ├ gate runner, GitHub webhooks
                       └ dolt sql-server (loopback)
```

**Transport.** `golang.org/x/crypto/ssh` runs inside the process: no system `ssh` and no port juggling. The developer's SSH key is their identity. No new port is opened: the server's own sshd authenticates each key, and each line in the starfixd account's `authorized_keys` is `restrict,command="starfixd stdio --principal <name>" <key>`, so the principal comes from which key authenticated, never from the client, and shared Unix accounts no longer hide who did what.

- `starfixd serve` is the daemon. It owns the store and listens on a unix socket in a 0700 directory. It runs on Linux only, because only there can it check which user connects (`--dev` overrides this on a single-user machine; Windows hosts use WSL2). The `starfix` client and MCP server run natively on Linux, macOS and Windows.
- `starfixd stdio` is the forced command. It connects to that socket, sends a bridge frame naming the principal, then copies the session's stdin and stdout. The daemon trusts the bridge frame only because the socket peer is local and is the daemon's own user (SO_PEERCRED on Linux).
- The protocol is newline-delimited JSON frames (`internal/proto`): a hello and welcome that negotiate the protocol version (§11), then requests and responses with typed error codes. A frame type is reserved for server-pushed events (stage 3).
- This replaces the earlier plan of JSON over HTTP/2: an SSH session already gives an authenticated, encrypted, ordered stream, and a forced command needs no extra listener.

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
| JSONL import/export | Port | bd-compatible both ways, so migration is `sfx import < bd.jsonl` |
| `batch` | Port | MCP `batch` tool and CLI; one transaction, one round trip |
| `kv`, `config`, `metadata` | Port | `settings` table plus issue `metadata` JSON |
| Memories (`remember`, `recall`, `forget`, `memories`) | Redesign | Scoped, tagged, authored records (§6) |
| `prime`, `onboard`, `quickstart`, `setup <agent>`, `rules`, `preflight` | Redesign | MCP `instructions` plus a capped `prime` resource; `sfx setup <agent>` (§5) |
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
| `doctor`, `recompute-blocked`, repair migrations | Replace | Nothing derived is stored, so nothing drifts; `sfx check` verifies connection and schema |
| OTEL metrics | Port, later | |

## 5. Agent interface (MCP)

**Interoperability.** Claude Code, Codex, Gemini CLI, Cursor, VS Code Copilot, Junie, Amp and Claude Desktop all speak MCP over stdio. One server serves them all. Agents that only read instruction files (Aider, Windsurf, Kilo, Kiro, Cody, OpenCode, Factory) get a pointer line, plus the CLI in read-only mode.

`sfx setup <agent> [--check|--remove]` writes three things:
- the agent's MCP config;
- a marker-delimited pointer in AGENTS.md, CLAUDE.md, GEMINI.md or the agent's rules file;
- where the agent supports hooks, a SessionStart hook that runs `sfx prime --hook`, which also registers the session.

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

**As built (stage 2, first slice, 7 Oct 2026).** Where the build differs from the above:
- Tools so far: `prime`, `start`, `finish`, `handoff`, `ready`, `blocked`, `list`, `show`, `create`, `update`, `close`, `reopen`, `dep` (one tool, `action` add or rm), `label`, `comment`, `comments`, `history`, `digest`. `claim` waits for stage 3 claims; `start` stands in for it. Their schemas cost about 1.6k tokens; no output schemas are published, since they would double that.
- Prime is a `prime` tool and `sfx prime`, not yet a `starfix://prime` resource, with the static MCP `instructions`. It holds your in-progress issues (assigned to you), the top 5 ready and the version notices; claims, inbox and memories join it in their stages.
- `sfx setup <agent>` covers Claude Code, Codex, Gemini CLI, Cursor and VS Code: it prints by default, `--write` edits the project's files, `--global` the home ones, plus `--check` and `--remove`. It writes the MCP config (`.mcp.json`, `.codex/config.toml`, `.gemini/settings.json`, `.cursor/mcp.json`, `.vscode/mcp.json`) and a 3-line pointer between `<!-- starfix:begin -->` and `<!-- starfix:end -->` in `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.github/copilot-instructions.md`, or Cursor's own `.cursor/rules/starfix.mdc` (`alwaysApply: true`). `--remove` deletes a pointer file left empty. VS Code refuses `--global`: its user MCP config lives in a per-platform profile.
- The SessionStart hook is written for Claude Code only, in `.claude/settings.json`, with no matcher, so it also runs on resume, clear and compact. Gemini CLI's and Cursor's hook support was too new and unsettled to write blind; Codex has none. `sfx prime --hook` prints `hookSpecificOutput.additionalContext` JSON, takes the session id from the hook input (`STARFIX_SESSION` still wins), prints nothing outside a starfix repository, and on any error adds a one-line note and exits 0, within 10 seconds. "Registers the session" is, until stage 3's agents registry, the session id on the connection the hook opens.
- Session ids: Claude Code sets `CLAUDE_CODE_SESSION_ID`; Codex and Gemini CLI set none starfix knows of, so `sfx mcp` picks one per process (kept across reconnects) unless `STARFIX_SESSION` is set. The welcome frame now echoes the principal, so the client knows who "you" are.
- `start` and `finish` (§12 item 1, second slice, 7 Oct 2026). Before leases, "take" is a compare-and-swap on the issue's `rev` that sets `in_progress` and assigns the caller's principal; the issue is held while it is `in_progress` and assigned. Taking your own issue again changes nothing; another principal's `start` is refused with the next ready issue to take, and their `finish` or releasing `handoff` is refused too (`close` still overrides). `start` without an id takes what `ready` lists first, in the same transaction. It returns the issue, its acceptance criteria, the last handoff and the suggested branch; memories join it in stage 4.
- `finish` closes the issue, records the handoff and creates the discovered issues with their `discovered-from` edges in one transaction: all or nothing. `handoff` records a note without closing; `release` also unassigns the issue and returns it to `open`.
- Handoffs are comments with `kind` `handoff` (migration 0002 adds the column), not yet the `handoffs` table of §3, whose `state`, `next`, `branch` and `worktree` fields arrive with stage 3. A bd export writes them as plain comments.
- Git awareness (§12 item 2) is client-side only: the branch is `<type>/<id>-<slug>` with `bug` → `fix/`, `feature` → `feature/`, `task` and `chore` → `maintenance/`, `docs` → `docs/`, anything else `feature/`. `sfx start --branch` creates or switches to it, `--worktree DIR` makes a worktree on it; MCP `start` only suggests it. Parsing an id from a branch or a `Starfix:` trailer is there for linking commits and pull requests later. No hooks are installed.
- `digest` (stage 2, 7 Oct 2026) is a server op over the `events` table and issue state, in one read-only snapshot; migration 0003 indexes `events (at)` so a window is a range scan. `since` is an RFC 3339 time, a date (midnight UTC) or a duration back from the server's now (`90m`, `24h`, `7d`, `2w`; default `24h`, at most 366 days). Sections: closed in the window (and still closed), started (set `in_progress`), in progress now (holder and how long), stalled (in progress with no event for 48 hours, `store.StalledAfter`), blocked now, handed off (latest note, cut to 200 bytes), created (top 5 by priority) and discovered-from links. Each lists 10 with a total; `by` filters on who made the event, or on the assignee for the state sections; `label` filters on the issue, or on either end of a discovered-from link. Imported issues do not count as created, and the time and cost lines (§12 item 10, §12.1) join it with token capture in stage 3. The MCP tool cuts titles and then drops items from the longest section until it fits 1.5k tokens, and says `truncated`.
- Every tool result is capped at about 2k tokens: lists re-ask the server with a smaller limit so their cursor stays exact; `show`, `comments` and `history` cut text or drop the oldest records and say so.

**As built (stage 3, discovery, 7 Oct 2026).** The goal is that an agent in any supported harness learns to use starfix without a person remembering per-harness steps. Where the build differs from the above and from stage 2:
- `sfx setup --all [--write|--check|--remove] [--global]` runs setup for every agent, one summary line each. Edits are chained per file: Gemini CLI keeps its MCP server and its hook in one `settings.json`, and Codex and Junie share `AGENTS.md`, so each part is applied to the previous part's output and the file is written once. `--all --global` skips the agents with no home config (VS Code, AI Assistant), printing their fix.
- New agents: `junie` (JetBrains Junie: `.junie/mcp/mcp.json` and `~/.junie/mcp/mcp.json` in the Claude Desktop shape, with no `type`; pointer in `AGENTS.md` or `~/.junie/AGENTS.md`) and `jetbrains` (JetBrains AI Assistant chat: no MCP file exists, so setup writes only the rule file `.aiassistant/rules/starfix.md` with `apply: always` frontmatter, a key not confirmed by the docs, and prints the IDE steps and the JSON to paste; `--global` is refused).
- SessionStart hooks for Codex (`.codex/hooks.json`, matcher `startup|resume|clear|compact`), Gemini CLI (in `settings.json`; its matcher is an exact source and the docs do not say an omitted one matches all, so one group each for `startup`, `resume` and `clear`; timeout 15000 ms), Cursor (`.cursor/hooks.json`, `{"version": 1, "hooks": {"sessionStart": [{"command", "timeout": 30}]}}`), VS Code (Preview; `.github/hooks/starfix.json`, a file of starfix's own, deleted on remove) and Junie (only `~/.junie/config.json` with `--global`, since Junie ignores hooks in project config). Each runs `sfx prime --hook=AGENT`; bare `--hook` stays Claude Code's. A hook is starfix's when its command is `prime --hook[=…]` through `sfx` or the `--command` program, as before, so other hooks are never touched.
- `sfx prime --hook=AGENT` prints the harness's format: Cursor's `{"additional_context": …}`, else `hookSpecificOutput.additionalContext`. It reads the session id from `session_id`, or VS Code's `sessionId`. It still always exits 0, prints nothing outside a starfix repository, and turns errors into a one-line note. An agent name with no hook (`jetbrains`) is a usage error.
- Each agent's note, printed after setup adds a part, names the trust step the person must still take: approve the server (Claude Code, VS Code), trust the project and run `/hooks` (Codex), trust the folder (Gemini CLI), enable the server in Settings › MCP (Cursor), run setup with `--global` for the hook (Junie), or add the server in the IDE (AI Assistant). The Gemini CLI, Cursor and VS Code trust wording is from general knowledge of those products, not the hook pages below.
- **Limitation, for the security review: session ids.** Only Claude Code sets a session id in the environment of both the hook and `sfx mcp` (`CLAUDE_CODE_SESSION_ID`). Codex, Gemini CLI, Cursor, VS Code and Junie pass the session id only in the hook's stdin, so the hook's prime connects under the harness's id while `sfx mcp`, which never sees it, picks its own (`m-…`). One agent session then shows as two in `sfx who`: a short-lived one from the hook and the MCP session that holds leases. Nothing is shared between them, so the hook cannot act for the MCP session or the reverse, but presence and audit trails split across two ids. No fix is built; options include the hook writing the id where `sfx mcp` can find it, which would need its own trust analysis.
- Sources, read 7 Oct 2026: code.claude.com/docs/en/hooks, geminicli.com/docs/hooks/reference/, learn.chatgpt.com/docs/hooks, cursor.com/docs/agent/hooks, code.visualstudio.com/docs/copilot/customization/hooks, junie.jetbrains.com/docs/junie-cli-mcp-configuration.html, junie.jetbrains.com/docs/junie-cli-hooks.html, jetbrains.com/help/ai-assistant/mcp.html, jetbrains.com/help/ai-assistant/configure-project-rules.html.

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

**As built (stage 3, claims, 7 Oct 2026).** Where the build differs from the above:
- `claims` has one row per issue ever taken: holder (principal, session, machine), `epoch`, `claimed_at`, `expires_at` (migration 0004). A released or expired claim keeps its row with no holder, so the epoch only rises. There is no `provisional` flag until offline claims (stage 5).
- `start` is the claim: it leases the issue and sets it `in_progress`, assigned to the principal, in one transaction. There is no separate `claim` tool. The same session starting again only extends the lease; another session of the same principal takes over at once under a new epoch, because a person who restarts their agent should not wait out the lease; another principal waits for expiry.
- Leases: an agent's is 15 minutes, renewed every minute by `sfx mcp` while it runs (protocol op `renew`). A person's own commands take 8 hours (`sfx start --for`), because nothing renews them, and run as session `cli`, one per machine. `sfx away D` extends every claim you hold, in every session, up to 7 days. A renewal never shortens a lease, and rewrites the row only when less than half the lease is left; renewals are not events.
- Fencing: `finish` and `handoff --release` take an optional epoch and refuse any other (`sfx mcp` passes the one its `start` returned). Closing an issue ends its claim, whoever closes it.
- Reaper: `starfixd serve` ends lapsed claims every 30 s as `starfixd/reaper`, recording `claim.expire` and returning the issue to `open`, unassigned, if it is still the holder's. A lapsed claim holds nothing even before the reaper runs. Inbox items for lost claims come with the inbox; until then the next `prime` of the session that lost one lists it under `lost`.
- An issue `in_progress` with no claim row (imported, set by hand, or taken before claims) stays held by its assignee with no lease, as in stage 2.
- Protocol 2 carries these fields; servers still accept protocol 1 clients, whose `start` gets the 15-minute default.

**As built (stage 3, agents registry, 7 Oct 2026).** Where the build differs from the above:
- `agents` is keyed (`principal`, `session`) with `machine`, `harness`, `started`, `last_seen`, `rev` and `write_id`, indexed on `last_seen` (migration 0005). `labels` waits until something reads it (label affinity).
- The server touches a session's row after every successful handshake, and on every `renew`; a failure is logged and never refuses the connection or the request. A touch rewrites the row only when it is new, the machine or harness changed, or `last_seen` is a minute old, and records no event: presence is not history, as with renewals. An empty harness keeps the one recorded; a harness the server would refuse is dropped and the session still registered.
- `sfx mcp`'s renew loop now runs while connected even when the session holds no claims, so an idle agent stays present; it does not dial just for that, so a session whose connection dropped leaves `who` after 5 minutes until its next tool call.
- The harness comes from the hello frame's `h` field: `STARFIX_HARNESS`, which `sfx setup` writes into the MCP config's `env` for every agent (Codex: a `[mcp_servers.starfix.env]` table; an inline `env = {…}` is refused with a fix), else `claude-code` for `CLAUDECODE=1`, else `gemini` for `GEMINI_CLI=1`. Values are the `sfx setup` agent names. Old servers ignore the field, so it needs no protocol bump.
- `who` (CLI, MCP tool and protocol op) lists sessions seen within `since` (default 5 minutes, at most 7 days) on the server's clock, most recently seen first, each with the issues it holds under an active claim; the result carries the server's `now`. The MCP tool drops the least recently seen to fit 2,000 tokens and says `more`. A person's own commands appear as session `cli`.
- The op is part of protocol 2 without another bump: protocol 2 had not shipped in a release (v0.1.0 speaks 1), so no client or server speaks a protocol 2 that lacks it.
- Fitting `who` under the 2,200-token schema budget took trimming a few existing tool descriptions; the set is at the limit.

**As built (stage 3, inbox and handoffs, 7 Oct 2026).** Where the build differs from the above:
- `inbox` (migration 0006) has an integer `id` (the next after the largest), `to_principal`, an optional `to_session`, `kind`, `issue_id`, `body` (200 bytes, whitespace collapsed), `from_principal`, `at`, `read_at` and `write_id`, indexed on (`to_principal`, `read_at`). An item without a session is for every session of the principal. Kinds so far: `claim.lost` (to the session that lost it, when the reaper ends its expired claim or another session or principal takes it over under a new epoch), `handoff` (to a handoff's `to`), `mention` and `assigned`. Gates and revoked claims beyond these wait for their stages.
- An item is written in the same transaction as its cause, so it rolls back with it, and nobody is told of their own act: an item is skipped when it would go to the acting principal (for a session item, the acting session). A mention is `@name` at the start or after a character that cannot be part of an address, in a comment or a handoff or finish note; it notifies only a principal in the `agents` registry, at most 10 per note, and not a handoff's `to`, who gets the `handoff` item instead. Assigning yourself sends nothing. Closing someone's claimed issue sends no `claim.lost`: the close itself is the news.
- Reading and acking are protocol ops `inbox` (unread, or `all`; newest first; 20 by default, at most 100; the result carries the unread count) and `ack` (ids, or `all`). Acking is not an event: like a renewal, it is bookkeeping, not history.
- Push: the op `watch` asks the server to push the caller's new items on that connection. After a write commits, the single writer offers its items to every watch for the recipient without blocking; each connection has a queue of 64 and a goroutine that sends them as `evt` frames (`{"t":"evt","op":"inbox","e":{item}}`), the frame type §4 reserved, rather than a new `ev`. A full queue drops it and sends one `{"t":"evt","op":"resync"}`; nothing more is pushed until the client watches again and reads the inbox. Before each response the server sends what is queued, so an item committed before a request always arrives ahead of its answer. `watch` returns the unread count; it is refused off a connection (`sfx watch` holds one).
- The client reads frames on its own goroutine, hands events to a callback and skips frame types it does not know. `sfx mcp` watches on every connection it opens and again after a resync, counts what arrives, and appends one line to the next tool result as a second text block, `inbox: N new (call inbox)`. The `inbox` tool lists unread items after acking any ids given. `prime` shows the unread count and the newest 3, and its `lost` list now comes from unread `claim.lost` items, replacing the stopgap of the claims note above. A server without the inbox refuses the op, and prime goes on without it.
- `sfx inbox` lists and acks; `sfx watch` prints items as they come until interrupted, and with `--json` one object per line, `{"op":"inbox","item":{…}}` or `{"op":"resync"}`.
- Handoffs (migration 0007): the note stays a comment of kind `handoff`, and its structured fields go in a `handoffs` row keyed by the comment's id: `state` (`done`, `partial` or `blocked`), `next` (500 bytes), `branch` (a git-safe pattern starting with a letter or digit, so it cannot be read as an option, 255), `worktree` (1,024, no control characters) and `to` (a principal). A separate table, rather than columns on `comments`, keeps comments as the plain append-only log of §3 and gives the fields a row only when there are any; the comment's `comment.add` event records them too. Fields need a note. `start` and `show` return the latest handoff with its fields; `finish` and `handoff` take them on the CLI and over MCP, which leaves out `worktree` (a path on one machine, and schema budget). A bd export still writes the note alone.
- These ops and fields are part of protocol 2 without another bump: protocol 2 has not shipped in a release (v0.1.0 speaks 1).
- The MCP tool set is 20 tools at about 2,390 estimated tokens; the budget rose from 2,200 to 2,400 for the `inbox` tool and the handoff fields.

**As built (stage 3, idempotency, acceptance and similar issues, 7 Oct 2026).** Where the build differs from the above:
- Idempotency (migration 0008): `create`, `finish` (with its discovered issues), `comment` and `handoff` take an optional `idem` key matching `^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`. The key, the SHA-256 of the op name and its arguments, and the result go on the operation's last event, under a unique index on (`principal`, `idem_key`), so keys are per principal. Events are buffered in the transaction and written at its end, so the stamp lands on the last one. A repeat with the same key and arguments returns the stored result and writes nothing; the same key with other arguments is refused with `conflict`, naming the key. A key older than 24 hours is refused the same way rather than reused, because the unique index still holds it. `sfx mcp` makes a key per tool call and reuses it when it redials after a dropped connection (`finish`, `handoff` and `comment` now retry like `create`); the CLI makes one per command. Keys are not in the MCP schemas. `create`'s existing key limit tightened from 128 characters to this pattern.
- Acceptance checklist (migration 0009): items are derived from the acceptance text on every read, never stored: each Markdown list item (`-`, `*`, `+`, `1.` or `1)`, with or without `[ ]`), indented lines continuing it, and text around the list ignored; text with no list is one item. State is a row in `acceptance_state` keyed by the issue and the SHA-256 of the item's text, so editing the text keeps the state of unchanged items and opens changed ones, and renumbering does not move ticks. `[x]` in the text counts as ticked until an untick writes an `open` row. The op `accept` ticks, unticks and waives (with a one-line reason up to 500 bytes), each change an `acceptance.tick`, `acceptance.untick` or `acceptance.waive` event, and returns `{id, items, open}`; it needs no claim and is refused on a closed issue. `close` and `finish` refuse while an item is open, with the new code `acceptance`; `close` alone takes `force`, and its `issue.close` event records the overridden items as `acceptance_overridden`. MCP `finish` takes `ticked: [n]` and `waived: {n: reason}`; `start` and `show` return the items, and MCP shows them in place of the acceptance prose. Imported bd issues are parsed the same way. Tick state is not in a bd export.
- Similar closed issues: Dolt's FULLTEXT index was probed first and is unreliable (`MATCH` in `WHERE` returns duplicate rows, and boolean mode is unsupported; `TestDoltFulltextIsUnreliable` fails once that changes), so a Go scorer compares titles instead. It scans the 2,000 most recently closed non-template issues (migration 0010 indexes `status, closed_at`), tokenizes titles (lowercase, letters and digits, stopwords dropped, a light suffix stem), and keeps an issue sharing two tokens, or one with a Jaccard score of at least 0.5; at most 3, best first. `create` still creates and returns `similar` beside `{id, rev}`; `show` returns them too. The CLI prints them to stderr after `create`, so the id stays alone on stdout; MCP gives one terse line. A failed lookup is logged and lists none.
- These ops, fields and the code are part of protocol 2 without another bump, which has not shipped in a release. The MCP schemas stay at about 2,400 estimated tokens after trimming several descriptions.

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

- `sfx away 4h` extends your leases before you go offline, so your claims hold.
- Conflicts show in `sfx conflicts` and in the MCP `conflicts` tool. Each one lists three choices: mine, theirs, or merged text. Resolving one is a normal operation, so it is audited too.
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

**`sfx upgrade [--check] [--rollback]`** (each developer machine)
- Reads the latest release, verifies the signature and checksum, and replaces the binary atomically. The previous binary is kept for `--rollback`.
- `--check` prints one line and changes nothing.
- Nothing upgrades itself without the command.

**`starfixd upgrade [--check] [--to vX.Y.Z] [--rollback]`** (on the server, as admin, or remotely as `sfx admin server upgrade`)
1. Verify the release, as above.
2. Back up first: a SQL dump and a Dolt tag `starfix-<old version>`.
3. Drain: refuse new writes, flush the commit batcher.
4. Swap the binary, run schema migrations, restart through systemd.
5. Health check. On failure, restore the old binary and report. Migrations are additive for one release, so the old binary still runs against the new schema.

**Dolt version.** Each starfix release pins the Dolt version it was tested with. `starfixd upgrade` installs that version (checksum verified), not simply the newest Dolt, because Dolt releases about weekly and an untested server/client mix is how bd lost data. `starfixd check` warns when the running Dolt differs from the pin.

**Version handshake.** The first exchange on every connection carries the client's version and protocol version, and the server's version, its protocol range and the latest release it knows of. starfixd checks for releases at most once a day; this can be turned off for air-gapped servers, where the admin sets the latest version by hand.

| Situation | Who is told | How |
|---|---|---|
| Server behind the latest release | every client and the admin | CLI: one stderr line, at most once a day per machine. MCP: one line in prime, never in tool results. `starfixd check` fails with a `fix:` line |
| Server behind a security release | everyone | as above, but on every CLI command and every session start until upgraded |
| Client behind the server | that client | one line: `sfx upgrade` |
| Client outside the server's protocol range | that client | refused, with a typed exit code and a `fix:` line (fail closed) |

The protocol supports one version back and one forward (principle 9), so the server and clients can be upgraded independently.

**As built (stage 2, 7 Oct 2026).** Where the build differs from the above:
- **Signing.** Changed on 7 Oct from cosign keyless: `checksums.txt.sig` is an Ed25519 signature over `checksums.txt`, made in the release workflow with a key held as a GitHub environment secret (`release`). Only release runs the maintainer approves, on `v*` tags only the maintainer can push, can use it; GitHub and organization owners can still reach it, which is the accepted trade-off. The public keys are compiled in (`internal/release/keys.go`, a list so a rotation can overlap); with none, every upgrade fails closed. GitHub build provenance (`actions/attest-build-provenance`) is kept as an independent check. `RELEASING.md` has the workflow, setup and rotation.
- **`sfx upgrade`** verifies the signature and the archive's sha256, runs the staged binary's `version` to confirm it, keeps the old binary as `<exe>.prev` and renames the new one into place (on Windows the running exe is first renamed to `.old`). It refuses dev builds and ignores prereleases. `--rollback` consumes `.prev`, so a second rollback reports there is nothing to restore.
- **`starfixd upgrade`** runs on Linux as the daemon's user, never root. The backup is the Dolt tag `starfix-<old version>` (`store.BackupTag`, after a commit of the working set; a later upgrade from the same version adds `starfix-<old>-<time>` rather than moving it). There is no SQL dump yet: for one, run `dolt dump` in the server's data directory before upgrading. There is no drain op: the restart relies on systemd's stop, which cancels the daemon's context, closes connections (an in-flight write is rolled back and the client told) and makes the final Dolt commit in `Store.Close`. Migrations run when the new daemon opens the store. It restarts only when `systemd_unit:` is set or `--restart` is given, through `sudo -n systemctl restart`; the health check is a handshake over the socket, wanting the new version within 30 seconds, and a failure restores the old binary and restarts it. `--to` refuses to go back a version (migrations only go forward); `--rollback` is the way back. `sfx admin server upgrade` is not built.
- **Dolt version.** `version.Dolt` records the pin (2.4.2), a test keeps CI's `DOLT_VERSION` equal to it, and the release notes name it. `starfixd upgrade` does not install Dolt yet, and `starfixd check` does not exist yet.
- **Latest release in the handshake.** The welcome still carries only the hand-set `latest:`; starfixd does not check GitHub for releases yet, so the "server behind" notices depend on that setting.

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
| 7 | **Live board** | `sfx tui` (terminal), driven by the event stream. A web view served by `starfixd` is dropped for now (7 Oct 2026): starfixd opens no port, and a TUI over the existing SSH connection keeps it that way. |
| 8 | **`starfixd --dev`** | A throwaway local server with a temporary Dolt database and seeded sample data, for trying starfix, demos and agent tests. |
| 9 | **Token budget in CI** | Scripted agent sessions measure tokens for each tool schema, each result shape and prime; CI fails when one exceeds its budget. |
| 10 | **Time and token reporting** | Optional: the client records wall time per claim and, where the harness exposes it, tokens per issue. Shown in `show` and `digest`. |

### 12.1 Token and cost tracking

Agreed 6 Oct 2026. Extends item 10.

- **Account.** Every project has an `account`: a client's engagement code name or an internal department. It defaults to `internal`. An epic or issue can override it, and children inherit it. Code names only; the map to real clients lives outside starfix.
- **What is recorded, per issue and per session:** model, input tokens, output tokens, cache-write tokens, cache-read tokens, wall time, harness. Cache tokens are separate because they are priced differently.
- **Where the numbers come from:** the harness, never the model's own report. The client reads whatever the harness exposes (for example Claude Code's OpenTelemetry usage metrics or hook payloads, and Codex's token-usage log) and attributes each delta to the issue the session held at that moment. When a session holds several issues, the delta is split by time held, and the record says it was split. A harness that exposes nothing records wall time only, marked as such.
- **Prices.** A `prices` table keyed by (model, effective date) with input, output, cache-write and cache-read rates. Cost is computed when a report runs, never stored, so a price change never rewrites history. Admins update prices with `sfx admin prices set`.
- **Subscriptions.** Reports always show the **list-price equivalent** (tokens at API rates). For a flat-rate plan, an admin records the plan's monthly fee and its seats; reports then also show the **amortized cost**: the month's fee split across all issues in proportion to their tokens.
- **Human time.** `sfx log 1.5h <id>` records a person's hours against the same account.
- **Reports.** `sfx cost --by account|issue|epic|person|model --since <date>` on the CLI, the MCP tool `cost` (below), and a cost line in `digest`. Agents see their current issue's running total in `show`, so they can notice when an issue gets expensive.
- **Limits.** Attribution is approximate when a session switches issues, and harnesses differ in what they expose. Invoicing stays outside starfix.

## 13. Plan

| Stage | Delivers | Gate |
|---|---|---|
| 0 | One-day Dolt spike: 20–50 concurrent claimers with compare-and-swap; commits per request vs. batched | **Done:** Dolt OK with conditions (`write_id`, serialized claims, batched commits) |
| 1 | Schema, `starfixd` core, SSH transport, issues/deps/labels/comments, ready via CTE, events, CLI CRUD, bd JSONL import, version handshake | Real bd backlogs imported and round-tripped |
| 2 | MCP server (work and issue tools), `start`/`finish`, git awareness, next-step errors, `starfixd --dev`, token budget in CI, `digest` (MCP and CLI), prime, `sfx upgrade` and `starfixd upgrade`, `setup` for Claude Code, Codex, Gemini, Cursor, VS Code | Agents use it daily on a real project |
| 3 | Claims with leases, epochs, reaper, agents registry, inbox, event push, handoff, idempotency, files to issues, acceptance checklist, similar closed issues (full-text), live board, time and token reporting, `account` and token capture (§12.1) | Multi-session soak test |
| 4 | Memory with scopes and tags, migrated from bd `kv.memory.*`; prices, `sfx cost` and `sfx log` (§12.1) | |
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
5. Dolt stays the backend. `sfx upgrade` and `starfixd upgrade` exist, and clients are warned when the server is out of date (§11).
6. All ten developer-experience features in §12 are in scope, and `digest` is an agent tool.
7. Track input and output tokens per issue, with cost estimates even on subscription plans (§12.1); accounts default to `internal`.

Maintainer, 7 Oct 2026, from the unattended-operation review ([bearings.md §3.14](bearings.md#314-unattended-operation)):

8. starfixd gains an autonomy window, narrower default rights, distinct agent identities, parking of a person's lapsed claim during a window, and git push fencing by claim epoch. They are staged after the security review, before any release that enables unattended work.
9. SSH stays the only transport; an HTTPS transport waits.
10. WireGuard is a documented, optional layer in front of the server's SSH port; nothing in starfix requires it.
