# Using sfx

`sfx` is the starfix client. People use it as a command line, and agents
run it as an MCP server ([agent guide](agents.md)). This page covers
connecting a repository to a server and every `sfx` command. The ideas
behind the commands are in [How starfix works](concepts.md).

## Connect a repository

A repository connects to a server through a `.starfix.yaml` file in its
root. Commit it: it holds no secrets.

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix.example.com
  host_key: SHA256:…
```

| Field | Holds | Default |
|---|---|---|
| `project` | The project's UUID. It must match the server's `project:` setting | required |
| `server.host` | The server's host name or address | required |
| `server.port` | The SSH port | `22` |
| `server.user` | The account starfixd runs as | `starfix` |
| `server.host_key` | The server's ED25519 host key fingerprint, `SHA256:…` | required |
| `server.iap` | Reach the server through Google Cloud IAP ([below](#servers-without-a-public-ip-google-cloud-iap)) | off |
| `key` | A private key file, absolute, `~/`-relative or relative to the repository root | ssh-agent |

### The host key is pinned

`sfx` checks the server's host key on every connection and never trusts a
key on first use. Get the fingerprint from the server's admin, or read it
yourself over a channel you trust:

```sh
ssh-keyscan -t ed25519 starfix.example.com | ssh-keygen -lf -
```

On a cloud server, prefer a value the server publishes through its
provider's API (for example in instance metadata) over a scan across the
network.

### Your key is your identity

The server's admin adds your public key with your principal name
([Principals](server.md#principals)). `sfx` signs in with a key from
ssh-agent (the OpenSSH agent's named pipe on Windows), or with the file in
`key:`. It runs its own SSH client, so it does not use your `~/.ssh/config`.

### Where sfx looks for the file

`sfx` looks for `.starfix.yaml` in the working directory, then in each
parent. It stops at the repository's top level (the first directory that
holds `.git`) and never looks above your home directory. `-C DIR` starts
the search in DIR instead.

On macOS and Linux the file must be yours and writable by no one else.
`chmod go-w .starfix.yaml` fixes a checkout made under a umask of 002.
Windows has no owner check, so there the search boundary is the only
guard.

### Servers without a public IP (Google Cloud IAP)

A server on a Compute Engine instance with no external address can be
reached through Identity-Aware Proxy TCP forwarding. Add an `iap` block;
`server.host` then names the instance:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix-1
  host_key: SHA256:…
  iap:
    project: example-project
    zone: us-central1-a
```

| Field | Holds |
|---|---|
| `iap.project` | The Google Cloud project id |
| `iap.zone` | The instance's zone |
| `iap.instance` | The instance name; defaults to `server.host` |

For each connection, `sfx` runs
`gcloud compute start-iap-tunnel INSTANCE PORT --listen-on-stdin` and
speaks SSH through it, so nobody keeps a tunnel open by hand. The host key
is still pinned and checked. Each developer needs:

- the Google Cloud CLI on `PATH`, signed in with `gcloud auth login`;
- the IAP-secured Tunnel User role on the instance, and permission to look
  the instance up (Compute Viewer on the project is enough).

The instance's firewall must admit IAP's range, `35.235.240.0/20`, on the
SSH port. `sfx setup` warns when `gcloud` is missing.

There is no general proxy setting, on purpose: `.starfix.yaml` is
committed, so a command in it would run whatever a cloned repository says.

## Commands

```text
sfx [-C DIR] [--json] COMMAND [ARGS]
```

`sfx help COMMAND` prints a command's usage. `-C DIR` runs as if started
in DIR. `--json` prints exactly one JSON document, errors included.

### Work on an issue

