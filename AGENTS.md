# Agent instructions

Canonical instructions for any AI coding agent working in this repository
(Claude Code, Codex, Gemini CLI, Cursor, GitHub Copilot and others). Codex,
Cursor and most other agents read this file directly. `CLAUDE.md` and
`GEMINI.md` import it, and `.github/copilot-instructions.md` points to it.
Put shared guidance here, not there.

## What this is

starfix is a public, Apache-2.0 Go module: an issue tracker, shared memory
and coordination layer for AI coding agents, stored in Dolt. `sfx` is the
client (a CLI for people and an MCP server for agents); `starfixd` is the
server daemon and the sshd forced-command bridge. Bearings, the orchestrator,
is a design only ([docs/design/bearings.md](docs/design/bearings.md)); no
Bearings code exists yet.

Read [README.md](README.md) for usage and the roadmap, and
[docs/design/starfix.md](docs/design/starfix.md) for the principles and
architecture. The README's status line says which stage is in progress.

## Layout

| Path | Holds |
|---|---|
| `cmd/sfx` | `sfx` entry point: builds a `cli.Env` from the process and calls `cli.Run` |
| `cmd/starfixd` | `starfixd` entry point: `serve`, `stdio`, `upgrade`, and the admin `import-bd` and `export-bd` |
| `internal/store` | Typed store over Dolt: single writer, CAS updates, event log, ready and blocked queries |
| `internal/store/migrations` | Embedded schema migrations, `NNNN_name.sql`, numbered 1..n without gaps |
| `internal/proto` | Wire protocol: NDJSON frames, handshake, `Op` constants, `*Args` and `*Result` types, typed errors |
| `internal/server` | Daemon: socket, peer check, sshd bridge, dispatch of ops to the store, error mapping, settings |
| `internal/client` | In-process SSH client with a pinned host key, `.starfix.yaml` discovery, session ids |
| `internal/safetext` | Unsafe characters (controls, bidi): the store's validation, bd import cleaning, and the escaping of everything clients print |
| `internal/cli` | `sfx` commands; writes only to the `Env` it is given |
| `internal/mcpserver` | MCP tools, `prime`, token budgets, agent-facing error rewording |
| `internal/agentsetup` | `sfx setup AGENT` and `--all`: per harness, the MCP config, pointer block, SessionStart hook and the hook's output format; per-project entries in a desktop app's config (`desktop.go`) |
| `internal/gitx` | Branch names from issues, issue ids from branches and `Starfix:` trailers |
| `internal/bdimport` | bd JSONL import and export |
| `internal/release`, `internal/tools/releasekey` | Release download, signature and checksum checks, binary swap; the signing tool |
| `internal/version` | Build version, and `Dolt`, the pinned Dolt release |
| `internal/dolttest` | Starts a throwaway `dolt sql-server` for tests |
| `internal/e2e` | End-to-end tests: CLI and MCP through an in-process SSH server to a real daemon and store |
| `spike/dolt` | Stage 0 measurements, kept as evidence; not maintained and excluded from lint |
| `docs/design` | Design documents |
| `.claude/agents`, `.codex/agents`, `.gemini/agents` | Agent specifications (below); `internal/agentspec` keeps them in step |

## Commands

The gate, as CI runs it (`.github/workflows/ci.yml`). Run all of it before
opening a pull request:

```sh
gofmt -l .                       # must print nothing
go vet ./...
golangci-lint run ./...          # CI pins v2.14.0; config in .golangci.yml
go mod verify                    # module cache still matches go.sum
go test -race ./...
for os in darwin windows; do     # sfx must build and vet on every developer OS
  GOOS=$os GOARCH=amd64 go vet ./...
  GOOS=$os GOARCH=amd64 go build ./cmd/...
done
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

The Go version is the `go` line in `go.mod`.

**Dolt.** The store, server, bdimport, `cmd/starfixd` and e2e tests start a
real `dolt sql-server` through `internal/dolttest`. They need `dolt` on
`PATH`, at the version in `version.Dolt` (`internal/version`; CI's `DOLT_VERSION`; a
test keeps the two equal, so change them together). Get it from
[Dolt's releases](https://github.com/dolthub/dolt/releases). Without
`dolt` those tests skip with a message, and the rest still run; a skipped
store test proves nothing, so install Dolt before changing the store,
server or protocol. Tests that need `git` skip without it too.

## Rules

1. **Nothing organization-specific** in code, tests or docs: no real host,
   fingerprint, path, person or client name. Use `example.com`, `alice`,
   `bob` and invented UUIDs (design principle 10).
2. **No secret in argv, logs, output or the repository.** `starfixd` refuses
   a DSN password on the command line, and a config file holding one must be
   mode 0600. Follow that pattern for anything new.
3. **Agents never handle release signing keys.** Releases are signed by the
   maintainer's gated `release` environment ([RELEASING.md](RELEASING.md)).
   Do not edit `internal/release/keys.go` or the release workflow's signing
   steps unless the maintainer asks.
4. **Keep the binaries small and auditable.** Standard library first; a new
   dependency needs a stated reason in the pull request. Release builds are
   `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w"`, so code must build
   without cgo. Pin GitHub Actions by commit SHA with a version comment.
