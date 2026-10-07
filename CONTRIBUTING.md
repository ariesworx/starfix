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
by hand, follow the [Local](docs/deploy/local.md) example in a Linux VM or
container you can throw away.

## Before you open a pull request

Run the whole gate in [Commands](AGENTS.md#commands), as CI does. Then
check that the docs are still true: a change in behavior updates the page
that describes it, in the same pull request.

| You changed | Update |
|---|---|
| An `sfx` command or flag | [docs/cli.md](docs/cli.md) |
| Agent setup, hooks or MCP tools | [docs/agents.md](docs/agents.md) |
| A server setting, limit or `starfixd` command | [docs/server.md](docs/server.md) |
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