| Command | Does |
|---|---|
| `start [ID]` | Claim an issue, the top ready one if you give no ID, and show it with its checklist, last handoff and branch name. `--for 4h` sets the lease (default 8h, at most 24h). `--branch` checks the branch out; `--worktree DIR` creates a worktree on it. `--take` takes the claim over from another session of yours |
| `finish ID` | Close your issue and end the claim in one step: `--reason`, a `--handoff` note (with the handoff fields below), and `--discovered TITLE` for each new issue found on the way. `--tick 1,3` and `--waive N=REASON` settle acceptance items first; finish is refused while any item is open. `--epoch N` refuses unless N is still the claim's epoch ([Claims and leases](concepts.md#claims-and-leases)). It sends the paths the issue's work touched, read from git ([Files](concepts.md#files-what-an-issue-touches)) |
| `accept ID N...` | Tick acceptance items. `--undo` unticks them; `--waive REASON` waives them |
| `handoff ID NOTE` | Leave a note for whoever continues. Fields: `--state done\|partial\|blocked`, `--next TEXT`, `--branch B`, `--worktree DIR`, and `--to P` to put it in P's inbox. `--release` ends your claim and unassigns the issue, and takes `--epoch N` like `finish`. It sends the issue's paths like `finish` |
| `away DURATION` | Extend all your claims, in every session, to at least now plus DURATION (up to 7d), for example before you go offline. Only from your own terminal (session `cli`): it is refused when `sfx` runs with another session id, as in an agent's session. It then sends the paths the work on each claim touched, read from git, one issue at a time |

### Find and change issues

| Command | Does |
|---|---|
| `create TITLE` | Create an issue and print its id: `-p N` priority, `-t TYPE`, `--body TEXT` (or `-` for stdin), `--parent ID`, `--account NAME`, `--label L`, and `--paths P,P` for the files the work will touch, relative to the current directory (a directory ends in `/`). Similar closed issues, if any, are listed on stderr |
| `show ID` | Show an issue with its dependencies, claim, account, time held and tokens by model ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)), acceptance checklist, latest handoff, likely files (declared first, then the most recent 20) with the issues others hold that overlap them, and similar closed issues. `--compact` is shorter |
| `list` | List issues that are not closed, oldest first, 50 at a time. `-n N` sets the page size (up to 500); `--cursor C` continues, as the `more:` line on stderr shows. Filters: `--status S,S`, `--all` (closed too), `-t TYPE`, `-p N,N`, `--assignee A`, `--parent ID`, `--label L` |
| `ready` | List issues nothing holds back, best first. An issue whose files overlap work another session holds comes last, with `overlaps ID` ([Files](concepts.md#files-what-an-issue-touches)) |
| `blocked` | List issues held back by open blockers |
| `update ID` | Change fields (`--title`, `--body`, `--status`, `-p`, `-t`, `--assignee`, `--parent`, `--account`, …). `--account NAME` sets the account the issue's time and tokens report against; `--account ""` clears it, to inherit the parent's. `--paths P,P` replaces the declared paths and `--paths ""` clears them. `--rev N` applies the change only if the issue is still at revision N. It cannot set `in_progress` (claim with `start`), change the status or assignee of a claimed issue, or drop an acceptance item that is still open |
| `close ID`, `reopen ID` | Close (with `--reason`) or reopen an issue. Close is refused while an acceptance item is open; `--force`, for admins only, closes anyway |
| `dep add\|rm FROM TO` | Add or remove a dependency: FROM depends on TO. `--type T` (default `blocks`) |
| `label add\|rm ID LABEL...` | Add or remove labels |
| `comment ID TEXT` | Add a comment (`-` reads stdin) |
| `comments ID`, `history ID` | List all of an issue's comments, or all its changes; `-n N` shows only the newest N |

### See what is happening

| Command | Does |
|---|---|
| `inbox` | List your unread inbox, newest first: 20 items, or `-n N` (at most 100). `--all` includes read items; `--ack ID` (repeatable or comma-separated) or `--ack-all` marks items read |
| `watch` | Print inbox items as they arrive, until ctrl-c (exit 0). With `--json`, one object per line: `{"op":"inbox","item":{…}}`, or `{"op":"resync"}` when it missed items (`sfx inbox` lists them) |
| `tui` | Show the [live board](#the-live-board) until `q` |
| `who` | List the sessions seen in the last 5 minutes (`--since 2h`, up to 7d) and the issues each holds; at most 100 (`-n N`, up to 500), then a count of the rest |
| `digest` | Summarize a window: `--since 24h` (default), `7d`, a date or a time; filter with `--by PRINCIPAL` or `--label L`. Under its header it prints the time issues were held (saying when some tokens were split by time), the tokens reported by model, and those no issue was held for |
| `prime` | Orient a session: your issues in progress, your inbox, the top ready work and version notices. `--hook[=AGENT]` is for an agent's session-start hook ([agent guide](agents.md#session-start-hooks)) |

### Agents and maintenance

| Command | Does |
|---|---|
| `mcp` | Serve the MCP tools for an agent on stdin and stdout ([agent guide](agents.md)) |
| `setup AGENT` | Set an agent up for this repository; see [Set up an agent](agents.md#set-up-an-agent) |
| `usage --hook[=AGENT]` | Send the token counts in the session's transcripts that earlier runs have not sent. Run by Claude Code's Stop, SubagentStop and SessionEnd hooks, which `setup claude-code` installs, with the hook's JSON on stdin; bare `--hook` is Claude Code's, the only agent captured so far. It always exits 0 and sends only counts ([Token usage hooks](agents.md#token-usage-hooks-claude-code)) |
| `upgrade` | Replace `sfx` with the latest release after checking its signature and checksum. `--check` reports and changes nothing; `--rollback` restores the binary the last upgrade replaced. It never runs on its own |
| `version` | Print the version and protocol |

## The live board

`sfx tui` fills the terminal with a board that keeps itself current:

- **Ready**: the issues nothing holds back, best first, up to 100. An
  issue whose files overlap work another session holds is marked
  `[overlaps ID]` ([Files](concepts.md#files-what-an-issue-touches)).
- **Held**: each issue under a live claim, with its holder as
  `principal/session` and how long they have held it, on the server's
  clock. `[lapsed]` marks a lease that ran out before the reaper freed
  the issue.
- **Blocked**: the issues held back by open blockers, `[by ID +N]`.
- **Events**: every change to an issue since the board opened, newest at
  the bottom, with its time, who made it and what it was.

From 60 columns the board is two columns, ready and blocked beside held,
with the events below; narrower, it is one.

| Key | Does |
|---|---|
| `j`, `k`, arrows | Move the selection |
| `PgUp`, `PgDn`, `g`, `G` | Move a page, or to the top or bottom |
| `Enter`, right arrow | Open the selected issue: its fields, claim, dependencies, files, checklist, handoff and body |
| `Esc`, left arrow, `Backspace` | Go back |
| `r` | Read everything again |
| `?` | Show the keys and marks |
| `q`, `Ctrl-C` | Quit |

The board rides the same SSH connection as every other command, so the
server opens no port for it. It asks the server to push every change to
an issue, and reads the lists again only when a change can alter them,
once per burst; an open issue is read again when a change names it. It
never polls: the only timer redraws relative times each second. If the
connection drops, the header says `reconnecting`, and the board redials
(after 1 s, then doubling to 30 s) and reads everything afresh. The board
is read-only.

`sfx tui` needs a terminal on standard input and output. Anywhere else it
refuses and points at `sfx ready`, `sfx blocked` and `sfx list`. It
honors [`NO_COLOR`](https://no-color.org): set, the board draws without
color and marks the selection with `>` alone. Like all output, text from
the server is escaped (below).

## Exit codes

| Code | Means |
|---|---|
| 0 | Success |
| 1 | Failure. stderr ends with a `fix:` line saying what to do next |
| 2 | Usage error: an unknown flag or a missing argument |
| 3 | The server refused this client's protocol version; upgrade one side |

A refusal exits 1 like any other failure, and `--json` gives its code.
When another principal holds an issue, a change to it is refused with
`forbidden`, as is a change reserved for admins. `start` is refused with
`conflict` instead, and names the next ready issue. A request past one of
the server's limits is refused with `invalid` or `busy`
([Limits](server.md#limits)).

## Text from others is shown safely

Issue text comes from other people and their agents. `sfx` prints any
control or bidirectional character as a visible escape (`\x1b`,
`\u202e`), so text cannot clear your terminal or disguise itself as
`sfx`'s own output. [Security](security-model.md#untrusted-text) has the full
rules.
