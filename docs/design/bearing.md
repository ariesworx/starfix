# Bearing: an agent orchestrator on starfix

Status: **draft specification**, open for review. Companion to
[starfix.md](starfix.md).

Bearing runs and supervises many AI coding agents across sessions and machines.
starfix holds all state: issues, claims, leases, memory, events and costs.
Bearing holds none. It is the deterministic Go layer that starts agents, keeps
their claims honest, routes work to them and lands their output safely.

The design draws on Gas Town and its successor Gas City, which proved the idea
and exposed its failure modes: claims that never release, reapers that kill live
agents, LLM supervisors that burn tokens, merges on red CI, and agents running
with every permission on the host. Each component below names the failure it
removes.

## 1. Principles

1. **Go supervises; LLMs judge.** Spawning, reaping, routing, budgeting and
   merging are deterministic Go reacting to events. A model is called only for
   a question that needs judgment, such as "why is this agent stuck?".
2. **Stateless.** Every fact lives in starfix. A Bearing restart loses nothing
   and adopts its running children.
3. **Event-driven, never polling.** Bearing subscribes to the starfix event
   stream (SSE). No status loops, no per-operation CLI processes.
4. **Identity is the SSH principal plus the session**, as in starfix. Never a
   path, a working directory or an environment variable.
5. **Safe by default.** Pull-request mode with a human gate. No merge without
   green CI, never a force-push, never a push to a protected branch. Agents run
   in their harness's allowlist mode; unrestricted permissions only inside a
   sandbox.
6. **Provider allowlist.** Bearing launches only providers and models an
   operator has allowed, and refuses everything else at config load.
7. **One static binary, Go only.** No Node, Python or TypeScript components. No
   hard dependency on tmux.
8. **Terse.** Same output rules as starfix: failures print, passes only with
   `-v`, one summary line, `--json` for machines.

## 2. Architecture

```
            operator: bearing CLI · TUI · HTML status page
                                │
 ┌──────────────── bearingd (one per machine) ─────────────────┐
 │ supervisor ─ patrol ─ dispatcher ─ governor ─ merge train   │
 │     │                                                       │
 │  agent CLIs (Claude Code, Codex, Gemini), one process group │
 │  and one git worktree each, optionally sandboxed            │
 └──────┬──────────────────────────────────────────────────────┘
        │ starfix client: MCP for agents, RPC + SSE for bearingd
        ▼
     starfixd ─── Dolt
```

- `bearingd` runs on every machine that runs agents, because it owns their
  processes and worktrees: a developer's laptop, a headless server, or both.
  Several can share one starfix server; that is the multi-machine story, and it
  mostly falls out of starfix's single authority and leases.
- **Deployment order.** Developer machines first, until Bearing is proven:
  supervise agents on the developer's machine under their own logins. Then a
  server `bearingd` that keeps working when no developer is online. The merge
  train (§3.5) holds push credentials and the branch lock, so it runs in
  exactly one place, on that server.
- Agents talk to starfix through its MCP server. Bearing never types into an
  agent's terminal.
- `bearing` is the operator CLI. It talks to the local `bearingd` over a unix
  socket.

## 3. Components

Each row names the starfix primitive it builds on and the Gas Town failure it
addresses.

| # | Component | Uses | Removes |
|---|---|---|---|
| 1 | **Supervisor with lease coupling** | claims, epochs, leases, agents registry | claims stuck `in_progress`; leases reading expired on live agents; orphaned processes |
| 2 | **Typed agent state** | agents registry, events | usage limits indistinguishable from hangs |
| 3 | **Event-driven patrol** | SSE events, reaper, gates | LLM patrol loops, heartbeat drift, idle token burn |
| 4 | **Path reservations at dispatch** | reservations, swarm waves | conflicts discovered only at merge time |
| 5 | **Safe merge train** | locks, gates, events | merges on red CI, force-push recovery |
| 6 | **Structured handoff** | handoffs, scoped memory, prime | lost reasoning when a session ends |
| 7 | **Budget and concurrency governor** | semaphores, cost tracking (starfix §12.1) | rate-limit exhaustion, runaway spend |
| 8 | **MCP inbox instead of keystrokes** | inbox, SSE, `starfix setup` | send-keys fragility, provider-specific hooks |
| 9 | **Dispatch policy** | ready, claims, labels | first-come dispatch, manual assignment |
| 10 | **Formula runner** | molecules, idempotency keys, gates | duplicate workflow instances |
| 11 | **Observability** | events, agents, digest | many stores, polling dashboards |
| 12 | **Sandboxed workers** | principal and session mapping | host-wide credential access |

### 3.1 Supervisor with lease coupling

- Spawn each agent CLI as a child with `Setpgid`, in its own git worktree, with
  a pty only when the harness needs one.
