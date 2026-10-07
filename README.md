# starfix

[![ci](https://github.com/ariesworx/starfix/actions/workflows/ci.yml/badge.svg)](https://github.com/ariesworx/starfix/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Issue tracker, shared memory and coordination for AI coding agents working
across sessions and machines. Agents use it through MCP; people administer it
from the command line. It is written in Go and stores its data in
[Dolt](https://github.com/dolthub/dolt) behind a small server.

The repository also holds the design for **Bearings**, an orchestrator that runs
and supervises many agents on top of starfix.

> **Status: stage 2 of 7 in progress.** Issues work end to end over SSH, bd
> backlogs import and export, and agents use starfix through MCP. Claims,
> memory and the offline cache follow. Not ready for production use.

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

## Quick start

On the server, as the Unix user starfixd runs as (here `starfix`), with a
Dolt sql-server on loopback:

```sh
go install github.com/ariesworx/starfix/cmd/starfixd@latest
cat > /etc/starfix/starfixd.yaml <<'YAML'   # chmod 600: it holds the DSN
dsn: starfix:PASSWORD@tcp(127.0.0.1:3306)/starfix
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f   # any UUID; uuidgen | tr A-Z a-z
socket: /run/starfix/starfixd.sock
YAML
starfixd serve        # run it under systemd
```

Give each developer one line in `~starfix/.ssh/authorized_keys`. The forced
command fixes who they are; the client cannot choose:

```text
restrict,command="starfixd stdio --principal alice" ssh-ed25519 AAAA… alice@example.com
```

In the repository, commit a `.starfix.yaml`. It holds no secrets:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix.example.com
  port: 22                 # default
  user: starfix            # default
  host_key: SHA256:…       # ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -
# key: ~/.ssh/id_ed25519   # optional; ssh-agent is used otherwise
```

Then, on each developer machine:

```sh
go install github.com/ariesworx/starfix/cmd/sfx@latest
sfx create "Fix the login redirect" -p 1 -t bug
sfx ready
sfx update sf-a1b2c3d4 --status in_progress
sfx dep add sf-a1b2c3d4 sf-e5f6g7h8     # a1b2… depends on e5f6…
sfx close sf-a1b2c3d4 --reason "fixed in #12"
```

The host key is pinned, never trusted on first use. Keys come from ssh-agent
(the OpenSSH named pipe on Windows) or the `key:` file.

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
silently; the mapping is in `internal/bdimport`.

## Commands

### `sfx`

`sfx [-C DIR] [--json] COMMAND [ARGS]`. `sfx help COMMAND` shows a
command's usage; `--json` prints one JSON document, errors included.

| Command | Does |
|---|---|
| `start [ID]` | Take an issue (the top ready one without ID) and show it with its last handoff and branch; `--branch` checks the branch out, `--worktree DIR` makes a worktree on it |
| `finish ID` | Close your issue with `--reason`, a `--handoff` note and `--discovered TITLE` work, in one step |
| `handoff ID NOTE` | Leave a note for whoever continues; `--release` unassigns it so another can start it |
| `create` | Create an issue and print its id |
| `show` | Show an issue and its dependencies (`--compact` for short) |
| `list` | List open issues (`--status`, `--all`) |
| `ready` | List issues nothing holds back |
| `blocked` | List issues held back by open blockers |
| `update` | Change fields; `--rev N` makes it a strict compare-and-swap |
| `close`, `reopen` | Close or reopen an issue |
| `dep` | Add or remove a dependency: FROM depends on TO |
| `label` | Add or remove labels |
| `comment`, `comments` | Add a comment; list an issue's comments |
| `history` | List an issue's changes |
| `digest` | Summarize a window (`--since 24h`, `7d`, a date or a time): closed, started, in progress, stalled, blocked, handed off, created and discovered; `--by P`, `--label L` filter it |
| `prime` | A session's orientation: your in-progress issues, top ready work, version notices; `--hook` for a SessionStart hook |
| `mcp` | The MCP server for agents, on stdin and stdout |
| `setup AGENT` | Set up `claude-code`, `codex`, `cursor`, `gemini` or `vscode`: MCP config, instruction pointer, SessionStart hook |
| `version` | Print the version |

Exit codes: 0 ok; 1 failure, with a `fix:` line; 2 usage; 3 protocol version
refused.

### `starfixd`

| Command | Does |
|---|---|
| `serve [--dev] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P]` | Run the daemon |
| `stdio --principal NAME` | sshd forced command: bridge one session to the daemon |
| `import-bd [--dry-run] [--json] FILE` | Import a bd `issues.jsonl` (`-` for stdin) |
| `export-bd [-o FILE]` | Write the store in bd's JSONL format |
| `version` | Print the version |

Settings come from flags, then `STARFIXD_*` environment variables, then
`/etc/starfix/starfixd.yaml`, then defaults. A password is refused on the
command line, and a config file that holds one must be mode 0600.

## Agents

Agents use starfix through MCP; they never need a shell. Set each agent
up once per repository and commit the files it writes:

```sh
sfx setup claude-code           # print what it would write, and where
sfx setup claude-code --write   # write them; safe to rerun
sfx setup codex --check         # fails, with a fix, if anything is missing
sfx setup codex --remove        # take starfix out again
```

| Agent | MCP config | Pointer | SessionStart hook |
|---|---|---|---|
| `claude-code` | `.mcp.json` | `CLAUDE.md` | `.claude/settings.json` |
| `codex` | `.codex/config.toml` | `AGENTS.md` | none |
| `gemini` | `.gemini/settings.json` | `GEMINI.md` | none |
| `cursor` | `.cursor/mcp.json` | `.cursor/rules/starfix.mdc` | none |
| `vscode` | `.vscode/mcp.json` | `.github/copilot-instructions.md` | none |

The pointer is a short block between `<!-- starfix:begin -->` and
`<!-- starfix:end -->` telling the agent to use the starfix tools and to
`prime`, `start` and `finish`; the rest of the file is left alone. Cursor's
is a rule file of starfix's own. `--remove` takes the block out, and
deletes the file if nothing else is left in it. The Claude Code hook runs
`sfx prime --hook` when a session starts, resumes, clears or compacts: it
adds prime to the session's context under the hook's session id, prints
nothing outside a starfix repository, and on any error adds a one-line
note instead of failing the session. Existing files keep their other
keys, their order and their mode; a second run changes nothing.

`--global` edits the files in your home directory instead (for Claude Code
`~/.claude.json`, `~/.claude/CLAUDE.md` and `~/.claude/settings.json`; for
Codex and Gemini CLI their `~/.codex/` and `~/.gemini/` files; for Cursor
the MCP config only); nothing outside the repository is touched without
it. VS Code keeps its user MCP config in a per-platform profile, so
`--global` is refused for `vscode`. `--command PATH` sets how the agent
runs `sfx` when it is not on PATH.

A session is two calls: `start` takes the top ready issue (or a named
one) and returns it with its acceptance criteria, the last handoff and a
branch name (`fix/sf-a1b2c3d4-fix-the-login-redirect`); `finish` closes it,
records a handoff note and files the work found on the way, linked
`discovered-from`. Until stage 3's leases, taking an issue sets it
`in_progress` and assigned to you, and another principal's `start` or
`finish` on it is refused with the next ready issue to take instead. The
MCP `start` never touches git; the agent runs git itself.

The tools are `prime`, `start`, `finish`, `handoff`, `ready`, `blocked`,
`list`, `show`, `create`, `update`, `close`, `reopen`, `dep`, `label`,
`comment`, `comments`, `history` and `digest`; there are no admin tools. Results are compact (writes return
`{id, rev}`, lists return id, title, status and priority) and capped at
about 2,000 tokens, prime and digest at 1,500. `digest` is structured data
from the event log, for a standup or status report; the agent writes any
narrative, and starfix runs no model. A refusal is a tool error with the
server's code and message and a `fix:` line naming the agent's next step.

The server knows who you are from your SSH key. The session id comes from
the agent's environment:

| Agent | Session id |
|---|---|
| Claude Code | `CLAUDE_CODE_SESSION_ID`, which it sets |
| Codex, Gemini CLI, Cursor, VS Code | none set; `sfx mcp` picks one per process (`m-…`) |
| any | `STARFIX_SESSION`, if set, wins (for example in the registration's `env`) |

One SSH connection serves an MCP session. It opens on the first tool call
and is redialed if it drops; reads and creates are retried on the new
connection, and other writes report that they may have applied.

## Roadmap

| Stage | Delivers | State |
|---|---|---|
| 0 | Dolt concurrency spike | Done |
| 1 | Store, server, SSH transport, version handshake, issue CLI, bd import | Done |
| 2 | MCP server, `start`/`finish`, `digest`, `prime`, `upgrade`, agent setup | In progress (MCP, `prime`, `setup`, `start`/`finish`, `digest` built) |
| 3 | Claims with leases, agents registry, inbox, event push, handoff, token capture | |
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
go test -race ./...
GOOS=windows go build ./cmd/...   # the client must build on every developer OS
```

| Path | Holds |
|---|---|
| `cmd/sfx`, `cmd/starfixd` | Entry points; `starfixd` also holds the admin import and export |
| `internal/store` | Typed store over Dolt: migrations, issues, deps, labels, comments, events |
| `internal/proto` | Wire frames, handshake, typed requests and errors |
| `internal/server` | Daemon, socket, bridge, settings |
| `internal/client`, `internal/cli` | SSH client, config discovery, CLI commands |
| `internal/gitx` | Branch names from issues, issue IDs from branches and `Starfix:` trailers, branch and worktree creation |
| `internal/bdimport` | bd JSONL import and export |
| `internal/mcpserver`, `internal/agentsetup` | MCP tools and `prime`; agent registration |
| `internal/e2e` | End-to-end tests through an in-process SSH server |
| `spike/dolt` | Stage 0 experiments (not built into the binaries) |
| `docs/design` | Design documents |

Branch with a type prefix (`feature/`, `fix/`, `docs/`, `maintenance/`,
`refactor/`) and title pull requests the same way. `main` takes squash merges
with green CI.

## Relationship to beads

starfix is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of, or
endorsed by, the beads or Dolt projects.

## License

Apache-2.0. See [LICENSE](LICENSE).
