# Docker test server

**For** trying starfix, or testing a branch, against a real `starfixd`
from a Mac or any machine with Docker, without touching a real server.

**Not for production.** It keeps no backups, and the
[deployment examples](README.md) are how to run starfix for real.

```text
your machine
  sfx ──SSH──▶ 127.0.0.1:2222 ──▶ container: sshd ──▶ starfixd ──▶ Dolt
                                     └── volume: Dolt's data, the host key, the config
```

One container runs the three programs, set up as in
[Running a server](../server.md#set-up-a-server): Dolt under its own
account with the same locked-down grants, starfixd as `starfix`, and
sshd with a forced command per key. starfixd runs without `--dev`, so it
makes every check it makes on a real server. The files are in
[deploy/docker](../../deploy/docker).

## What you need

- **Docker Desktop or OrbStack** on a Mac, Apple silicon or Intel, or
  Docker Engine 25 or later with Compose v2 on Linux. On Apple silicon the
  image is built for arm64, so nothing is emulated.
- **Go**, at the `go` line in [go.mod](../../go.mod), to build `sfx` from
  the same checkout, so client and server speak the same protocol.
- **An SSH key**, such as `~/.ssh/id_ed25519`, loaded into ssh-agent
  (`ssh-add -l` lists it).

## Start it

Run everything from the repository root, on the branch you want to test.
`COMPOSE_FILE` saves typing the file in every command:

```sh
export COMPOSE_FILE=deploy/docker/compose.yaml
export STARFIX_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)"
docker compose up -d --build --wait
docker compose logs --no-log-prefix starfix | grep -A 12 'test server ready' | tail -n 13
```

The build compiles `starfixd` and `sfx` from your checkout and downloads
Dolt at the release starfix pins, checking its SHA-256. The first start
makes the host key, the project and Dolt's password, which only the
config file holds (mode 0600) and nothing prints. The log ends with what
a client needs (these values are invented):

```text
starfix test server ready (not for production)
  host key    SHA256:…
  project     6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
  principals  alice
  admins      alice

Save this as .starfix.yaml in a scratch repository:

project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: localhost
  port: 2222
  host_key: SHA256:…
```

Later starts reuse the volume, so the project, the host key and the data
stay the same, and print the block again; the command above shows the
newest. The container generated the host key on your own machine,
so there is nothing to verify out of band.

| Variable | Default | Sets |
|---|---|---|
| `STARFIX_KEYS` | none | The principals: one `NAME TYPE KEY` line per key, such as `alice ssh-ed25519 AAAA…`. A file mounted at `/etc/starfix-keys` adds lines. A start that gives keys replaces those installed before; one that gives none keeps them |
| `STARFIX_ADMINS` | the first name | The [admins](../server.md#admins), comma-separated |
| `STARFIX_PROJECT` | random | The project UUID, used on the first start only |
| `STARFIX_PORT` | `2222` | The port on 127.0.0.1, also printed in `.starfix.yaml` |

sshd listens on 127.0.0.1 alone, so nothing beyond your machine can reach
it.

## Connect a scratch repository

Build `sfx` into a scratch repository and paste the printed block into
its `.starfix.yaml`:

```sh
mkdir -p ~/sfx-scratch
go build -o ~/sfx-scratch/sfx ./cmd/sfx
cd ~/sfx-scratch && git init -q
pbpaste > .starfix.yaml        # or paste it in an editor
./sfx ready
./sfx create "Try the test server" -p 2 -t task
./sfx ready
```

`sfx` signs in through ssh-agent. If your key is not in it, add
`key: ~/.ssh/id_ed25519` to `.starfix.yaml`. The rest of
[Using sfx](../cli.md) and the [agent guide](../agents.md) work as on any
server.

**To act as a second principal**, such as a non-admin, give it a key and
start the server again with both keys. Compose recreates the container,
and the volume keeps the data. alice stays the only admin, as the first
name:

```sh
ssh-keygen -q -t ed25519 -N '' -C bob@example.com -f ~/sfx-scratch/bob
export STARFIX_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)
bob $(cat ~/sfx-scratch/bob.pub)"
docker compose up -d --wait
```

Then give bob a repository of his own, with the same `.starfix.yaml`
plus `key: ~/sfx-scratch/bob`, and run `~/sfx-scratch/sfx` there.

## Test a change

After you change the code, rebuild the image and `sfx`:

```sh
docker compose up -d --build --wait
go build -o ~/sfx-scratch/sfx ./cmd/sfx
```

The volume keeps its data, and a new migration runs on it when starfixd
starts. An image older than the volume's schema refuses to open it
(migrations only go forward): wipe the volume, below, to go back.

## Run the smoke test

```sh
deploy/docker/smoke.sh
```

It builds the image and `sfx` from the checkout and starts its own
server, on port 2223 with two invented principals, so a server on 2222
is left alone. It drives `sfx` through create, ready, start, comment,
hours, memory, finish with discovered work, dependencies, show, prices,
token usage and cost, plans and the digest, checks that a non-admin is
refused admin commands, and restarts the server to check that nothing
changed. It removes its server and volume, prints one line, and exits
non-zero on the first failure. CI does not run it. Arguments go to
`docker build`; behind a proxy that intercepts TLS, pass its CA with
`--secret id=ca,src=FILE`, which never reaches the image.

## Look inside

| To | Run |
|---|---|
| Follow the logs of all three programs | `docker compose logs -f` |
| Open a shell | `docker compose exec starfix bash` |
| Query Dolt as its own user | `docker compose exec -u dolt -w /var/lib/dolt/data/starfix starfix dolt sql` |
| Read the config | `docker compose exec starfix cat /etc/starfix/starfixd.yaml` (it holds the password) |

## Stop and wipe

```sh
docker compose stop              # stop; docker compose start resumes
docker compose down              # remove the container, keep the volume
docker compose down -v           # remove the volume too: data, host key and project
docker image rm starfix-dev:local
```

After `down -v`, the next start makes a new project and host key, so
update `.starfix.yaml`.

## How it differs from a real server

| Running a server | Here | Why |
|---|---|---|
| Ubuntu 24.04 with systemd units for Dolt and starfixd | Ubuntu 24.04; the entrypoint starts the three programs and stops all of them when one exits | A container has no systemd. Each program still runs as its own user with a bare environment, `umask 077` and no way to gain privileges |
| starfixd installed from a signed release into `/opt/starfix/bin` | Built from the checkout into `/usr/local/bin`; no `systemd_unit` and no sudo rule | It tests what is checked out. Rebuild the image instead of running `starfixd upgrade` |
| The host key in `/etc/ssh`; `/var/lib/dolt`, `/etc/starfix` and `~starfix/.ssh` on the disk | The same paths, linked into the `/state` volume, with the host key in `/state/hostkeys` | Everything survives a rebuild, and `down -v` wipes it |
| sshd serves every account | `AllowUsers starfix`, with passwords off | Nobody else signs in |
| A daily dump | No backups | It is for testing |