- While the PID is alive, renew the claim lease. Liveness is the process, not
  the model's willingness to call a heartbeat tool.
- On exit: if the agent finished, the issue is already closed. Otherwise
  release the claim, or mark it `stalled` with a handoff (§3.6) when work is in
  the worktree.
- On `bearingd` restart, adopt live children by PID and session ID; reap the
  rest. Teardown kills the whole process group.
- A fenced epoch on every claim means a recycled agent with the same name can
  never act on a stale claim.

### 3.2 Typed agent state

`working`, `idle`, `blocked{reason, until}`, `exiting`, `stalled`.

- Derived from exit codes and the harness's structured output per provider,
  never from scraping terminal text.
- `blocked{usage_limit, until}` pauses the agent and its claims without being
  reaped, and resumes it at `until`.
- State changes are starfix events, so every observer sees the same truth.

### 3.3 Event-driven patrol

Go handlers on the event stream replace LLM watchdog roles:

| Event | Handler |
|---|---|
| lease expired | reclaim, or mark stalled with a recovery packet |
| session exited | apply §3.1 exit rules |
| gate opened (CI green, human approval) | advance the formula or merge train |
| budget threshold | pause dispatch; notify through the inbox |
| agent stalled longer than N | ask a model once for a diagnosis; post it to the inbox |

### 3.4 Path reservations at dispatch

