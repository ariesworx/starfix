# Bearings: an agent orchestrator on starfix

Status: **draft specification**, open for review. Companion to
[starfix.md](starfix.md).

Bearings runs and supervises many AI coding agents across sessions and machines.
starfix holds all state: issues, claims, leases, memory, events and costs.
Bearings holds none. It is the deterministic Go layer that starts agents, keeps
their claims honest, routes work to them and lands their output safely.

The name follows starfix: a navigator fixes a position from several star
bearings, as Bearings lands one change from many agents.

The design draws on Gas Town and its successor Gas City, which proved the idea
and exposed its failure modes: claims that never release, reapers that kill live
agents, LLM supervisors that burn tokens, merges on red CI, and agents running
with every permission on the host. Each component below names the failure it
removes.

## 1. Principles

1. **Go supervises; LLMs judge.** Spawning, reaping, routing, budgeting and
   merging are deterministic Go reacting to events. A model is called only for
   a question that needs judgment, such as "why is this agent stuck?".
2. **Stateless.** Every fact lives in starfix. A Bearings restart loses nothing
   and adopts its running children.
3. **Event-driven, never polling.** Bearings subscribes to the starfix event
   stream (SSE). No status loops, no per-operation CLI processes.
4. **Identity is the SSH principal plus the session**, as in starfix. Never a
   path, a working directory or an environment variable.
5. **Safe by default.** Pull-request mode with a human gate. No merge without
   green CI, never a force-push, never a push to a protected branch. Agents run
   in their harness's allowlist mode; unrestricted permissions only inside a
   sandbox.
6. **Provider allowlist.** Bearings launches only providers and models an
   operator has allowed, and refuses everything else at config load.
7. **One static binary, Go only.** No Node, Python or TypeScript components. No
   hard dependency on tmux.
8. **Terse.** Same output rules as starfix: failures print, passes only with
   `-v`, one summary line, `--json` for machines.

## 2. Architecture

```
            operator: bearings CLI · TUI · HTML status page
                                │
 ┌──────────────── bearingsd (one per machine) ────────────────┐
 │ supervisor ─ patrol ─ dispatcher ─ governor ─ merge train   │
 │     │                                                       │
 │  agent CLIs (Claude Code, Codex, Gemini), one process group │
 │  and one git worktree each, optionally sandboxed            │
 └──────┬──────────────────────────────────────────────────────┘
        │ starfix client: MCP for agents, RPC + SSE for bearingsd
        ▼
     starfixd ─── Dolt
```

- `bearingsd` runs on every machine that runs agents, because it owns their
  processes and worktrees: a developer's laptop, a headless server, or both.
  Several can share one starfix server; that is the multi-machine story, and it
  mostly falls out of starfix's single authority and leases.
- **Deployment order.** Developer machines first, until Bearings is proven:
  supervise agents on the developer's machine under their own logins. Then a
  server `bearingsd` that keeps working when no developer is online. The merge
  train (§3.5) holds push credentials and the branch lock, so it runs in
  exactly one place, on that server.
- Agents talk to starfix through its MCP server. Bearings never types into an
  agent's terminal.
- `bearings` is the operator CLI. It talks to the local `bearingsd` over a unix
  socket.

## 3. Components

Each row names the starfix primitive it builds on and the Gas Town failure it
addresses.

| # | Component | Uses | Removes |
|---|---|---|---|
| 1 | **Supervisor with lease coupling** | claims, epochs, leases, agents registry | claims stuck `in_progress`; leases reading expired on live agents; orphaned processes |
| 2 | **Typed agent state** | agents registry, events | usage limits indistinguishable from hangs |
| 3 | **Event-driven patrol** | SSE events, reaper, gates | LLM patrol loops, heartbeat drift, idle token burn |
| 4 | **Conflict log; reservations later** | events; later reservations | rework from overlapping work, unmeasured |
| 5 | **Safe merge train** | locks, gates, events | merges on red CI, force-push recovery |
| 6 | **Structured handoff** | handoffs, scoped memory, prime | lost reasoning when a session ends |
| 7 | **Budget and concurrency governor** | semaphores, cost tracking (starfix §12.1) | rate-limit exhaustion, runaway spend |
| 8 | **MCP inbox instead of keystrokes** | inbox, SSE, `starfix setup` | send-keys fragility, provider-specific hooks |
| 9 | **Dispatch policy** | ready, claims, labels | first-come dispatch, manual assignment |
| 10 | **Formula runner** | molecules, idempotency keys, gates | duplicate workflow instances |
| 11 | **Observability** | events, agents, digest | many stores, polling dashboards |
| 12 | **Sandboxed workers** | principal and session mapping | host-wide credential access |
| 13 | **Epics** | parent links, gates, events, memory, cost | half-landed epics, stale bases, sibling drift |

### 3.1 Supervisor with lease coupling

- Spawn each agent CLI as a child with `Setpgid`, in its own git worktree, with
  a pty only when the harness needs one.
- While the PID is alive, renew the claim lease. Liveness is the process, not
  the model's willingness to call a heartbeat tool.
