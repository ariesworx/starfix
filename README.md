# starfix

[![ci](https://github.com/ariesworx/starfix/actions/workflows/ci.yml/badge.svg)](https://github.com/ariesworx/starfix/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Issue tracker, shared memory and coordination for AI coding agents working
across sessions and machines. Agents use it through MCP; people administer it
from the command line. It is written in Go and stores its data in
[Dolt](https://github.com/dolthub/dolt) behind a small server.

The repository also holds the design for **Bearings**, an orchestrator that runs
and supervises many agents on top of starfix.

> **Status: stage 3 of 7 in progress.** Issues work end to end over SSH, bd
> backlogs import and export, and agents use starfix through MCP, with
> leased claims, a registry of who is at work, and an inbox the server
> pushes. Memory and the offline cache follow. Not ready for production use.

## Why starfix

- **One authority, no merge surprises.** Clients send operations to one
  server, which orders them. There is no multi-master database merge.
- **Claims that can't go stale.** Every claim carries a lease and an epoch. The
  server expires leases on its own clock and refuses writes from a stale epoch,
  so a crashed or recycled agent never holds work forever.
- **Identity from your SSH key.** The server's sshd decides who you are. The
  client can't claim to be someone else, and nothing depends on paths or
  environment variables.
- **MCP for agents, CLI for people.** An agent never needs a shell; an admin
  never needs MCP.
- **Fails loudly.** Typed exit codes, typed errors, and a `fix:` line on every
  failure. No silent empty database.
- **Built for context budgets.** Compact results by default; writes return only
  `{id, rev}`.
- **Cost-aware.** Token use per issue, priced at list and amortized subscription
  rates (planned, stage 3–4).

## How it works

```text
 agent ──MCP──▶ sfx (CLI + MCP, local cache)
 person ─CLI──▶        │
                       │ SSH, pinned host key; in-process, no system ssh
                       ▼
                server sshd ── forced command: starfixd stdio --principal NAME
                       │ unix socket (0700 directory, same Unix user)
                       ▼
                starfixd serve ── Dolt sql-server (loopback)
```

- `starfixd serve` owns the store and listens on a unix socket.
- `starfixd stdio` is the sshd forced command. The principal comes from which
  key authenticated, never from the client.
- Frames are newline-delimited JSON. Each connection opens with a version
  handshake: an incompatible protocol is refused, and an older client is warned.

## Components

| Binary | Runs on | Purpose | Status |
|---|---|---|---|
| `sfx` | Linux, macOS, Windows | CLI for people; MCP server for agents | Built |
| `starfixd` | Linux (Windows via WSL2) | Server daemon and sshd bridge | Built |
| `bearings`, `bearingsd` | Linux, macOS (Windows via WSL2) | Agent orchestrator | Design ([spec](docs/design/bearings.md)) |

The client command is `sfx`, short for starfix; the project and the server keep
the full name.

`starfixd serve` refuses to run off Linux, because only Linux lets it check
which user connects to its socket. `--dev` overrides that on a single-user
machine.

## Install

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
```

It installs the latest `sfx` to `~/.local/bin` after checking the release's
Ed25519 signature (with OpenSSL 3; without it, it warns and checks only the
sha256) and the archive's sha256, and prints the line to add if that
directory is not on your `PATH`. It never uses sudo or edits your shell
files. Options go after `sh -s --`:

| Option | Does |
|---|---|
| `--version vX.Y.Z` | Install that release instead of the latest |
| `--dir DIR` | Install into DIR (default `~/.local/bin`) |
| `--server` | Install `starfixd` instead (Linux) |
| `--require-signature` | Fail rather than warn when openssl cannot check the signature |

From then on `sfx upgrade` and `starfixd upgrade` fetch, verify and install
new releases (Commands).

Or install by hand: each [release](https://github.com/ariesworx/starfix/releases)
carries `sfx` for Linux, macOS and Windows and `starfixd` for Linux, on
amd64 and arm64, as `<bin>_<version>_<os>_<arch>.tar.gz` (`.zip` on
Windows), with `checksums.txt` and its Ed25519 signature
`checksums.txt.sig`. Unpack the binary onto your `PATH`. On Windows, use the
zip, or the script inside WSL.

`go install github.com/ariesworx/starfix/cmd/sfx@latest` builds the same
code from source, unsigned, into `$(go env GOBIN)` or `$(go env GOPATH)/bin`;
under a version manager such as mise that is the manager's own directory,
which changes with the Go version. Prefer the script.

To verify a download by hand, work from a checkout of the release's tag, so
the public keys come from the repository rather than the download:

```sh
git clone --branch v0.3.0 https://github.com/ariesworx/starfix && cd starfix
gh release download v0.3.0 -p checksums.txt -p checksums.txt.sig -p 'sfx_0.3.0_linux_amd64.tar.gz'
go run ./internal/tools/releasekey verify checksums.txt   # the signature, against internal/release/keys.go
sha256sum --ignore-missing -c checksums.txt               # the archive; shasum -a 256 -c on macOS
gh attestation verify sfx_0.3.0_linux_amd64.tar.gz --repo ariesworx/starfix   # build provenance
```

Releases are signed in CI with a key only the maintainer's approved
release runs can use; [RELEASING.md](RELEASING.md) has the details and the
trade-off.

## Quick start

First lock Dolt down. A `dolt sql-server` started without a config lets
`root` in from localhost with no password, grants it `FILE`, and leaves
`secure_file_priv` empty, so any user on the host could skip starfixd,
rewrite the event log as anyone, and read or write files as Dolt's user.
starfixd refuses to start on such an account (below). Run Dolt under its own
Unix user, listening on loopback and a socket, with file access off:

```sh
cat > /var/lib/dolt/config.yaml <<'YAML'
data_dir: /var/lib/dolt/data
listener:
  host: 127.0.0.1                  # loopback only
  port: 3306
  socket: /run/dolt/dolt.sock
system_variables:
  secure_file_priv: /var/lib/dolt/no-files   # a directory that does not exist
YAML
dolt sql-server --config /var/lib/dolt/config.yaml   # under systemd, as the user dolt
```

Then, once, as Dolt's `root`, create starfix's database and an account
with rights on it alone, and close `root`:

```sql
CREATE DATABASE starfix;
CREATE USER 'starfix'@'localhost' IDENTIFIED BY 'PASSWORD';
GRANT ALL ON starfix.* TO 'starfix'@'localhost';
ALTER USER 'root'@'localhost' IDENTIFIED BY 'A-LONG-RANDOM-PASSWORD';  -- or DROP USER it
```

On the server, as the Unix user starfixd runs as (here `starfix`):

```sh
go install github.com/ariesworx/starfix/cmd/starfixd@latest   # then install it as /usr/local/bin/starfixd
cat > /etc/starfix/starfixd.yaml <<'YAML'   # chmod 600: it holds the DSN
dsn: starfix:PASSWORD@unix(/run/dolt/dolt.sock)/starfix
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f   # any UUID; uuidgen | tr A-Z a-z
socket: /run/starfix/starfixd.sock
# systemd_unit: starfixd.service   # lets `starfixd upgrade` restart and health-check it
# log_level: info                  # debug adds a line per request; warn, error
# log_format: text                 # or json, for a log shipper
# admins: [alice]                  # may change issues others hold and force a close
# limits: {write_rate: 10, write_burst: 100}   # see "Limits"
YAML
starfixd serve        # run it under systemd
```

Every command that opens the store (`serve`, `import-bd`, `export-bd`,
`upgrade`) checks the account first and refuses, with the SQL above as the
fix, when it is `root`, holds any privilege on `*.*` or `GRANT OPTION`, or
when `secure_file_priv` is empty. On a developer's own machine,
`--dev --allow-unsafe-dolt` lets a default Dolt through with a warning;
`--allow-unsafe-dolt` is refused without `--dev`, and no config file or
environment variable can set it.

Give each developer one line in `~starfix/.ssh/authorized_keys`. The forced
command, with an absolute path, fixes who they are; the client cannot
choose:

```text
restrict,command="/usr/local/bin/starfixd stdio --principal alice" ssh-ed25519 AAAA… alice@example.com
```

Harden sshd for that account too (OpenSSH; adjust the account name):

```text
# /etc/ssh/sshd_config.d/starfix.conf
Match User starfix
    AuthenticationMethods publickey
    DisableForwarding yes         # no tunnel to Dolt, even from a line missing restrict
    PermitTTY no
# globally:
MaxStartups 10:30:60          # drop unauthenticated floods early
PerSourcePenalties yes        # OpenSSH 9.8+: back off sources that fail or crash sessions
```

Lock the account's password (`passwd -l starfix`), and rate-limit new
connections at the firewall (`ufw limit 22/tcp`). Do not add a
`ForceCommand`: it would override each key's `command=` and so its
principal; keep every key line in the `restrict,command=` form instead.
For a team on known machines, a WireGuard tunnel in front of port 22 is a
good optional layer: sshd then listens only on the tunnel's address, and
nothing on the internet reaches it. starfix neither needs nor configures it.

In the repository, commit a `.starfix.yaml`. It holds no secrets:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix.example.com
  port: 22                 # default
  user: starfix            # default
  host_key: SHA256:…       # ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -
# iap: {project: example-project, zone: us-central1-a}   # optional; see below
# key: ~/.ssh/id_ed25519   # optional; ssh-agent is used otherwise
```

Then, on each developer machine:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
sfx create "Fix the login redirect" -p 1 -t bug
sfx ready
sfx update sf-a1b2c3d4 --status in_progress
sfx dep add sf-a1b2c3d4 sf-e5f6g7h8     # a1b2… depends on e5f6…
sfx close sf-a1b2c3d4 --reason "fixed in #12"
```

The host key is pinned, never trusted on first use. Keys come from ssh-agent
(the OpenSSH named pipe on Windows) or the `key:` file.

`sfx` finds `.starfix.yaml` in the working directory or a parent, but
stops at the repository's top level (the first directory holding `.git`)
and never looks above your home directory. On macOS and Linux the file
must be yours and writable by no one else; `chmod go-w .starfix.yaml`
fixes a checkout made under a umask of 002. Windows has no owner check, so
there the search boundary is the only guard.

### Servers without a public IP (Google Cloud IAP)

A server on a Compute Engine instance with no external address is reached
through Identity-Aware Proxy TCP forwarding. Add an `iap` block, and
`server.host` becomes the instance name:

```yaml
server:
  host: starfix-1
  host_key: SHA256:…
  iap:
    project: example-project
    zone: us-central1-a
    instance: starfix-1    # optional; defaults to server.host
```

| Field | Holds |
|---|---|
| `iap.project` | The Google Cloud project id |
| `iap.zone` | The instance's zone |
| `iap.instance` | The instance name; defaults to `server.host` |

`sfx` then runs `gcloud compute start-iap-tunnel INSTANCE PORT
--listen-on-stdin` itself for each connection and speaks SSH through it, so
nobody keeps a tunnel open by hand. The host key is still pinned and
checked. Each developer needs the Google Cloud CLI on `PATH`, signed in
with `gcloud auth login`, and the IAP-secured Tunnel User role on the
instance; the firewall must admit IAP's range (35.235.240.0/20) on the SSH
port. `sfx setup` warns when `gcloud` is missing.

There is no general proxy command setting, on purpose: `.starfix.yaml` is
committed, so a command in it would run whatever a cloned repository says.

## Moving from bd

Export with `bd export -o bd.jsonl`, copy it to the server and, as the
starfixd user:

```sh
starfixd import-bd --dry-run bd.jsonl   # what would change; writes nothing
starfixd import-bd bd.jsonl             # safe to rerun
starfixd export-bd -o back.jsonl        # bd's format, for bd or a later import
```

bd IDs are kept, so commits and branches that cite them still resolve. A
stored issue edited in starfix after the export is not overwritten. bd types
and statuses starfix lacks are kept as `bd-type:` and `bd-status:` labels and
restored on export. Whatever starfix cannot hold yet (some dependency types,
memories, a few fields) is listed with the IDs it affects, never dropped
silently; the mapping is in `internal/bdimport`. Control and bidirectional
characters, which starfix refuses (see Untrusted text), are removed with a
`text` warning naming the field, rather than failing the issue.

## Commands

### `sfx`

`sfx [-C DIR] [--json] COMMAND [ARGS]`. `sfx help COMMAND` shows a
command's usage; `--json` prints one JSON document, errors included.

| Command | Does |
|---|---|
| `start [ID]` | Claim an issue (the top ready one without ID) for `--for` (default 8h, at most 24h) and show it with its last handoff and branch; `--branch` checks the branch out, `--worktree DIR` makes a worktree on it; `--take` takes it over from another session of yours that holds it |
| `finish ID` | Close your issue with `--reason`, a `--handoff` note and `--discovered TITLE` work, in one step; ends the claim. The note can carry the handoff fields below. `--tick 1,3` and `--waive N=REASON` settle acceptance items first; it is refused while any is open |
| `accept ID N...` | Tick acceptance items (`--undo` unticks, `--waive REASON` waives them). Items come from the acceptance text: each Markdown list item (`- [ ] x`, `- x`, `1. x`), or the whole text as one; `start` and `show` print them as a checklist. A `- [x]` box counts as ticked only in the text an issue is created or imported with; later edits to the text tick nothing |
| `handoff ID NOTE` | Leave a note for whoever continues; `--release` ends the claim and unassigns it so another can start it. Optional fields: `--state done\|partial\|blocked`, `--next TEXT`, `--branch B`, `--worktree DIR`, and `--to P`, which puts it in P's inbox. `start` and `show` print the latest; the worktree, a path on your machine, only to your own principal |
| `inbox` | List your unread inbox, newest first: lost claims, handoffs to you, mentions, assignments (`--all` includes read ones, `-n N`); `--ack ID`, repeatable or comma-separated, or `--ack-all` marks them read |
| `watch` | Print your inbox items as they happen, until interrupted (ctrl-c exits 0). With `--json`, one object per line: `{"op":"inbox","item":{…}}`, or `{"op":"resync"}` when it fell behind and missed items (`sfx inbox` lists them) |
| `away DURATION` | Extend all your claims, in every session, to at least now plus DURATION (up to 7d), for example before going offline. Only from your own terminal: the server refuses it from an agent's session (`STARFIX_SESSION` or a harness session id set) |
| `who` | List the sessions seen in the last 5 minutes (`--since 2h`, up to 7d): principal, session, machine, harness, when last seen and the issues each holds; at most 100 (`-n N`, up to 500), then a count of the rest. Every principal sees every machine name |
| `create` | Create an issue and print its id; similar closed issues, if any, go to stderr |
| `show` | Show an issue, its dependencies, acceptance checklist and similar closed issues (`--compact` for short) |
| `list` | List open issues (`--status`, `--all`) |
| `ready` | List issues nothing holds back |
| `blocked` | List issues held back by open blockers |
| `update` | Change fields; `--rev N` makes it a strict compare-and-swap. It cannot set `in_progress` (`start` does), change the status or assignee of a claimed issue, or drop an acceptance item that is still open |
| `close`, `reopen` | Close or reopen an issue. Close is refused while an acceptance item is open; `--force`, for admins only, closes anyway and records the open items in the event |
| `dep` | Add or remove a dependency: FROM depends on TO |
| `label` | Add or remove labels |
| `comment`, `comments` | Add a comment; list an issue's comments, all of them, read a page at a time (`-n N`: only the newest N) |
| `history` | List an issue's changes, as `comments` (`-n N`) |
| `digest` | Summarize a window (`--since 24h`, `7d`, a date or a time): closed, started, in progress, stalled, blocked, handed off, created and discovered; `--by P`, `--label L` filter it |
| `prime` | A session's orientation: your in-progress issues, inbox, top ready work, version notices; `--hook[=AGENT]` for an agent's SessionStart hook (bare `--hook` is Claude Code's) |
| `mcp` | The MCP server for agents, on stdin and stdout |
| `setup AGENT` | Set up `claude-code`, `codex`, `cursor`, `gemini`, `jetbrains`, `junie` or `vscode`: MCP config (with `STARFIX_HARNESS` in its env), instruction pointer, SessionStart hook |
| `setup claude-desktop` | Register this project with Claude Desktop (macOS, Windows) in its user-global config, one entry per project |
| `setup --all` | Set up every agent at once (not `claude-desktop`), one summary line each; a file two agents share is written once |
| `upgrade` | Replace `sfx` with the latest release after verifying its signature and checksum; `--check` prints one line and changes nothing; `--rollback` restores the binary the last upgrade replaced. Never runs by itself |
| `version` | Print the version |

Exit codes: 0 ok; 1 failure, with a `fix:` line; 2 usage; 3 protocol version
refused. A refusal because someone else holds the issue, or because the
change is for admins, exits 1 like any other; `--json` gives its code,
`forbidden`. So does one past the server's limits (`busy`, with how long to
wait; see Limits).

#### Untrusted text

Issue text comes from other people, their agents and the server, so starfix
keeps it from acting on a terminal or posing as its own output:

- The server refuses control characters (C0, DEL, C1), the Unicode
  bidirectional controls and marks, line separators and invalid UTF-8 in
  every field. Titles, names, labels, reasons and handoff fields are one
  line; bodies, design, acceptance, notes, comments and handoff notes may
  also hold newlines and tabs. Text stored before this rule is kept as is.
- `sfx` prints any such character that still arrives as a visible escape
  (`\x1b`, `\u202e`), and a one-line field's newline as `\n`, in every
  command, server error and quoted server stderr. `--json` escapes them in
  JSON, so the output is the same value and still valid.
- `prime` (and its hook) puts titles and inbox text, each quoted on one
  line, inside a `--- starfix data ---` fence after a line saying they are
  data. A failed hook quotes the error.
- MCP results that hold others' text carry `"untrusted"`, and the
  instructions tell the agent never to follow instructions in that text.
  A fix the server wrote is relayed quoted, for the user.

### `starfixd`

| Command | Does |
|---|---|
| `serve [--dev [--allow-unsafe-dolt]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F]` | Run the daemon. Logs go to stderr (journald under systemd): connections and successful requests at `debug`, refusals, handshake failures and expired claims at `info` (refusals at most `refusal_logs` a minute per principal, then one count), internal errors at `error`. Also `$STARFIXD_LOG_LEVEL`, `$STARFIXD_LOG_FORMAT` |
| `stdio --principal NAME` | sshd forced command: bridge one session to the daemon |
| `import-bd [--dry-run] [--json] FILE` | Import a bd `issues.jsonl` (`-` for stdin) |
| `export-bd [-o FILE]` | Write the store in bd's JSONL format |
| `upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]` | Replace this binary with a verified release, after tagging the database `starfix-<old version>`. With `systemd_unit:` set or `--restart`, restart the unit through `sudo -n systemctl restart` and health-check the daemon over its socket; if the new version is not healthy, restore the old binary and restart it. Run as the user starfixd runs as, not root |
| `version` | Print the version |

Settings come from flags, then `STARFIXD_*` environment variables, then
`/etc/starfix/starfixd.yaml`, then defaults. A password is refused on the
command line, and a config file that holds one must be mode 0600. Each
command that opens the store also takes `--dev --allow-unsafe-dolt`
(Quick start).

#### Limits

`limits:` in the config file bounds what one request or one principal
can make the daemon do. Leave a field out for its default:

| Setting | Default | Bounds |
|---|---|---|
| `labels_per_issue` | 50 | Labels on one issue, and so in one `create` |
| `acceptance_items` | 200 | Items in acceptance text, and item numbers in one `accept` or `finish` |
| `deps_per_issue` | 200 | Edges out of one issue |
| `sessions_per_principal` | 256 | A principal's rows in the `who` registry; a new session past it drops the least recently seen |
| `agent_keep` | `7d` | How long a registry row not seen is kept; a principal's latest row is always kept, so it stays mentionable |
| `inbox_unread` | 1000 | A principal's unread inbox items; past it the oldest are marked read (still under `inbox --all`) |
| `notices_per_minute` | 10 | Mentions, assignments and handoffs one principal can send another a minute; the rest are not delivered. Lost claims always are |
| `inbox_keep` | `30d` | How long a read inbox item is kept |
| `conns` | 1024 | Connections past the handshake; twice it caps sockets still in it |
| `conns_per_principal` | 32 | One principal's connections |
| `idle_timeout` | `10m` | A connection that sends nothing this long is closed with a note, unless it watches its inbox (`sfx mcp` and `sfx watch` do) |
| `write_rate`, `write_burst` | 10, 100 | Each principal's write token bucket: writes a second, and how many at once. Reads are not counted |
| `refusal_logs` | 20 | Refusal log lines per principal a minute |

A request past a per-request cap is refused with `invalid`; a connection
or write past a rate or connection cap with `busy`, whose fix says how
long to wait. Text fields were already capped (titles 500 bytes, names
255, bodies and comments 64 KiB). Reads come a page at a time so no reply
can pass the 4 MiB frame: `comments` and `history` return the newest page
(up to 100 entries, 500 with a limit, about 1 MiB of text) and a cursor to
the one before, which `sfx` follows (`-n N` shows only the newest N);
`who` lists at most 100 agents and counts the rest; `show` lists at most
200 edges and `blocked` 50 blockers an issue, each counting the rest.
Similar-issue lookups read closed titles from a cache refreshed on close
and reopen, or after a minute.

The event log is never pruned: it is the history, and Dolt keeps every
version of it. An event keeps a text over 8 KiB as its first 512 bytes,
its length and its SHA-256, so an edit loop over long fields grows the log
by about a kilobyte a write, not by the text. Watch the Dolt data
directory's size, and run `dolt gc` in a quiet hour if it grows; the
write rate limit bounds how fast one principal can grow it.

Admins are principals listed under `admins:` in the config file, or in
`$STARFIXD_ADMINS` (comma-separated); `serve` reads them when it starts.
Only an admin may change an issue another principal holds, or
`close --force`; each such override is recorded as an `admin.override`
event naming the holder. Nothing over the protocol or MCP reads or changes
the list. The names `starfixd` (the claim reaper) and `import` (the bd
importer) are reserved: no key may use them, and neither may be an admin.

`starfixd upgrade` restarts the unit as the daemon's user through `sudo -n`,
so give that user exactly this rule (`visudo -f /etc/sudoers.d/starfix`,
with your unit name):

```text
starfix ALL=(root) NOPASSWD: /usr/bin/systemctl restart starfixd.service
```

## Agents

Agents use starfix through MCP; they never need a shell. Set each agent
up once per repository and commit the files it writes:

```sh
sfx setup --all --write         # every agent below; safe to rerun
sfx setup claude-code           # print what it would write, and where
sfx setup claude-code --write   # write them; safe to rerun
sfx setup codex --check         # fails, with a fix, if anything is missing
sfx setup codex --remove        # take starfix out again
```

| Agent | MCP config | Pointer | SessionStart hook | You still |
|---|---|---|---|---|
| `claude-code` | `.mcp.json` | `CLAUDE.md` | `.claude/settings.json` | approve the project's MCP server when Claude Code asks |
| `codex` | `.codex/config.toml` | `AGENTS.md` | `.codex/hooks.json` | trust the project, and the hooks with `/hooks` |
| `gemini` | `.gemini/settings.json` | `GEMINI.md` | `.gemini/settings.json` | trust the folder when Gemini CLI asks |
| `cursor` | `.cursor/mcp.json` | `.cursor/rules/starfix.mdc` | `.cursor/hooks.json` | turn starfix on in Cursor Settings › MCP |
| `vscode` | `.vscode/mcp.json` | `.github/copilot-instructions.md` | `.github/hooks/starfix.json` (Preview) | trust the server; hooks run only where VS Code's Preview hooks are on |
| `junie` | `.junie/mcp/mcp.json` | `AGENTS.md` | `~/.junie/config.json`, with `--global` only | run `sfx setup junie --global --write` for the hook |
| `jetbrains` | none: added in the IDE | `.aiassistant/rules/starfix.md` | none | add the printed JSON under Settings › Tools › AI Assistant › Model Context Protocol (MCP), scope Project |

Setup prints the "you still" step after it adds a part. The pointer is a
short block between `<!-- starfix:begin -->` and `<!-- starfix:end -->`
telling the agent to use the starfix tools and to `prime`, `start` and
`finish`; the rest of the file is left alone. Cursor's and AI Assistant's
are rule files of starfix's own. `--remove` takes the block out, and
deletes the file if nothing else is left in it; VS Code's hook file is
starfix's own too. Codex and Junie share `AGENTS.md`, so `setup --all`
writes its block once, and removing either agent takes it out.

Each hook runs `sfx prime --hook=AGENT` (Claude Code's runs bare
`--hook`) when a session starts, in the harness's own format: Gemini CLI
matches sources exactly, so it gets one entry each for `startup`, `resume`
and `clear`; Codex's matcher is `startup|resume|clear|compact`. Prime reads
the session id from the hook's input (`session_id`, or VS Code's
`sessionId`), adds prime to the session's context, prints nothing outside
a starfix repository, and on any error adds a one-line note instead of
failing the session. A hook is starfix's only when its whole command is
the one setup writes, `sfx prime --hook[=AGENT]` through `sfx` or the
`--command` program; other hooks, including your own commands that end
in `sfx prime --hook`, are never touched. Existing files keep their other
keys, their order and their mode; a second run changes nothing.

The starfix MCP entry itself is replaced wholesale: it holds `command`,
`args` and `STARFIX_HARNESS` in its env, and nothing else. `--check`
fails on any extra key or env variable (`cwd`, `PATH`, `LD_PRELOAD` and
the like), and `--write` drops them. A JSON file with a duplicate key is
refused, since the harness would run the last copy. Codex's
`config.toml` is edited table-aware; a file setup cannot read safely, or
one that defines starfix with dotted keys or an inline table, is refused,
and you add the printed snippet by hand.

Setup never reads or writes a project file through a symbolic link: a
link anywhere below the repository root, file included, is refused with
a fix, so a cloned repository cannot point `.mcp.json` at a secret or
`.claude` at your home directory. New project files are 0644.

`--global` edits the files in your home directory instead (for Claude Code
`~/.claude.json`, `~/.claude/CLAUDE.md` and `~/.claude/settings.json`; for
Codex, Gemini CLI and Junie their `~/.codex/`, `~/.gemini/` and `~/.junie/`
files; for Cursor the MCP config and hook); nothing outside the repository
is touched without it. VS Code keeps its user MCP config in a per-platform
profile and AI Assistant in the IDE's settings, so `--global` is refused
for `vscode` and `jetbrains`, and `--all --global` skips them with that
fix. `--command PATH` sets how the agent runs `sfx` when it is not on PATH.
New files in your home directory are created 0600, in 0700 directories.
A home file or directory may be a symbolic link (a dotfiles repository)
only to something of yours inside your home directory; setup edits the
target and keeps the link.

A global hook runs in every repository with a `.starfix.yaml` that you
open with the agent, and connects to the server that file names, chosen
by whoever wrote the repository. Setup says so when it writes one. Prefer
per-project hooks, or turn the global one off before opening a repository
you do not trust.

Claude Desktop (macOS and Windows) has no project files, hooks or working
directory, so `sfx setup claude-desktop`, run inside the repository, adds
one entry per project to the app's own config:

```sh
sfx setup claude-desktop --write    # then quit and reopen Claude Desktop
```

| OS | Config |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

The entry is named `starfix-<directory name>` and runs
`/absolute/path/to/sfx -C /path/to/repo mcp`, because the app starts
servers without your shell's PATH: the path is sfx's entry on PATH (for
example Homebrew's link, which survives upgrades) or, failing that, the
running binary; `--command` must be absolute. Several projects coexist,
and `--check` and `--remove` touch only this checkout's entry; a different
checkout with the same directory name is refused, with a fix. The rest of
the file keeps its keys and order. `--all` leaves Claude Desktop out, as
its file is outside the repository. With no pointer or hook, the agent
learns starfix from the MCP server's instructions.

A session is two calls: `start` takes the top ready issue (or a named
one) and returns it with its acceptance criteria, the last handoff and a
branch name (`fix/sf-a1b2c3d4-fix-the-login-redirect`); `finish` closes it,
records a handoff note and files the work found on the way, linked
`discovered-from`. The MCP `start` never touches git; the agent runs git
itself.

Taking an issue claims it: it becomes `in_progress`, assigned to you, and
leased to your session. Only a claim holds an issue; `update` cannot set
`in_progress`. While the lease runs, another principal's `start` is
refused with the next ready issue to take instead, and its `update`,
`close`, `reopen`, `finish`, `handoff` and `accept` are refused with
`forbidden`, naming the holder: ask them to hand it off, wait for the
lease, or ask an admin. Anyone may still `comment`, label or link it. When
the lease runs out the server returns the issue to `open`. An agent's
lease is 15 minutes, and `sfx mcp` renews it every minute while the agent
runs, so a session that dies lets its issues go within 15 minutes. A claim
taken from a terminal lasts 8 hours (`--for`, at most 24h), and
`sfx away 4h` extends all of yours. Another session of your own takes over
your live claim only when asked (`sfx start ID --take`, or the MCP
`start` tool's `take`); the same session reconnecting keeps it. Each new holder
raises the claim's epoch; `finish` and `handoff --release` refuse an epoch
other than the current one (`--epoch N`; `sfx mcp` passes it), so a
session that lost its claim cannot close work someone has since taken.
`show` prints the claim.

Each principal has an inbox. The server puts an item there in the same
transaction as its cause: `claim.lost` when the reaper ends your
session's expired claim, another session takes it over, or someone else
(an admin, or your other session) closes or releases it; `handoff` when
a handoff names you with `--to`; `mention` when a comment, or a handoff or
finish note, says `@you` (only principals the server has seen, and never
yourself); `assigned` when someone else assigns you an issue. `sfx inbox`
lists them and `--ack` marks them read. `sfx mcp` asks the server to push
new items as they happen, and adds one line to the agent's next tool
result, `inbox: N new (call inbox)`; the `inbox` tool lists them and acks.
`prime` shows the unread count and the newest few.

`sfx who` (and the `who` tool) lists who is at work: every session that
connected or renewed in the last 5 minutes, with its machine, its harness
and the issues it holds. The server records a session when it connects,
and `sfx mcp` keeps it present by renewing every minute while connected.
The harness is `STARFIX_HARNESS`, which `sfx setup` writes into the MCP
config's `env`; failing that, `claude-code` when `CLAUDECODE=1` and
`gemini` when `GEMINI_CLI=1`.

The tools are `prime`, `start`, `finish`, `handoff`, `ready`, `blocked`,
`list`, `show`, `create`, `update`, `close`, `reopen`, `dep`, `label`,
`comment`, `comments`, `history`, `digest`, `who` and `inbox`; there are no admin tools. Results are compact (writes return
`{id, rev}`, lists return id, title, status and priority) and capped at
about 2,000 tokens, prime and digest at 1,500. `digest` is structured data
from the event log, for a standup or status report; the agent writes any
narrative, and starfix runs no model. A refusal is a tool error with the
server's code and message and a `fix:` line naming the agent's next step;
a fix only the user can act on is quoted from the server. Results that hold
text others wrote carry `"untrusted"` (see Untrusted text).

The server knows who you are from your SSH key. The session id comes from
the agent's environment:

| Agent | Session id |
|---|---|
| Claude Code | `CLAUDE_CODE_SESSION_ID`, which it sets |
| Codex, Gemini CLI, Cursor, VS Code, Junie, AI Assistant | none set; `sfx mcp` picks one per process (`m-…`) |
| Claude Desktop | none set; each app launch is one `sfx mcp` process, so one session (`m-…`) |
| a person's own `sfx` commands | `cli`, one per machine |
| any | `STARFIX_SESSION`, if set, wins (for example in the registration's `env`) |

A limitation for every harness but Claude Code: the SessionStart hook
learns the harness's session id from its input, but `sfx mcp` cannot, so
it picks its own. The hook's prime and the agent's tool calls then appear
as two sessions in `sfx who`, and the hook's session holds no lease. Claude
Code avoids this because it sets `CLAUDE_CODE_SESSION_ID` for both.

One SSH connection serves an MCP session. It opens on the first tool call
and is redialed if it drops; reads and creates are retried on the new
connection, and other writes report that they may have applied.

## Roadmap

| Stage | Delivers | State |
|---|---|---|
| 0 | Dolt concurrency spike | Done |
| 1 | Store, server, SSH transport, version handshake, issue CLI, bd import | Done |
| 2 | MCP server, `start`/`finish`, `digest`, `prime`, `upgrade`, agent setup | Done |
| 3 | Claims with leases, agents registry, inbox, event push, handoff, token capture | In progress (claims, agents registry, inbox, push and structured handoffs built) |
| 4 | Team and personal memory with tags; prices and `sfx cost` | |
| 5 | Offline cache, outbox, conflict resolution | |
| 6 | Locks, gates, formulas, swarm, cross-project | |
| 7 | Scheduled digests, GitHub sync, compaction, vectors | |

The Bearings stages (B0–B5) start once starfix stage 3 lands; see the
[Bearings spec](docs/design/bearings.md#6-plan).

## Design documents

| Document | Covers |
|---|---|
| [starfix.md](docs/design/starfix.md) | Principles, architecture, data model, bd parity, MCP tools, memory, coordination, offline, upgrades, developer experience, cost tracking, plan |
| [database.md](docs/design/database.md) | Why Dolt; the Postgres fallback; vector search and embeddings |
| [bearings.md](docs/design/bearings.md) | The Bearings orchestrator: components, defaults, plan, decisions |
| [spike/dolt/RESULTS.md](spike/dolt/RESULTS.md) | Stage 0 findings on Dolt under concurrent writers |

## Development

Requires Go (see `go.mod`) and, for the store tests, a `dolt` binary on
`PATH` (CI pins 2.4.2).

```sh
gofmt -l . && go vet ./...
golangci-lint run ./...
STARFIX_REQUIRE_DOLT=1 go test -race -shuffle=on ./...   # fail, not skip, without dolt
GOOS=windows go build ./cmd/...   # the client must build on every developer OS
shellcheck install.sh
```

| Path | Holds |
|---|---|
| `cmd/sfx`, `cmd/starfixd` | Entry points; `starfixd` also holds the admin import and export |
| `internal/store` | Typed store over Dolt: migrations, issues, deps, labels, comments, events |
| `internal/proto` | Wire frames, handshake, typed requests and errors |
| `internal/server` | Daemon, socket, bridge, settings |
| `internal/client`, `internal/cli` | SSH client, config discovery, CLI commands |
| `internal/safetext` | The one rule for unsafe characters: store validation, import cleaning, output escaping |
| `internal/gitx` | Branch names from issues, issue IDs from branches and `Starfix:` trailers, branch and worktree creation |
| `internal/bdimport` | bd JSONL import and export |
| `internal/release`, `internal/tools/releasekey` | Release download, signature and checksum verification, binary swap; the release-key tool ([RELEASING.md](RELEASING.md)) |
| `internal/mcpserver`, `internal/agentsetup` | MCP tools and `prime`; agent registration |
| `internal/e2e` | End-to-end tests through an in-process SSH server |
| `spike/dolt` | Stage 0 experiments (not built into the binaries) |
| `docs/design` | Design documents |
| `install.sh` | The one-line installer (README, Install); tested by `internal/release` |

Branch with a type prefix (`feature/`, `fix/`, `docs/`, `maintenance/`,
`refactor/`) and title pull requests the same way. `main` takes squash merges
with green CI.

### Security checks

| Check | Runs |
|---|---|
| golangci-lint, with gosec, staticcheck, errcheck, govet and revive | Every pull request and push to `main` |
| `govulncheck` against the Go vulnerability database | Every pull request and push, weekly on `main` (Mondays), and before every release build |
| `go mod download && go mod verify` | Every pull request and push, and before every release build |
| CodeQL, `security-extended` queries | Every pull request and push, and weekly |
| Dependabot, for Go modules and GitHub Actions | Weekly; minor and patch updates grouped |

Dependencies are not vendored. `go.sum` pins each module's content hash and
the Go checksum database rejects a module that was changed after
publication; `go mod verify` checks that the local module cache has not
changed since download.
The dependency list is kept small on purpose. Every action is pinned by
commit SHA.

**Contributing with an AI agent:** [AGENTS.md](AGENTS.md) holds the rules and
the full CI gate for any coding agent; `CLAUDE.md` and `GEMINI.md` import it.
`go-engineer`, a test-first Go engineer and reviewer, is ready to use as a
Claude Code, Codex or Gemini CLI subagent (AGENTS.md, Agent specifications).

## Relationship to beads

starfix is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of, or
endorsed by, the beads or Dolt projects.

## License

Apache-2.0. See [LICENSE](LICENSE).
