# Contributing to starfix

This page gets you from a fresh clone to a pull request. The rules every
change follows, the map of the code and the CI gate are in
[AGENTS.md](AGENTS.md). They apply to people and agents alike, so read
them before your first change.

## What you need

| Tool | Version | Used for |
|---|---|---|
| Go | the `go` line in [go.mod](go.mod) | everything |
| Dolt | `Dolt` in [internal/version](internal/version/version.go) | the store, server, bd import and end-to-end tests |
| golangci-lint | the version CI pins | lint, with the config in `.golangci.yml` |
| shellcheck | any recent | `install.sh` |
| git | any recent | tests of branch and worktree creation |

Get Dolt from [Dolt's releases](https://github.com/dolthub/dolt/releases).

## Build and test

```sh
go build ./cmd/...
go test ./...
```

Tests that need Dolt, git or root skip without them, and a skipped store
test proves nothing. [Commands](AGENTS.md#commands) says which tests need
what, and how CI makes a missing Dolt fail instead. To try a real server
by hand, start the [Docker test server](docs/deploy/docker.md), which
builds from your checkout, or follow the [Local](docs/deploy/local.md)
example in a Linux VM you can throw away. `deploy/docker/smoke.sh`
drives that server through the main commands end to end; CI does not
run it.

### The soak test

`TestSoak` in `internal/e2e` is stage 3's gate: four principals with
five sessions each work one backlog at once through the in-process SSH
server, a real daemon and Dolt, while the test drops answers and
connections, restarts the daemon, and lets sessions vanish or stall past
their leases. It checks claims, fencing, the event log, idempotent
retries, the inbox and event pushes, token usage, rule 18's caps, and
that the daemon leaks nothing. A short run of about 12 seconds is part of
`go test`. Run a long one before changing claims, the reaper, the event
log, pushes or the write path:

```sh
STARFIX_REQUIRE_DOLT=1 STARFIX_SOAK=10m go test -race -timeout 45m -run 'TestSoak$' -v ./internal/e2e/
```

The run prints its seed first and with any failure, followed by the
events of each issue involved. `STARFIX_SOAK_SEED=<seed>` repeats a
run's choices; the interleaving of sessions still differs. With `-v` the
report lists each op's count, outcome and p50 and p99 latency, the
refusals by code, and what the chaos did. Latency is reported, not
asserted: only a request with no answer in 60 seconds fails the run.

## Before you open a pull request

Run the whole gate in [Commands](AGENTS.md#commands), as CI does. Then
check that the docs are still true: a change in behavior updates the page
that describes it, in the same pull request.

| You changed | Update |
|---|---|
| An `sfx` command or flag | [docs/cli.md](docs/cli.md) |
| Agent setup, hooks or MCP tools | [docs/agents.md](docs/agents.md) |
| A server setting, limit or `starfixd` command | [docs/server.md](docs/server.md) |
| A setup step in docs/server.md, or the Docker test server | [docs/deploy/docker.md](docs/deploy/docker.md) and `deploy/docker` |
| A security property | [docs/security-model.md](docs/security-model.md) |
| The release process | [RELEASING.md](RELEASING.md) |
| Something that departs from a design | an **As built** note in [docs/design](docs/design/README.md) |

Branches, pull request titles and commit messages follow
[Git](AGENTS.md#git), and a new dependency needs a reason in the pull
request.

## Working with an AI agent

[AGENTS.md](AGENTS.md) is the instruction file for any coding agent.
[Agent specifications](AGENTS.md#agent-specifications) describes
`go-engineer`, a Go engineer and reviewer you can run as a subagent.