- On exit: if the agent finished, the issue is already closed. Otherwise
  release the claim, or mark it `stalled` with a handoff (§3.6) when work is in
  the worktree.
- On `bearingsd` restart, adopt live children by PID and session ID; reap the
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

### 3.4 Conflicts: worktrees first, reservations later

Every agent works in its own worktree and branch, and conflicts are resolved at
the pull request by rebasing. That is the safety net, and the first stages
rely on it alone.

- **Log every conflict.** When a branch conflicts, `bearingsd` records an event
  with the issues and files involved, and the rework it cost in tokens.
- **Reservations are deferred.** If the log shows rework is expensive, add
  reservations: before dispatch, reserve the paths an issue will touch, and hold
  overlapping issues until the first merges. Sources, in order: paths in the
  issue's `design` field; files similar past issues touched (starfix's
  files-to-issues index); reserve on first write. No model call unless data
  shows it is needed.

### 3.5 Safe merge train

- A starfix lock (FIFO, leased) guards each target branch.
- Batch ready branches, test the batch tip, bisect on red, and eject the
  culprit back to its issue with the failure attached.
- Gates come from the starfix gate runner: required CI checks and, by default,
  human approval.
- **Queue only until the server runs.** The train orders, tests and opens pull
  requests; a human merges. Merging by the train waits for the server
  `bearingsd`, is then opt-in per project, and still requires green CI. The
  one exception is an epic branch: from B3 the train may merge a child with
  green CI into `epic/*`, never `main` (§3.13). Never force-push; never push to
  a protected branch.
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
the MCP `cost` tool (starfix §12.1). Bearings keeps no ledger. It does two
things:

- **Capture.** Because it launches each agent, Bearings reads the harness's
  usage output at the end of each turn or session and reports it to starfix
  against the claimed issue. This covers harnesses whose hooks cannot report
  usage.
- **Enforce.** The governor reads cost back from starfix to apply budgets.

The numbers are the same whether work ran under Bearings or by hand.

### 3.7.2 Plans and credentials

Bearings never calls a model API itself. It launches the providers' own CLIs
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

- `bearings who`: agents, state, claim, lease remaining, spend.
- `bearings board`: live TUI (bubbletea) over the event stream.
- `bearingsd --http`: a server-rendered status page (`html/template`), no
  JavaScript framework.
- The starfix `digest` covers "what happened in the last N hours".
- OpenTelemetry traces and metrics, optional.

### 3.12 Sandboxed workers

