# starfix

[![ci](https://github.com/ariesworx/starfix/actions/workflows/ci.yml/badge.svg)](https://github.com/ariesworx/starfix/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**An issue tracker built for AI coding agents.** Agents claim work, hand
it off and see who else is working, across sessions and machines. They
reach starfix through MCP (Model Context Protocol), the standard way
coding agents call tools, and people use the same tracker from the
command line. One small Go server keeps the data in
[Dolt](https://github.com/dolthub/dolt), a SQL database with Git-style
history.

> **Status: stage 3 of 7 in progress.** Issues, claims, handoffs, the
> inbox, agent setup and importing from bd (beads) work today. Memory and
> an offline cache come next. Not ready for production use yet; see the
> [roadmap](#roadmap).

## Why starfix

When several agents work on one codebase, they have to agree on who does
what. starfix makes that safe:

- **Claims can't go stale.** An agent claims an issue with a lease. If the
  agent dies, the lease lapses within 15 minutes and the issue is ready
  again. An agent that lost its claim cannot close work someone else has
  since taken.
- **One authority.** Every change goes through one server, which puts them
  in order. There are no database merges and no conflicts to resolve.
- **Identity comes from SSH keys.** The server's sshd decides who you are.
  A client cannot claim to be someone else.
- **Built for context windows.** Results are compact: a write returns
  only the issue's id and new revision (`{id, rev}`), and no result passes
  about 2,000 tokens.
- **Failures say what to do.** Errors come with a `fix:` line naming the
  next step, and each exit code and error code has one documented
  meaning.

## What you can do with it

| You can | With | Read more |
|---|---|---|
| Track issues with types, priorities, labels, parents and dependencies, and see what is ready or blocked | `create`, `ready`, `blocked`, `dep` | [Issues](docs/concepts.md#issues), [ready and blocked](docs/concepts.md#dependencies-ready-and-blocked) |
| Claim an issue, then close it with a handoff note, filing what you found on the way | `start`, `finish` | [Claims](docs/concepts.md#claims-and-leases), [start and finish](docs/concepts.md#a-session-start-and-finish) |
| Check work against its acceptance criteria | `accept`, `finish --tick` | [Acceptance criteria](docs/concepts.md#acceptance-criteria) |
| Hand work between people and agents | `handoff --to` | [Handoffs](docs/concepts.md#handoffs) |
| Get mentions, handoffs, assignments and lost claims in an inbox the server pushes | `inbox`, `watch` | [The inbox](docs/concepts.md#the-inbox) |
| See every session at work and what it holds | `who` | [Who is at work](docs/concepts.md#who-is-at-work) |
| Summarize a day or a week for a standup | `digest` | [The digest](docs/concepts.md#the-digest) |
| Set up Claude Code, Codex, Gemini CLI, Cursor, VS Code, Junie, JetBrains AI Assistant or Claude Desktop | `setup` | [Agents](docs/agents.md) |
| Move a bd (beads) backlog over, keeping its IDs | `starfixd import-bd` | [Moving from bd](docs/migrate-from-bd.md) |
| Reach a server that has no public address, through Google Cloud IAP | `iap:` in `.starfix.yaml` | [Using sfx](docs/cli.md#servers-without-a-public-ip-google-cloud-iap) |
| Upgrade clients and the server from signed releases | `upgrade` | [Installing starfix](docs/install.md#upgrade-and-roll-back) |

Planned: team and personal memory, token cost tracking, an offline cache,
and **Bearings**, an orchestrator that runs and supervises many agents on
top of starfix ([design](docs/design/bearings.md)).

## How it works

```text
 agent ──MCP──┐                             the server
              ├─▶ sfx ══ SSH ══▶ sshd ─▶ starfixd stdio --principal NAME
 person ─CLI──┘   (pinned host key)            │ unix socket, 0700
                                               ▼
                                         starfixd serve ─▶ Dolt (loopback)
```

- **`sfx`** is the client: a command line for people, and an MCP server
  for agents. It runs on Linux, macOS and Windows. The name is short for
  starfix.
- **sshd** checks the key. Each key's line in `authorized_keys` runs one
  fixed command, `starfixd stdio --principal NAME`, so the key, not the
  client, decides your principal: the name the server knows you by.
- **`starfixd serve`** owns the data. It applies every change in order,
  records each as an event in the same transaction, and stores it in Dolt.
  It runs on Linux.
- Messages are newline-delimited JSON. Each connection opens with a
  version handshake.

## Deployment examples

| Example | For | Server | Cost a month |
|---|---|---|---|
| [Local](docs/deploy/local.md) | One developer and their agents | Your own Linux machine, WSL2 or a Linux VM | $0 |
| [Team](docs/deploy/team.md) | A team with agents on laptops, in cloud sandboxes and in CI | A small cloud VM, reached over SSH | About $19 |
| [Enterprise](docs/deploy/enterprise.md) | A company that keeps the tracker off the internet | A private VM behind Google Cloud IAP, with backups nothing on it can delete | About $65 to $114 |

Costs are Google Cloud list prices for us-central1, read in October 2026.
[Deployment examples](docs/deploy/README.md) compares the three.

## Quick start

**To join a project that has a server:**

1. [Install `sfx`](#install).
2. Load your SSH key into ssh-agent with `ssh-add`; `sfx` signs in
   through the agent. Send the public key that `ssh-add -L` prints to the
   server's admin.
3. In the repository, which has a committed `.starfix.yaml`, check the
   connection:

   ```sh
   sfx ready
   ```

4. If nobody has set your agent up in this repository yet, do it and
   commit what setup writes. For Claude Code:

   ```sh
   sfx setup claude-code --write
   ```

   The files tell agents to call `prime` when a session starts, then
   `start` and `finish` for each issue. [Agents](docs/agents.md#set-up-an-agent)
   lists every agent, and the one step each person still takes on their
   own machine, such as approving the MCP server.

**To run your own server,** start with the [Local](docs/deploy/local.md)
example, or follow [Running a server](docs/server.md) on any Linux
machine.

## Install

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
```

The script checks the release's checksum, and its signature when
OpenSSL 3 is installed, then installs `sfx` into `~/.local/bin`. It never
uses sudo. On Windows, download the zip
from the [releases page](https://github.com/ariesworx/starfix/releases).
[Installing starfix](docs/install.md) covers the options, the server
binary, building from source and verifying a download by hand.

## Documentation

| Guide | Covers |
|---|---|
| [How starfix works](docs/concepts.md) | Identity, issues, dependencies, claims and leases, handoffs, the inbox, the digest |
| [Using sfx](docs/cli.md) | Connecting a repository, every command, exit codes |
| [Agents](docs/agents.md) | Setting agents up, session-start hooks, MCP tools, sessions |
| [Running a server](docs/server.md) | Setup step by step, principals, admins, settings, limits, upgrades, backups, troubleshooting |
| [Deployment examples](docs/deploy/README.md) | Local, team and enterprise deployments, with costs |
| [Installing starfix](docs/install.md) | Install options, manual downloads, verifying releases, upgrades |
| [Moving from bd](docs/migrate-from-bd.md) | Importing a beads backlog, and exporting it back |
| [Security model](docs/security-model.md) | Identity, host keys, the Dolt account, untrusted text, releases, CI checks |
| [Design documents](docs/design/README.md) | Principles, architecture, data model, the Dolt spike, Bearings |
| [Contributing](CONTRIBUTING.md) | Building, testing and opening a pull request |
| [Releasing](RELEASING.md) | How releases are built, signed and published |

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

The Bearings stages (B0 to B5) start once starfix stage 3 lands; see the
[Bearings plan](docs/design/bearings.md#6-plan).

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) explains how to build and test starfix
and how pull requests work. [AGENTS.md](AGENTS.md) holds the rules every
contributor follows, person or agent.

## Relationship to beads

starfix is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of, or
endorsed by, the beads or Dolt projects.

## License

Apache-2.0. See [LICENSE](LICENSE).