5. **Store writes go through `Store.write`**, on the single writer
   connection, one SQL transaction per operation. Do not open another
   writer or write to Dolt from anywhere else.
6. **Every `UPDATE` sets `write_id = ?`** to a value unique to that write.
   Dolt detects conflicts per cell, not per row, so `rev = rev + 1` alone
   lets two concurrent writers both succeed silently (stage 0,
   `spike/dolt/RESULTS.md`). `wtx.exec` refuses an `UPDATE` without it.
   `SELECT ... FOR UPDATE` does not lock in Dolt; do not use it.
7. **Every mutation appends an event** with `wtx.event`, in the same
   transaction. `writeOnce` refuses a transaction that changed rows but
   recorded no event. The event log is the truth; ready, blocked and
   digest are derived from it and never stored.
8. **Write closures must be safe to rerun.** A write that loses to a
   concurrent transaction is retried from the start.
9. **Migrations are append-only.** Add the next `NNNN_name.sql`; never edit,
   renumber or delete one that has merged. They only go forward: an older
   `starfixd` refuses a database a newer one migrated (`ErrSchemaTooNew`).
10. **The protocol is versioned.** Frames are NDJSON (`internal/proto`). The
    server decodes `*Args` with `DisallowUnknownFields`, so a new argument
    field, or a new op, is refused by an older server. Treat either as a
    protocol change: raise `proto.Proto` and `ProtoMax`, and keep `ProtoMin`
    at the previous version, so new servers still accept old clients
    (design principle 9). The server must then work when an old client
    omits the new field. After the bump, a new client against an old
    server is refused at the handshake with code `version` (exit code 3)
    and told to have the server upgraded, rather than failing on its
    first request. Clients ignore unknown result fields and frame
    types, so adding those needs no bump.
11. **Every refusal says what to do next.** Return a `proto.Error` with a
    typed `Code`, a message naming the cause and a `Fix` naming the next
    action (`proto.Errf(code, fix, msg)`). Match `internal/server/errors.go`.
    The CLI prints the message and a `fix:` line; `internal/mcpserver`
    rewords the fix as the agent's next tool call (`errors.go`).
12. **MCP is for agents and stays small.** No admin tools over MCP: import,
    export, setup, upgrade and settings are CLI only. Identity is implicit:
    the principal comes from the SSH key and the session from the
    environment, never from a tool argument. Results are capped
    (`budget.go`: 2,000 tokens, prime and digest 1,500) and the whole tool
    schema set at about 2,400 estimated tokens; the tests enforce both and
    pin the tool list. Keep schema descriptions terse.
13. **Output stays terse.** Writes return `{id, rev}`; lists return the
    compact summary. `sfx --json` prints exactly one JSON document, errors
    included. Exit codes: 0 ok, 1 failure, 2 usage, 3 protocol refused.
14. **Platforms.** `sfx` runs on Linux, macOS and Windows, so every change
    must build for all three. `starfixd serve` is Linux-only, because only
    Linux can check the socket peer's user (`peer_linux.go`); `--dev`
    overrides that on a single-user machine.
15. **Go style.** `gofmt` and `goimports`, table-driven tests, errors
    wrapped with `%w`. Library code in `internal/` never touches
    `os.Stdout`, `os.Stderr` or `os.Stdin`; it uses the writers it is given
    (`cli.Env`). Only `main` packages wire up the process's streams. Panic
    only on a programming error that tests catch, and say so in a comment.
16. **American English** in code, comments and docs: "behavior", "color",
    "organization", "license" (noun and verb), "-ize".
17. **Keep the docs true.** A change in behavior updates the doc that
    describes it in the same pull request: the README's command tables,
    `RELEASING.md`, or the design doc. Where the build departs from the
    design, add to that section's dated **As built** note in
    `docs/design/starfix.md` rather than rewriting the plan.

## Agent specifications

`go-engineer` is a test-first Go engineer and reviewer for changes in this
repository, in each harness's format: `.claude/agents/go-engineer.md`
(Claude Code), `.codex/agents/go-engineer.toml` (Codex) and
`.gemini/agents/go-engineer.md` (Gemini CLI). Delegate Go work and reviews
to it. The three files share one prompt, name and description; edit all
three together, and `internal/agentspec` fails until they match. A spec
adds working method only; repository facts and rules belong here.

## Git

- `main` is protected: squash merges through a pull request with green CI.
- Branch with a type prefix: `feature/`, `fix/`, `maintenance/`, `docs/` or
  `refactor/`, then lowercase kebab-case.
- Title the pull request the same way: `fix: …`, `feature: …`.
- Commit subjects are imperative; the body says why. Note "No new
  dependencies" or justify the one you added.
- Do not push, open pull requests or merge unless asked.