Run each agent in a container with only its worktree mounted and a network
allowlist. Containers, not bubblewrap, so one design runs on macOS (most
developers, through the local container runtime's VM) and Linux (servers). Inject a session-scoped credential that starfixd maps to
(principal, session). No ambient `~/.ssh`, no git push rights. Unrestricted
agent permissions are allowed only here.

### 3.13 Epics: distributed work on one goal

An epic is a starfix issue of type `epic` whose children point to it with
`parent_id`. Bearings never dispatches the epic itself; it runs the children,
possibly on several machines at once, and lands them as one change. That is the
hardest coordination problem Bearings has, because siblings share a goal, often
share files, and depend on each other's code, not just each other's status.

**What goes wrong without a design**

| Failure | Cause |
|---|---|
| `main` holds half an epic | Each child lands on its own |
| A child builds on code that isn't there | Its blocker is *closed* but not yet *merged* where the child branches from |
| Siblings disagree on an interface | Each agent decides alone; nothing carries the decision to the others |
| Conflicts pile up late | Siblings touch the same area in parallel |
| Scope and spend creep | Agents file new children mid-epic and Bearings dispatches them |
| One failing child stalls the epic silently | Retries loop; siblings keep building on a broken base |
| The epic branch drifts from `main` | Long-lived branch, no one merges `main` back |

**Design**

1. **Plan, then approve.** A planning step (a person, or an agent at a person's
   request) splits the epic into children sized for one session each, with
   dependencies and an epic-level `design` (interfaces, conventions, out of
   scope). Dispatch starts only after a human approves the plan through a
   starfix gate. Parallelism comes from the dependency graph, so a poor split
   costs more than any scheduler can win back.
2. **One integration branch per epic.** The merge train creates
   `epic/<id>` from `main`. Children branch from its tip and land on it through
   the train (queue, test, then merge into the epic branch, since that branch is
   not protected). Only the final epic pull request to `main` goes to a human.
   Small epics can opt into `integration = "trunk"`: each child opens its own
   pull request to `main`, as for standalone issues.
3. **"Landed", not "closed", unblocks.** A child's dependents become dispatchable
   when it has *landed* on the epic branch, not when its agent closes it. The
   merge train records a `landed {issue, branch, sha}` event in starfix, and the
   dispatcher requires every blocker to be landed. Workers on any machine fetch
   the epic branch before creating a worktree, so they always start from code
   that contains their blockers.
4. **Shared epic context.** `prime` for a child includes the epic's `design`, the
   epic's decision log, and short handoffs from landed siblings. When an agent
   makes a decision others must follow (an interface, a name, a schema), it
   records it with the starfix MCP tools as an epic decision; Bearings pushes it
   to the inboxes of running siblings, whose next prompt sees it.
5. **Lower parallelism, measured conflicts.** Siblings collide more than
   unrelated issues, so each epic has its own cap (`max_parallel`, default 3)
   under the governor's global caps. The conflict log (§3.4) is grouped by
   epic; epics are where reservations would pay off first, if the log shows
   they are needed.
6. **Discovered work waits.** A child an agent files during the epic is created
   `deferred` with `discovered-from`. It is dispatched only after a human
   accepts it. A per-epic policy (such as "under N points and under budget")
   may replace the human later, once spend forecasts are trustworthy. Bugs
   outside the epic go to the backlog, not into the epic.
7. **Budget for the whole epic.** The governor caps spend across all children
   (`per_epic_usd`), forecasts remaining cost from children done so far, and
   stops dispatching when the forecast exceeds the cap, posting to the inbox.
8. **Failure stops the line, not the fleet.** A child that fails CI on the
   epic branch is ejected by the train and retried at most twice, with the
   failure attached to its handoff. Then it is marked `blocked{needs_human}`.
   Its dependents wait; independent siblings keep running.
9. **Keep the branch fresh.** When `main` moves, the train merges `main` into
   `epic/<id>` (never a rebase or force-push). A conflict there becomes a child
   issue at the head of the queue, and dispatch on that epic pauses until it
   lands.
10. **Close-out.** When every child has landed and the epic branch is green,
    Bearings opens the epic pull request to `main` and marks the epic
    close-eligible. The epic closes when that pull request merges. Cancelling an
    epic stops its agents, releases claims and keeps every branch.

**What starfix needs** (beyond stage 3): a `landed` event and "blockers landed"
in the ready query used for dispatch (stage 6, with gates); epic decisions as
epic-tagged memory (stage 4); `epic status` and close-eligible (bd parity);
`cost --by epic` (stage 4, already designed).

**Distributed by construction.** Nothing in this design depends on where an
agent runs. Claims, the landed event, decisions and budgets live in starfix;
code moves only through the epic branch on the git remote. A laptop and a
server can work the same epic, and a child resumed on another machine starts
from its handoff and the branch.

## 4. Configuration

One TOML file per project, `bearings.toml`, plus a per-machine file for local
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

[epics]
integration = "branch" # "branch" (epic/<id>, default) or "trunk"
max_parallel = 3
per_epic_usd = 300
discovered = "hold"    # "hold" (human accepts) or a policy name
require = ["ci", "human"]

[sandbox]
mode = "none"          # "none" or "container"
```

## 5. What Bearings does not build

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

Bearings stages depend on starfix stages ([starfix.md §13](starfix.md#13-plan)).

| Stage | Delivers | Needs starfix |
|---|---|---|
| B0 | Supervisor (§3.1), typed state (§3.2), `bearings who`, provider allowlist, one provider (Claude Code) | 3 (leases, agents, inbox, SSE) |
| B1 | Event-driven patrol (§3.3), handoff (§3.6), inbox delivery (§3.8), conflict log (§3.4), Codex and Gemini | 3 |
| B2 | Dispatch policy and pools (§3.9), governor (§3.7), live board (§3.11) | 3–4 (cost) |
| B3 | Merge train in queue mode, merging only into `epic/*` (§3.5), formula runner (§3.10), epics (§3.13) | 6 (locks, gates, molecules) |
| B4 | Sandboxed workers (§3.12), HTML status page, OpenTelemetry | 3 |
| B5 | Server `bearingsd` running unattended; merge train may merge (opt-in) | 6 |
| — | Reservations (§3.4), only if the conflict log shows rework is expensive | 6 (reservations) |
| — | Gas City shim: an `exec:` beads provider backed by starfix, so Gas City users can try it | 1 (issues, deps, ready) |

Gate for B0: a dozen agents across two machines run for a day with no stuck
claims, no orphaned processes and no reaped live agents.

## 7. Decisions

| # | Decision |
|---|---|
| 1 | Developer machines first; a server `bearingsd` follows once proven, for work while no developer is online (§2). |
| 2 | The merge train only queues, tests and opens pull requests until the server runs (§3.5). |
| 3 | Sandboxes are containers, on macOS and Linux (§3.12). |
| 4 | Build the Gas City shim (§6). |
| 5 | Worktrees and pull requests handle conflicts; log them, and defer reservations until the log justifies them (§3.4). |
| 6 | Bearings lives in this repository as `cmd/bearings` and `cmd/bearingsd`, in the same Go module, released under one tag with starfix, so it never ships against a protocol starfix does not speak. |
| 7 | Epics integrate on an `epic/<id>` branch by default; small epics may opt into `integration = "trunk"` (§3.13). |
| 8 | A human accepts discovered children; a per-epic policy may come later, once spend forecasts are trustworthy (§3.13). |
| 9 | A person or a planning agent may plan an epic; dispatch always waits for a human to approve the plan (§3.13). |
| 10 | From B3 the merge train may merge children with green CI into `epic/*`, never `main`, before the server runs; `main` still waits for a human. This narrows decision 2 (§3.5, §3.13). |

## 8. Open questions

None at present.

