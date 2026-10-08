# How starfix works

This page explains the ideas behind every `sfx` command and MCP tool:
identity, issues, claims, handoffs, the inbox and the registry of who is
at work.
The [CLI reference](cli.md) and the [agent guide](agents.md) build on it.

## Identity: principals and sessions

- A **principal** is a person or an agent identity, such as `alice`. The
  server decides it from the SSH key that connected, so a client cannot
  claim to be anyone else
  ([Security](security-model.md#identity-comes-from-ssh-keys)).
- A **session** is one run of an agent, or a person's terminal on one
  machine. Its id comes from the environment
  ([Sessions](agents.md#sessions)).

The names `starfixd` (the server's own actions, such as ending expired
claims), `import` (the bd importer) and `starfixd-upgrade` (the health
check `starfixd upgrade` runs) are reserved.

## Issues

An issue has an id such as `sf-a1b2c3d4`, a title, a type (`bug`,
`feature`, `task`, `epic` or `chore`), a priority from 0 (critical) to 4
(backlog) and a status: `open`, `in_progress`, `blocked`, `deferred` or
`closed`. It can also carry a body, labels, a parent issue, comments and
acceptance criteria.

Every change is recorded as an event in the same transaction as the change
itself. The event log is the history: `sfx history ID` reads it, and
`ready`, `blocked` and `digest` are computed from it when you ask.

## Dependencies: ready and blocked

`sfx dep add FROM TO` records that FROM depends on TO. A dependency of type
`blocks` (the default) or `conditional-blocks` holds FROM back until TO is
closed. The other types (`related`, `discovered-from`, `duplicates`,
`supersedes`, `waits-for`) only link issues.

- **Ready** issues are open, and nothing holds them back: no open blocker
  on the issue or on any parent, and nothing deferred. They come out
  highest priority first, then oldest first.
- **Blocked** issues are not closed and have at least one open blocker,
  their own or a parent's.

A `blocks`, `conditional-blocks` or `waits-for` dependency that would form
a cycle is refused.

## Claims and leases

Before an agent or a person works on an issue, they **claim** it with
`start`. The claim makes the issue `in_progress`, assigns it to you and
leases it to your session. Only a claim can set `in_progress`; `update`
cannot.

While the lease runs, other principals cannot change the issue. Their
`start` is refused and points them at the next ready issue. Their
`update`, `close`, `reopen`, `finish`, `handoff` and `accept` are refused
with `forbidden`, naming the holder. Anyone can still comment on it, label
it or link it.

| Who claims | Lease | Kept alive by |
|---|---|---|
| An agent, through MCP | 15 minutes | `sfx mcp` renews it every minute while the agent runs |
| A person, from a terminal | 8 hours (`--for`, at most 24 hours) | `sfx away 4h` extends all your claims, up to 7 days |

When a lease runs out, the server returns the issue to `open`. So a
session that crashes lets its work go within 15 minutes, and nothing stays
stuck.

A claim's **epoch** is a number that rises with each new holder, and
`show` prints it.
`finish` and `handoff --release` refuse a stale epoch, so a session that
lost its claim cannot close work that someone else has since taken.
`sfx mcp` sends the epoch of each claim it took; from a terminal, pass
`--epoch N` for the same check. Another session of your own takes over a
live claim only when you ask (`sfx start ID --take`). The same session
reconnecting keeps its claim.

**Admins** are principals named in the server's config. Only an admin may
change an issue another principal holds, which is recorded as an
`admin.override` event naming the holder, or force a close past open
acceptance items. See [Admins](server.md#admins).

## A session: start and finish

A unit of work is two steps:

1. `start` claims the top ready issue (or the one you name). It returns the
   issue, its acceptance checklist, the last handoff note and a branch name
   such as `fix/sf-a1b2c3d4-fix-the-login-redirect`.
2. `finish` closes the issue, records a handoff note and files any work
   found on the way as new issues, linked `discovered-from`.

`sfx start --branch` also checks the branch out, and `--worktree DIR`
creates a worktree on it. The MCP `start` tool never runs git; the agent
does.

## Acceptance criteria

The acceptance text of an issue becomes a checklist: each Markdown list
item (`- [ ] x`, `- x`, `1. x`) is one item, or the whole text is one item.
`sfx accept ID N` ticks item N; `--undo` unticks it, and
`--waive REASON` waives it. `finish` can tick or waive items in the same
step (`--tick 1,3`, `--waive 2=REASON`).

An issue with an open item cannot be closed. An admin's `close --force`
closes it anyway and records the open items in the event.

A `- [x]` box counts as ticked only in the text an issue is created or
imported with. Later edits to the text tick nothing.

## Handoffs

`sfx handoff ID NOTE` leaves a note for whoever continues. It can say how
far the work got (`--state done|partial|blocked`), what to do next
(`--next`), and which `--branch` or `--worktree` holds it. `--to P` puts
the handoff in P's inbox, and `--release` ends your claim and unassigns the
issue so someone else can start it.

`start` and `show` print the latest handoff. The worktree path is shown
only to your own principal, because it is a path on your machine.

## The inbox

Every principal has an inbox. The server adds an item in the same
transaction as its cause:

| Item | When |
|---|---|
| `claim.lost` | Your lease ran out and the server ended the claim, another session took it over, or someone else closed or released the issue |
| `handoff` | A handoff names you with `--to` |
| `mention` | A comment, handoff or finish note says `@you`. Only principals the server has seen can be mentioned, and never yourself |
| `assigned` | Someone else assigned you an issue |

`sfx inbox` lists unread items and `--ack` marks them read. `sfx watch`
prints them as they arrive. An agent's `sfx mcp` asks the server to push
new items, and adds `inbox: N new (call inbox)` to the agent's next tool
result.

## Who is at work

`sfx who` lists every session that connected or renewed in the last 5
minutes, with its principal, machine, harness and the issues it holds. The
server records a session when it connects, and `sfx mcp` keeps it present
while the agent runs.

## The digest

`sfx digest` summarizes a time window (`--since 24h` by default, `7d`, a
date or a time): what closed, started, is in progress or stalled, is
blocked, was handed off, created or discovered. An issue in progress with
no activity for 48 hours counts as stalled. The digest is structured data
for a standup or a status report. starfix runs no model; the agent writes
any narrative.
