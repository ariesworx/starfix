# How starfix works

This page explains the ideas behind every `sfx` command and MCP tool:
identity, issues, claims, handoffs, the inbox, the registry of who is at
work and memory.
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
  highest priority first, then oldest first, except that an issue whose
  files overlap work another session holds comes after the rest
  ([Files](#files-what-an-issue-touches)).
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
   issue, its acceptance checklist, the last handoff note, the
   [memories](#memory) relevant to it and a branch name such as
   `fix/sf-a1b2c3d4-fix-the-login-redirect`.
2. `finish` closes the issue, records a handoff note and files any work
   found on the way as new issues, linked `discovered-from`.

`sfx start --branch` also checks the branch out, and `--worktree DIR`
creates a worktree on it. The MCP `start` tool never runs git; the agent
does.

## Files: what an issue touches

The server keeps, for each issue, the paths its work is likely to touch,
so that two sessions are not sent to edit the same files. They come from
two sources:

- **Commit paths** are what the work did. `sfx` reads them from git and
  sends them: `sfx mcp` with a renewal (at most every five minutes per
  issue) and with `finish` and `handoff`, and a terminal's `sfx finish`,
  `handoff` and `away`. An issue's paths are those of the commits since
  the default branch whose `Starfix:` trailer names it, or, on the
  issue's own branch, every commit on it and the uncommitted files.
  Commits a merge brought in are not counted. Earlier holders' paths
  stay with the issue.
- **Declared paths** are what someone expects the work to touch, given
  with `sfx create --paths` or `sfx update --paths`, relative to the
  current directory. A path ending in `/` is a directory and covers
  everything under it. `update --paths`
  replaces the declared set, and an empty value clears it.

Paths are relative to the repository's root, with forward slashes. An
issue keeps at most 200 (the server's `paths_per_issue`); past that, the
oldest commit paths make way for new ones, and more declared paths than
that are refused.

`ready` ranks an issue down, never out, when one of its paths equals a
path of an issue another session holds, or when a declared directory of
either covers a path of the other. It names those issues (`overlaps
ID`), and `show` lists the likely files and who holds the overlapping
work. Your own session's claims do not count, nor does a path that more
than 10 held issues share, such as `go.mod`. `start` with no id takes
the first issue that overlaps nothing, if there is one.

Reading git is a hint, never a requirement: with no git, no repository
or a detached HEAD, `sfx` sends nothing and the work goes on.

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
| `claim.lost` | Your lease ran out and the server ended the claim, another session took it over, someone else closed or released the issue, or, after your lease ran out, someone changed its status or assignee |
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

`sfx tui` shows the same work live: what is ready, each held issue with
its holder and how long they have held it, what is blocked, and every
change as the server pushes it ([The live board](cli.md#the-live-board)).

## The digest

`sfx digest` summarizes a time window (`--since 24h` by default, `7d`, a
date or a time): what closed, started, is in progress or stalled, is
blocked, was handed off, created or discovered. An issue in progress with
no activity for 48 hours counts as stalled. The digest is structured data
for a standup or a status report. starfix runs no model; the agent writes
any narrative. It also totals the time issues were held in the window,
the tokens reported in it and their cost at list price
([below](#accounts-time-and-tokens)).

## Memory

A memory is something worth knowing in a later session: how deploys
work, a decision and its reason, a person's preferences. Each has a
**key**, a **body**, optional **tags**, an optional **issue** it is
about, and a **pin**. `sfx remember` and the MCP `remember` tool write
one, `recall` searches them and `forget` deletes one.

**Scopes.** Every memory has one of three:

| Scope | Who can read and change it |
|---|---|
| `project` (the default) | Everyone on the server |
| `team` | Everyone on the server |
| `user` | Its author alone |

A `user` memory is private, and the server, not the client, enforces
it: no other principal can recall, list, change or forget it, and its
key and body never appear in an issue's history, the digest, the events
the server pushes, `prime` or `start`. The digest's count of events does
include a change to one, without saying what it was. Two people's `user`
memories with the same key are two records.

One starfix server serves one project, so today `team` and `project`
reach the same people. `team` is for what holds across the team's
projects, such as a review rule, and will reach further once one server
holds several projects; the scope is stored, and `recall --scope`
filters by it, now. An agent remembers in `project` scope unless the
memory is the person's own preference, when it uses `user` and asks if
unsure, and uses `team` only when someone chose it.

**Keys and revisions.** A key names one memory in its scope. Each
memory has a revision, `rev`, that every change raises. `remember`
without `--rev` creates the key and is refused if it exists, naming the
stored revision. With `--rev N` it replaces revision N and is refused
if someone changed the memory since, so two edits of the same key never
overwrite each other silently. A replace sets the body, and leaves the
tags, the issue and the pin as they are unless it gives them; an empty
`--tag ''` or `--issue ''` clears them. New keys never conflict. `forget --rev
N` is refused the same way. Forgetting removes the memory but not its
revision count: remembering its key again continues from the next
revision, so a revision read before the forget does not match again.
A principal's oldest forgotten memories are deleted for good to make
room when new keys reach `memories_per_scope`; a key whose record is
gone starts again at revision 1.

**Recall** finds memories by text (in the key or body, in any case), by
exact key, by tag and by scope: pinned first, then the newest. There is
no search by meaning yet.

**Prime and start.** `prime` shows the pinned memories, then those
relevant to your in-progress issues (linked to one of them, or tagged
with one of their labels), then the newest, as many as fit its budget;
never in key order. `start` shows up to five memories relevant to the
issue it takes. Pinning is a person's call: `sfx pin KEY` and `sfx
unpin KEY`. Agents cannot pin.

**No secrets.** The server refuses a memory that looks like it holds a
credential, in every scope: a private key, an AWS access key id, a
GitHub, GitLab, Slack or Stripe token, a JSON web token, a bearer
token, a password in a URL or after `mysql -p`, a `password=` or
`secret:` assignment, or a long random token. The refusal names what
it looks like, never the text. Write where the secret is kept instead,
such as a vault path or an environment variable's name.

How large a memory may be, and how many each principal may keep, are
[server limits](server.md#limits).

## Accounts, time and tokens

Every issue reports its time and tokens against an **account**: a
client's engagement code name or an internal department, never a real
client name. Set one with `sfx create --account NAME` or `sfx update ID
--account NAME`; an issue without one inherits its parent's, up the chain,
and then the server's `account:` setting, `internal` by default. Account
names are lowercase letters, digits and inner hyphens.

**Time** is the time an issue was held under claims: from `start` until
`finish`, close, a releasing handoff, a takeover, or the lease running
out. Every harness gets it, even one that reports no tokens.

**Tokens** come from the harness, never from the model's own account of
them: the client sends what the harness recorded for each request (model,
input, output, cache write and cache read), keyed by the harness's own
request id, so sending a record twice stores it once. A count the harness
did not report stays unknown, which `show` leaves out rather than printing
0. So far `sfx` captures Claude Code's tokens, from its local transcripts
through hooks that `sfx setup claude-code` installs, and sends only the
counts ([Token usage hooks](agents.md#token-usage-hooks-claude-code)).
Other harnesses report time only until their capture is built.

**Attribution** is worked out when you read it, from the claim history,
and never stored. A request goes to the issue its session held at that
moment. A turn or session record that spans several issues is split by
time held, and `show` and `digest` say some tokens were split; a stretch
when the session held nothing stays unattributed, and `digest` counts it
apart.
Splitting by time is an estimate when a session switches issues. A
record is split in whole tokens, so the parts add up to the record: the
issues' tokens plus the unattributed ones are the total.

### Cost

**Cost** is the tokens' list-price equivalent: what they would cost at
each model's API rates, whatever plan pays for them. An admin records the
rates ([Prices](server.md#prices)); a token is priced by its model's
rate in effect at the record's time, so a price change never rewrites a
past cost. Cost is worked out when read, like attribution, and exactly:
an issue's share of a split record is priced as the tokens it got, so the
issues' costs add up to the record's. A model with no price is shown with
its tokens and marked unpriced, never guessed at.

`show` gives an issue's running cost, `digest` a window's, and `sfx cost
--since 7d --by account` a report by account, issue, epic (the nearest
epic up the parent chain), person (who reported the tokens) or model.
Tokens no issue was held for are `(unattributed)`, issues under no epic
are `(no epic)`, and a group is marked `split` when part of it came from
a record shared with another group. Invoicing stays outside starfix.
