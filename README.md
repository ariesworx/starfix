# starfix

[![ci](https://github.com/ariesworx/starfix/actions/workflows/ci.yml/badge.svg)](https://github.com/ariesworx/starfix/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Issue tracker, shared memory and coordination for AI coding agents working
across sessions and machines. Agents use it through MCP; people administer it
from the command line. It is written in Go and stores its data in
[Dolt](https://github.com/dolthub/dolt) behind a small server.

The repository also holds the design for **Bearing**, an orchestrator that runs
and supervises many agents on top of starfix.

> **Status: stage 1 of 7 done.** Issues work end to end over SSH, and bd
> backlogs import and export. Stage 2 (the MCP server for agents) is in
> progress; claims, memory and the offline cache follow. Not ready for
> production use.

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
 agent ──MCP──▶ starfix (CLI + MCP, local cache)
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
| `starfix` | Linux, macOS, Windows | CLI for people; MCP server for agents | CLI built; MCP in stage 2 |
| `starfixd` | Linux (Windows via WSL2) | Server daemon and sshd bridge | Built |
| `bearing`, `bearingd` | Linux, macOS (Windows via WSL2) | Agent orchestrator | Design ([spec](docs/design/bearing.md)) |

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
go install github.com/ariesworx/starfix/cmd/starfix@latest
starfix create "Fix the login redirect" -p 1 -t bug
starfix ready
starfix update sf-a1b2c3d4 --status in_progress
starfix dep add sf-a1b2c3d4 sf-e5f6g7h8     # a1b2… depends on e5f6…
starfix close sf-a1b2c3d4 --reason "fixed in #12"
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

### `starfix`

`starfix [-C DIR] [--json] COMMAND [ARGS]`. `starfix help COMMAND` shows a
command's usage; `--json` prints one JSON document, errors included.

| Command | Does |
|---|---|
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

## Roadmap

| Stage | Delivers | State |
|---|---|---|
| 0 | Dolt concurrency spike | Done |
| 1 | Store, server, SSH transport, version handshake, issue CLI, bd import | Done |
| 2 | MCP server, `start`/`finish`, `digest`, `prime`, `upgrade`, agent setup | In progress |
| 3 | Claims with leases, agents registry, inbox, event push, handoff, token capture | |
| 4 | Team and personal memory with tags; prices and `starfix cost` | |
| 5 | Offline cache, outbox, conflict resolution | |
| 6 | Locks, gates, formulas, swarm, cross-project | |
| 7 | Scheduled digests, GitHub sync, compaction, vectors | |

Bearing's stages (B0–B5) start once starfix stage 3 lands; see the
[Bearing spec](docs/design/bearing.md#6-plan).

## Design documents

| Document | Covers |
|---|---|
| [starfix.md](docs/design/starfix.md) | Principles, architecture, data model, bd parity, MCP tools, memory, coordination, offline, upgrades, developer experience, cost tracking, plan |
| [database.md](docs/design/database.md) | Why Dolt; the Postgres fallback; vector search and embeddings |
| [bearing.md](docs/design/bearing.md) | The Bearing orchestrator: components, defaults, plan, decisions |
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
| `cmd/starfix`, `cmd/starfixd` | Entry points; `starfixd` also holds the admin import and export |
| `internal/store` | Typed store over Dolt: migrations, issues, deps, labels, comments, events |
| `internal/proto` | Wire frames, handshake, typed requests and errors |
| `internal/server` | Daemon, socket, bridge, settings |
| `internal/client`, `internal/cli` | SSH client, config discovery, CLI commands |
| `internal/bdimport` | bd JSONL import and export |
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