- Before dispatch, reserve the paths the issue predicts. Proposed order of
  sources (open question 2): paths in the issue's `design` field; else files
  that similar past issues touched (starfix's files-to-issues index); else
  reserve on first write, with `bearingd` watching the worktree. No model call
  until data shows one is needed.
- The dispatcher picks a wave of issues whose reservations do not overlap.
  Overlapping work waits or is serialized.
- Agents widen or narrow their reservation through the starfix MCP `reserve`
  tool as they learn more. Reservations expire with the claim.

### 3.5 Safe merge train

- A starfix lock (FIFO, leased) guards each target branch.
- Batch ready branches, test the batch tip, bisect on red, and eject the
  culprit back to its issue with the failure attached.
- Gates come from the starfix gate runner: required CI checks and, by default,
  human approval.
- **Queue only until the server runs.** The train orders, tests and opens pull
  requests; a human merges. Merging by the train waits for the server
  `bearingd`, is then opt-in per project, and still requires green CI. Never
  force-push; never push to a protected branch.
- Only the merge train holds push credentials. Agents never do.

### 3.6 Structured handoff

When a session ends or its context fills, the supervisor asks the agent for a
`handoff` record (state, next step, branch, worktree, open questions), or writes
a minimal one itself from git and the event log. The next claimant receives it
in `prime` with the pinned and relevant memories. Provider-neutral; no session
forking.

### 3.7 Budget and concurrency governor

- Server-side semaphores cap active agents per developer, project and provider.
- Spend budgets in tokens or dollars per issue, per formula run and per day,
  using starfix's token capture and price table (§12.1). Subscription plans use
  the amortized cost.
- At a threshold the governor stops dispatching, lets running agents finish
  their current step, and posts to the inbox.

### 3.7.1 Accounting split

starfix owns the accounting: token usage per issue, session and `account`, the
price table, list-price and amortized cost, `starfix cost`, `starfix log` and
the MCP `cost` tool (starfix §12.1). Bearing keeps no ledger. It does two
things:

- **Capture.** Because it launches each agent, Bearing reads the harness's
  usage output at the end of each turn or session and reports it to starfix
  against the claimed issue. This covers harnesses whose hooks cannot report
  usage.
- **Enforce.** The governor reads cost back from starfix to apply budgets.

The numbers are the same whether work ran under Bearing or by hand.

### 3.7.2 Plans and credentials

Bearing never calls a model API itself. It launches the providers' own CLIs
under whatever login they have, an API key or a subscription plan. Judgment
calls (§3.3) go through the same CLI in headless mode.

- Parallel agents on one subscription share its usage limits. A limit is the
  `blocked{usage_limit, until}` state (§3.2), not a failure, and the governor
  caps concurrency per provider.
- Unattended servers need a login on that host. Check each provider's terms
  for unattended and parallel use; API keys are the usual fit for fleets.

### 3.8 MCP inbox instead of keystrokes

Nudges, mail and operator instructions go to the starfix inbox. The agent reads
them through its MCP `inbox` tool, or a session-start or prompt-submit hook that
reads the local starfix cache. tmux, if used at all, is a viewer.

### 3.9 Dispatch policy

- `ready --claim` with work-in-progress limits per agent and per pool.
- Label affinity: a label can require a provider or model.
- Priority aging so old low-priority work eventually runs.
- Steal a claim only after its lease expires.
- Pools scale from ready-queue depth, within the governor's caps.

### 3.10 Formula runner

TOML workflow templates instantiated as starfix molecules with idempotency keys,
so a retried start never duplicates a run. Steps advance on events. A step can
require a gate: CI, a human approval, or a named reviewer.

### 3.11 Observability

- `bearing who`: agents, state, claim, lease remaining, spend.
- `bearing board`: live TUI (bubbletea) over the event stream.
- `bearingd --http`: a server-rendered status page (`html/template`), no
  JavaScript framework.
- The starfix `digest` covers "what happened in the last N hours".
- OpenTelemetry traces and metrics, optional.

### 3.12 Sandboxed workers

Run each agent in a container with only its worktree mounted and a network
allowlist. Containers, not bubblewrap, so one design runs on macOS (most
developers, through the local container runtime's VM) and Linux (servers). Inject a session-scoped credential that starfixd maps to
(principal, session). No ambient `~/.ssh`, no git push rights. Unrestricted
agent permissions are allowed only here.

## 4. Configuration

One TOML file per project, `bearing.toml`, plus a per-machine file for local
limits. Sketch:

```toml
[providers]
allow = ["claude-code", "codex", "gemini"]   # anything else is refused

[pools.default]
provider = "claude-code"
max = 4
labels = ["backend", "docs"]

[budget]
per_issue_usd = 20
per_day_usd = 150

[merge]
mode = "pr"            # "pr" (default) or "train"
require = ["ci", "human"]

[sandbox]
mode = "none"          # "none" or "container"
```

## 5. What Bearing does not build

| Not built | Why |
|---|---|
| LLM supervisor hierarchy (Mayor, Deacon, Witness, Dogs as agents) | Token cost and drift; Go patrol does it (§3.3) |
| A mandatory planning agent as the only interface | Optional. The CLI, inbox and digest are the interface |
| Identity from path, cwd or environment variables | Forgeable; identity is principal plus session |
| tmux as IPC or a hard dependency | Fragile; the inbox replaces it |
| Mail stored as issues | starfix has an inbox |
| Federation, reputation, ledgers | Conflicts with one authority |
| Release workflows that push upstream with user credentials | Only the merge train pushes, and only to unprotected branches |
| Autonomous merge to main by default | Default is PR plus human gate |
| Unrestricted permissions by default | Only inside a sandbox |
| Per-operation CLI execs, polling dashboards | Source of orphans and connection exhaustion |
| Non-Go UI or helpers | Go only |
| Many providers on day one | Start with Claude Code, Codex and Gemini |

## 6. Plan

Bearing stages depend on starfix stages ([starfix.md §13](starfix.md#13-plan)).

| Stage | Delivers | Needs starfix |
|---|---|---|
| B0 | Supervisor (§3.1), typed state (§3.2), `bearing who`, provider allowlist, one provider (Claude Code) | 3 (leases, agents, inbox, SSE) |
| B1 | Event-driven patrol (§3.3), handoff (§3.6), inbox delivery (§3.8), Codex and Gemini | 3 |
| B2 | Dispatch policy and pools (§3.9), governor (§3.7), live board (§3.11) | 3–4 (cost) |
| B3 | Reservations (§3.4), merge train in PR mode (§3.5), formula runner (§3.10) | 6 (locks, reservations, gates, molecules) |
| B4 | Sandboxed workers (§3.12), HTML status page, OpenTelemetry | 3 |
| B5 | Server `bearingd` running unattended; merge train may merge (opt-in) | 6 |
| — | Gas City shim: an `exec:` beads provider backed by starfix, so Gas City users can try it | 1 (issues, deps, ready) |

Gate for B0: a dozen agents across two machines run for a day with no stuck
claims, no orphaned processes and no reaped live agents.

## 7. Decisions

| # | Decision |
|---|---|
| 1 | Developer machines first; a server `bearingd` follows once proven, for work while no developer is online (§2). |
| 2 | The merge train only queues, tests and opens pull requests until the server runs (§3.5). |
| 3 | Sandboxes are containers, on macOS and Linux (§3.12). |
| 4 | Build the Gas City shim (§6). |

## 8. Open questions

1. **Repository.** Recommended: inside starfix as `cmd/bearing` and
   `cmd/bearingd`, one Go module, one release tag for all binaries, so Bearing
   never ships against a protocol starfix does not speak. A separate repository
   would need a public, versioned client API and a compatibility matrix from
   day one. Split later if Bearing outgrows it.
2. **Reservation sources** (§3.4). The trade-off: over-reserving serializes
   work; under-reserving brings merge conflicts back; a model call adds tokens
   and latency to every dispatch. Recommended: the layered order in §3.4.
