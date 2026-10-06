# starfix

Issue tracker, shared memory and coordination for AI coding agents working
across sessions and machines. Agents use it through MCP; people administer it
from the command line. It is written in Go and stores its data in
[Dolt](https://github.com/dolthub/dolt) behind a small server.

**Status: stage 1 in progress.** The store, the `starfixd` server, the SSH
transport with its version handshake, and the `starfix` CLI for issues are
built. The MCP server, bd import, claims and the offline cache are not. Read
the [design](docs/design/starfix.md), the
[database choice](docs/design/database.md), and the draft
[Bearing orchestrator spec](docs/design/bearing.md).

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
starfix help                                 # every command; --json on all
```

The host key is pinned, never trusted on first use. Exit codes: 0 ok, 1
failure (with a `fix:` line), 2 usage, 3 protocol version refused.

starfix is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of, or
endorsed by, the beads or Dolt projects.

## License

Apache-2.0. See [LICENSE](LICENSE).
