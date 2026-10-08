# Security model

This page explains how starfix decides who you are, what each principal
may change, and how it handles text, files and releases from others. The
setup steps that apply these rules are in [Running a server](server.md).

## Identity comes from SSH keys

The server's sshd decides who connects. Each key's line in
`~starfix/.ssh/authorized_keys` forces one command,
`starfixd stdio --principal NAME`, so the principal comes from the key that
matched. A client cannot claim to be anyone else, and nothing depends on
paths or environment variables.

- `restrict` on each line turns off forwarding and terminals, and the
  sshd `Match` block repeats that for the whole account
  ([step 6](server.md#6-let-the-first-principal-in)).
- A `ForceCommand` would override every key's command, and so its
  principal. Never add one for the account.
- A session id, which comes from the client's environment, only tells one
  principal's sessions apart. It grants nothing.
- To revoke someone, delete their key lines and stop their open sessions
  ([Principals](server.md#principals)).

## The server's host key is pinned

`sfx` checks the server's ED25519 host key against `host_key` in
`.starfix.yaml` on every connection, and never trusts a key on first use.
A mismatch is refused; its fix says to confirm the new key with the
admin before you change the file. `sfx` runs its own SSH client, so
nothing in `~/.ssh/config` can weaken the check ([The host key is
pinned](cli.md#the-host-key-is-pinned)).

## Who may change what

- **A claim protects an issue.** While someone holds it, only the holder's
  principal may change it, close it or hand it off. Others get `forbidden`,
  naming the holder ([Claims and leases](concepts.md#claims-and-leases)).
  Anyone may still comment on it, label it or link it.
- **Admins may override.** An admin's change to an issue another
  principal holds is recorded as an `admin.override` event naming the
  holder, and a forced close records the open acceptance items in its
  close event. Only the server's config file or environment lists admins
  ([Admins](server.md#admins)).
- **Every change is an event**, written in the same transaction as the
  change, with its principal, session and time. Clients cannot edit or
  delete events.
- **What others see.** `sfx who` shows every principal's machine names. A
  handoff's worktree path, a path on your machine, is shown only to your
  own principal.

## The Dolt account

A `dolt sql-server` started without a config lets `root` in from localhost
with no password, grants it `FILE`, and leaves `secure_file_priv` empty.
Any user on the machine could then skip starfixd, rewrite the event log as
anyone, and read or write files as Dolt's user.

So every command that opens the store first checks the account it connects
as. It refuses to continue, with the SQL to fix it, when the account:

- is `root`;
- holds any privilege on `*.*`, or `GRANT OPTION`;
- is on a server whose `secure_file_priv` is empty.

A passwordless `root` cannot be seen from a least-privileged account, so
starfixd cannot check for it. [Step
4](server.md#4-create-the-database-and-the-config-file) gives `root` a
random password. On a developer's own machine, `--dev --allow-unsafe-dolt`
lets a default Dolt through with a warning. No config file or environment
variable can set it.

## The daemon's socket

`starfixd serve` listens on a unix socket in a directory that must be mode
0700 and owned by the daemon's user. On Linux it also checks each
connecting process's user ID and refuses any but its own, which is why
`serve` runs only on Linux. `starfixd stdio` runs as that same user,
because sshd runs every forced command as the `starfix` account.

## Untrusted text

Issue text comes from other people, their agents and the server. starfix
keeps it from acting on a terminal or posing as starfix's own output:

- **The server refuses unsafe characters** in every field: control
  characters (C0, DEL, C1), the Unicode bidirectional controls and marks,
  line separators and invalid UTF-8. Titles, names, labels, reasons and
  handoff fields are one line. Bodies, design, acceptance, notes, comments
  and handoff notes may also hold newlines and tabs. Text stored before
  this rule is kept as it is.
- **`sfx` escapes what still arrives.** Any such character prints as a
  visible escape (`\x1b`, `\u202e`), and a one-line field's newline as
  `\n`, in every command, server error and quoted server stderr. With
  `--json`, the JSON encoding escapes them, so the value is the same and
  the output stays valid.
- **`prime` fences data.** It quotes titles and inbox text, each on one
  line, inside a `--- starfix data ---` fence, after a line saying they are
  data. A failed hook quotes the error.
- **MCP marks others' text.** Results that hold it carry `"untrusted"`,
  and the server's instructions tell the agent never to follow
  instructions in that text. A fix the server wrote is relayed quoted, for
  the user.

## Files in a repository

A cloned repository is someone else's code, so `sfx` treats its files with
care:

- **`.starfix.yaml`** must be yours and writable by no one else (on macOS
  and Linux), and `sfx` never looks for it above the repository's top
  level or your home directory ([Where sfx looks for the
  file](cli.md#where-sfx-looks-for-the-file)).
- **No commands in config.** A command in a committed file would run
  whatever the repository says, so there is no proxy-command setting.
  Google Cloud IAP is built in instead, and runs `gcloud` without a shell
  ([IAP](cli.md#servers-without-a-public-ip-google-cloud-iap)).
- **`sfx setup` follows no symbolic links** inside the repository, and
  keeps the MCP entry it writes to what starfix needs: `--check` reports
  anything else in the entry, such as `cwd` or `LD_PRELOAD`, and `--write`
  drops it. A JSON file with a duplicate key is refused ([What setup
  writes](agents.md#what-setup-writes-and-what-it-leaves-alone)).
- **Global hooks run everywhere.** A session-start hook set up with
  `--global` runs in every repository that has a `.starfix.yaml`, and
  connects to the server that file names. Prefer per-project setup
  ([`--global`](agents.md#setting-agents-up-for-every-repository---global)).

## Limits

Every principal is bounded: write rates, connections, sessions, inbox
size, labels, dependencies and acceptance items each have a cap, and text
fields and replies have fixed sizes. [Limits](server.md#limits) lists the
caps, their settings and what a refusal past each one says.

## Secrets

- No password goes on a command line, and a config file that holds one
  must be mode 0600 ([Settings](server.md#settings)).
- `.starfix.yaml` holds no secrets, and is meant to be committed.
- The installer never uses sudo and never edits your shell files.

## Releases

Each release's `checksums.txt` is signed with Ed25519. The public key is
built into `sfx` and `starfixd` (`internal/release/keys.go`) and into
`install.sh`. `sfx upgrade` and `starfixd upgrade` check the signature and
the archive's checksum before they install anything, and run the new
binary's `version` before swapping it in. `install.sh` checks the
checksum, and the signature when OpenSSL 3 is installed; without it, the
script warns, or fails with `--require-signature`. Every archive carries
GitHub build provenance as well.

The private key is a secret of the repository's `release` environment, so
only release runs the maintainer approves can use it.
[RELEASING.md](../RELEASING.md) has the details, and [Verify a download by
hand](install.md#verify-a-download-by-hand) the commands.

## Checks in CI

| Check | Runs |
|---|---|
| golangci-lint, with gosec, staticcheck, errcheck, govet and revive | Every pull request and push to `main` |
| `govulncheck`, against the Go vulnerability database | Every pull request and push, weekly on `main` (Mondays), and before every release build |
| `go mod download && go mod verify` | Every pull request and push, and before every release build |
| CodeQL, with the `security-extended` queries | Every pull request and push, and weekly |
| Dependabot, for Go modules and GitHub Actions | Weekly; minor and patch updates grouped |

## Dependencies

The dependency list is kept small on purpose, and a new dependency needs a
stated reason. Dependencies are not vendored. `go.sum` pins each module's
content hash, the Go checksum database rejects a module changed after
publication, and `go mod verify` checks that the local module cache has not
changed since download. Every GitHub Action is pinned by commit SHA.
