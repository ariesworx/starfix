# Running a server

A starfix server is one Linux machine running three programs:

| Program | Role |
|---|---|
| sshd | The front door. It checks each person's or agent's key and runs `starfixd stdio` for them |
| `starfixd serve` | The daemon. It owns the data and applies every change in order |
| Dolt | The database, listening on loopback for starfixd alone |

One server holds one project. This page sets a server up step by step, then
covers principals, settings, limits, upgrades, backups and troubleshooting.
For complete deployments with costs, see the
[deployment examples](deploy/README.md).

## What a server needs

- **Linux**, amd64 or arm64, with systemd and OpenSSH. The steps below were
  tested on Ubuntu 24.04. `starfixd serve` refuses to run on other systems,
  because only Linux lets it check which user connects to its socket.
- **Dolt**, at the release starfix is tested with: `Dolt` in
  [internal/version](../internal/version/version.go), 2.4.2 for starfix
  v0.2.0. Each release's notes name it.
- **Little hardware.** Idle, Dolt used about 90 MB of memory and starfixd
  about 13 MB. Under the stage 0 load test Dolt reached 1.4 GB and about
  250 writes a second on 4 vCPUs
  ([results](../spike/dolt/RESULTS.md)), roughly ten times what a busy
  team of agents needs. 2 GB of memory suits a team; the data starts at
  under 1 MB.

## Set up a server

Run these steps as root (`sudo -i`). Each step ends with a command that
shows it worked, and none puts a password on a command line.

### 1. Install Dolt

Use `arm64` in place of `amd64` on an Arm machine:

```sh
curl -fsSLO https://github.com/dolthub/dolt/releases/download/v2.4.2/dolt-linux-amd64.tar.gz
tar -xzf dolt-linux-amd64.tar.gz
install -m 0755 dolt-linux-amd64/bin/dolt /usr/local/bin/dolt
dolt version
```

### 2. Run Dolt under its own account

Dolt gets its own Unix user and listens on loopback only:

```sh
useradd --system --home-dir /var/lib/dolt --shell /usr/sbin/nologin dolt
install -d -o dolt -g dolt -m 0700 /var/lib/dolt /var/lib/dolt/data /var/lib/dolt/cfg
install -d -m 0755 /etc/dolt
cat > /etc/dolt/config.yaml <<'EOF'
log_level: info
listener:
  host: 127.0.0.1
  port: 3306
data_dir: /var/lib/dolt/data
cfg_dir: /var/lib/dolt/cfg
system_variables:
  secure_file_priv: /var/lib/dolt/no-files
EOF
cd /var/lib/dolt
sudo -u dolt -H dolt config --global --add user.name "starfix server"
sudo -u dolt -H dolt config --global --add user.email starfix@localhost
sudo -u dolt -H dolt config --global --add metrics.disabled true
```

- `secure_file_priv` names a directory that does not exist, so no account
  can read or write files through Dolt, whatever its grants. starfixd
  refuses to start when it is empty ([why](security-model.md#the-dolt-account)).
- `cfg_dir` keeps Dolt's accounts outside the data directory.
- Run `dolt` commands from a directory the `dolt` user can read, such as
  `/var/lib/dolt`, or it fails to load its config.

Then run it under systemd. Save this as `/etc/systemd/system/dolt.service`:

```ini
[Unit]
Description=Dolt SQL server for starfix
After=network.target

[Service]
User=dolt
Group=dolt
Environment=HOME=/var/lib/dolt
WorkingDirectory=/var/lib/dolt/data
ExecStart=/usr/local/bin/dolt sql-server --config /etc/dolt/config.yaml
Restart=on-failure
RestartSec=5
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/dolt
PrivateTmp=yes
PrivateDevices=yes
IPAddressDeny=any
IPAddressAllow=localhost
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload
systemctl enable --now dolt
journalctl -u dolt -n 3
```

The log ends with `Server ready. Accepting connections.`

### 3. Install starfixd

starfixd runs as its own user, `starfix`. sshd runs every key's forced
command as this user:

```sh
useradd --system --create-home --home-dir /var/lib/starfix --shell /bin/sh starfix
usermod -p '*' starfix
install -d -o starfix -g starfix -m 0755 /opt/starfix/bin
cd /var/lib/starfix
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sudo -u starfix -H sh -s -- --server --dir /opt/starfix/bin --require-signature
ln -sfn /opt/starfix/bin/starfixd /usr/local/bin/starfixd
starfixd version
```

- **The shell must be real.** sshd runs a forced command through the
  account's shell, so `/usr/sbin/nologin` would refuse every connection.
- **The password is `*`, not locked.** No password matches `*`. A locked
  account (`passwd -l`, a `!` in `/etc/shadow`) makes sshd refuse even
  keys when PAM is off.
- **starfix owns the binary's directory**, so `starfixd upgrade` can
  replace it without root. `/usr/local/bin/starfixd` gives the forced
  command a path that never changes.
- The installer checks the release's signature and checksum
  ([Install](install.md)).

### 4. Create the database and the config file

This creates starfix's database and an account with rights on it alone,
gives Dolt's `root` a random password, and writes the config file. Both
passwords travel on stdin:

```sh
cd /var/lib/dolt
(
  set -e
  umask 077
  dbpw=$(openssl rand -hex 24)
  rootpw=$(openssl rand -hex 24)
  printf "CREATE DATABASE starfix;\nCREATE USER 'starfix'@'localhost' IDENTIFIED BY '%s';\nGRANT ALL ON starfix.* TO 'starfix'@'localhost';\nALTER USER 'root'@'localhost' IDENTIFIED BY '%s';\n" "$dbpw" "$rootpw" |
    sudo -u dolt -H env DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port 3306 --no-tls -u root sql
  install -d -o root -g starfix -m 0750 /etc/starfix
  printf 'dsn: starfix:%s@tcp(127.0.0.1:3306)/starfix\nproject: %s\nsystemd_unit: starfixd.service\nadmins: [alice]\n' "$dbpw" "$(cat /proc/sys/kernel/random/uuid)" |
    install -o starfix -g starfix -m 0600 /dev/stdin /etc/starfix/starfixd.yaml
)
grep project /etc/starfix/starfixd.yaml
```

- **The account can do anything to its one database and nothing else**:
  no global privilege, no `FILE`, no `GRANT OPTION` (`ALL` does not include
  it). starfixd checks this every time it opens the store.
- **Nobody needs `root`'s password**, so the step forgets it. Dolt's own
  user can still administer the server from the data directory:
  `cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt sql`. You may
  drop `root` instead (`DROP USER 'root'@'localhost';`); Dolt does not
  bring it back on restart.
- **`project`** is the project's UUID. Every repository's `.starfix.yaml`
  names it.
- **`admins`** lists the first principal, `alice`, who you let in at step
  6. [Settings](#settings) lists every other key.

The step runs once: a second run finds `root` protected and stops.

### 5. Run starfixd under systemd

Save this as `/etc/systemd/system/starfixd.service`:

```ini
[Unit]
Description=starfix server
After=network.target dolt.service
Requires=dolt.service

[Service]
User=starfix
Group=starfix
ExecStart=/opt/starfix/bin/starfixd serve
Restart=on-failure
RestartSec=5
RuntimeDirectory=starfix
RuntimeDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
```

`RuntimeDirectory` makes `/run/starfix` with mode 0700, so only the
`starfix` user reaches the daemon's socket. Then let `starfixd upgrade`
restart the unit, and start it:

```sh
echo 'starfix ALL=(root) NOPASSWD: /usr/bin/systemctl restart starfixd.service' > /etc/sudoers.d/starfix
chmod 0440 /etc/sudoers.d/starfix
visudo -cf /etc/sudoers.d/starfix
systemctl daemon-reload
systemctl enable --now starfixd
journalctl -u starfixd -n 3
```

The log shows `starfixd serving` with the socket, the project and the
admins. If it shows a refusal instead, its `fix:` says what to change.

### 6. Let the first principal in

Each key gets one line in `~starfix/.ssh/authorized_keys`. The line's
forced command names the principal, so sshd, not the client, decides who
connects:

```sh
install -d -o starfix -g starfix -m 0700 /var/lib/starfix/.ssh
echo 'restrict,command="/usr/local/bin/starfixd stdio --principal alice" ssh-ed25519 AAAA… alice@example.com' >> /var/lib/starfix/.ssh/authorized_keys
chown starfix:starfix /var/lib/starfix/.ssh/authorized_keys
chmod 0600 /var/lib/starfix/.ssh/authorized_keys
```

`restrict` turns off forwarding, terminals and the rest. Never add a
`ForceCommand` for the account: it would override each key's `command=`,
and so its principal.

Then harden sshd for the account. Save this as
`/etc/ssh/sshd_config.d/00-starfix.conf`:

```text
MaxStartups 10:30:60
ClientAliveInterval 60
Match User starfix
    AuthenticationMethods publickey
    DisableForwarding yes
    PermitTTY no
```

```sh
install -d -m 0755 /run/sshd
sshd -t
systemctl try-reload-or-restart ssh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

- `sshd -t` checks the config. It needs `/run/sshd`, which systems that
  start sshd on the first connection have not made yet.
- The unit is `sshd` rather than `ssh` on some distributions.
- `DisableForwarding` means no tunnel to Dolt, even from a key line that
  lacks `restrict`.
- `MaxStartups` drops floods of unauthenticated connections early.
- `ClientAliveInterval` keeps an agent's long session alive through
  tunnels that drop idle connections.
- A `Match` block in a drop-in file applies to that file only (OpenSSH 9.6
  checked).
- On a machine whose admins all sign in with keys, also add
  `PasswordAuthentication no` above the `Match` line.

The last command prints the host key fingerprint, `SHA256:…`. Give it to
everyone who connects.

**On a public port**, also rate-limit new connections at the firewall
(`ufw limit 22/tcp`). Skip that when everyone arrives through a few shared
addresses (Google Cloud IAP, a VPN or a NAT), because it would throttle
the whole team. To keep sshd off the internet altogether, put it behind a
VPN such as WireGuard; starfix works the same either way.

OpenSSH 9.8 and later also turn away, for a while, an address that fails
to log in or crashes a session (`PerSourcePenalties`, on by default).
Behind shared addresses, one bad client could then lock out the team, so
list those ranges in `PerSourcePenaltyExemptList`, above the `Match` line
(it is a global setting).

### 7. Connect a repository

Commit a `.starfix.yaml` to the repository, with the project UUID from step
4 and the fingerprint from step 6:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix.example.com
  host_key: SHA256:…
```

Then, on alice's machine, inside the repository:

```console
$ sfx create "Fix the login redirect" -p 1 -t bug
sf-aukz3rip
$ sfx ready
sf-aukz3rip  P1  open  Fix the login redirect
```

[Using sfx](cli.md#connect-a-repository) explains every field of the file,
and the [agent guide](agents.md) sets agents up in the repository.

## Principals

A principal is a name for a person or an agent identity, such as `alice`.
It comes from the key line that matched, never from the client.

- **Add someone** by appending a line, as in [step
  6](#6-let-the-first-principal-in). A principal may have several lines,
  one per key, for example a laptop and a desktop.
- **Name rules:** lowercase, starting with a letter, then letters, digits,
  `.`, `_` or `-`, up to 64 characters. `starfixd` (the claim reaper) and
  `import` (the bd importer) are reserved.
- **Agents on someone's machine** use that person's key, so their work is
  theirs. Give an agent that runs on its own, in CI or in a cloud sandbox,
  its own principal and key (for example `ci-bot`). Its work then shows
  under its own name, and its key can be revoked alone.
- **Remove someone** by deleting their lines. That stops new connections.
  To end their open sessions too, stop their bridges:
  `pkill -f -- 'stdio --principal bob$'`.

## Admins

Admins are principals named under `admins:` in the config file, or in
`STARFIXD_ADMINS` (comma-separated). `serve` reads the list when it starts.
Only an admin may change an issue another principal holds, or close one
with acceptance items still open (`close --force`). A change to an issue
another principal holds is recorded first as an `admin.override` event
that names the holder; a forced close records the open items in its close
event. Nothing over the protocol or MCP reads or changes the list, and a
reserved name cannot be an admin.

## Settings

Settings come from flags, then `STARFIXD_*` environment variables, then
the config file (`/etc/starfix/starfixd.yaml`, or `--config FILE`, or
`STARFIXD_CONFIG`), then defaults.

| Key | Holds | Default | Flag and variable |
|---|---|---|---|
| `dsn` | Dolt's address and account, in [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql#dsn-data-source-name) form | required | `--dsn`, `STARFIXD_DSN` |
| `project` | The project's UUID, in lowercase. A client naming another project is refused | required | `--project`, `STARFIXD_PROJECT` |
| `socket` | The daemon's unix socket | `/run/starfix/starfixd.sock` | `--socket`, `STARFIXD_SOCKET` |
| `prefix` | The prefix of new issue ids | `sf` | `--prefix` |
| `admins` | [Admins](#admins) | none | `STARFIXD_ADMINS` |
| `systemd_unit` | The unit `starfixd upgrade` restarts and health-checks | none | |
| `log_level` | `debug`, `info`, `warn` or `error` ([Logs](#logs)) | `info` | `--log-level`, `STARFIXD_LOG_LEVEL` |
| `log_format` | `text` (key=value) or `json`, for a log shipper | `text` | `--log-format`, `STARFIXD_LOG_FORMAT` |
| `latest` | The newest starfix release, set by hand. Clients' `prime` notes when this server is older | none | |
| `limits` | [Limits](#limits) | defaults | |

- **A password never goes on the command line.** `--dsn` with a password is
  refused, because it would show in the process list. A config file that
  holds one must be mode 0600.
- **The Dolt account is checked** by every command that opens the store:
  `serve`, `import-bd`, `export-bd` and `upgrade`. [The Dolt
  account](security-model.md#the-dolt-account) lists what they refuse.
- **`--dev`** is for a developer's own machine. It lets `serve` run off
  Linux. With `--dev`, the commands that open the store also accept
  `--allow-unsafe-dolt`, which lets a default Dolt through with a warning.
  No config file or variable can set either.

## Limits

`limits:` in the config file bounds what one request or one principal can
make the daemon do. Leave a field out for its default:

```yaml
limits:
  write_rate: 20
  write_burst: 200
```

| Setting | Default | Bounds |
|---|---|---|
| `labels_per_issue` | 50 | Labels on one issue, and so in one `create` |
| `acceptance_items` | 200 | Items in acceptance text, and item numbers in one `accept` or `finish` |
| `deps_per_issue` | 200 | Edges out of one issue |
| `sessions_per_principal` | 256 | A principal's rows in the `who` registry; a new session past it drops the least recently seen |
| `agent_keep` | `7d` | How long an unseen registry row is kept. A principal's latest row is always kept, so it stays mentionable |
| `inbox_unread` | 1000 | A principal's unread inbox items; past it, the oldest are marked read (still under `inbox --all`) |
| `notices_per_minute` | 10 | Mentions, assignments and handoffs one principal can send another a minute; the rest are not delivered. Lost claims always are |
| `inbox_keep` | `30d` | How long a read inbox item is kept |
| `conns` | 1024 | Connections past the handshake; twice this caps sockets still in it |
| `conns_per_principal` | 32 | One principal's connections |
| `idle_timeout` | `10m` | A connection that sends nothing this long is closed with a note, unless it watches its inbox (`sfx mcp` and `sfx watch` do) |
| `write_rate`, `write_burst` | 10, 100 | Each principal's write token bucket: writes a second, and how many at once. Reads are not counted |
| `refusal_logs` | 20 | Refusal log lines per principal a minute |

A request past a per-request cap is refused with `invalid`. A write past
the write rate is refused with `busy`, and its fix says how long to wait. A
connection past a connection cap is also refused with `busy`, and its fix
asks you to close other starfix sessions or wait for them to end.

Fixed caps that no setting changes:

- **Text:** titles 500 bytes, names 255, bodies and comments 64 KiB.
- **Pages:** no reply may pass the 4 MiB frame. `comments` and `history`
  return the newest page (up to 100 entries, 500 with a limit, about 1 MiB
  of text) and a cursor to the page before, which `sfx` follows.
- **Lists:** `who` lists at most 100 sessions and counts the rest; `show`
  lists at most 200 edges and `blocked` 50 blockers an issue, each with a
  count of the rest.
- **Similar issues:** lookups read closed titles from a cache refreshed on
  close and reopen, or after a minute.

## Logs

starfixd logs to stderr, which systemd sends to the journal
(`journalctl -u starfixd`).

| Level | Logs |
|---|---|
| `debug` | Connections and each successful request |
| `info` | Startup, refusals, handshake failures and expired claims. Refusals are capped at `refusal_logs` a minute per principal |
| `warn` | Connections refused at the connection cap, socket peers refused, how many refusal lines the cap left out, and failures to accept a connection that may pass, such as running out of file descriptors, which starfixd retries after a pause of up to a second |
| `error` | Internal errors |

Any other failure to accept connections stops starfixd: it prints the
error and exits 1, and the unit's `Restart=on-failure` starts it again.

## Upgrades

Upgrade the server first, then the clients. A newer server accepts older
clients. A newer client that needs a newer protocol is refused by an older
server at the handshake, with exit code 3 and a fix that says to upgrade
the server.

On the server, as the `starfix` user:

```sh
cd /var/lib/starfix
sudo -u starfix -H starfixd upgrade --check
sudo -u starfix -H starfixd upgrade
```

`starfixd upgrade` does this, in order:

1. Downloads the release and checks its signature and checksum.
2. Commits the database and tags it `starfix-<old version>`, a point to go
   back to.
3. Swaps the binary.
4. With `systemd_unit:` set, or `--restart`, restarts the unit through the
   sudo rule from [step 5](#5-run-starfixd-under-systemd). It then checks
   that the new version answers on the socket within 30 seconds. If it does
   not, upgrade puts the old binary back and restarts it.

It refuses to run as root, except with `--check`. `--to vX.Y.Z` installs a given release, but
never an older one, because migrations only go forward. `--rollback`
restores the binary the last upgrade replaced. Take a
[dump](#backups-and-restore) first if you want a copy outside Dolt.

Each person runs `sfx upgrade` on their own machine. It never runs by
itself ([Install](install.md#upgrade-and-roll-back)).

Keep Dolt at the release starfix names, and change it only with a starfix
release that does.

## Backups and restore

Back up three things:

| What | Holds | How |
|---|---|---|
| The database | Every issue, comment and event | A daily `dolt dump`, below |
| `/etc/starfix/starfixd.yaml` | The project UUID and Dolt's password | With your other secrets |
| `~starfix/.ssh/authorized_keys` and `/etc/ssh/ssh_host_ed25519_key*` | Who may connect, and the host key every `.starfix.yaml` pins | With your other secrets |

A dump writes the database's current rows, the event log included, as SQL.
It runs while starfixd serves. This cron entry keeps a week of dumps,
one file per weekday. Save it as `/etc/cron.d/starfix-backup`:

```text
15 3 * * * root cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt dump -f -r sql -fn /var/backups/starfix/starfix-$(date -u +\%a).sql
```

```sh
install -d -o dolt -g dolt -m 0700 /var/backups/starfix
```

Copy the files off the machine every day: to object storage with
versioning, or with your backup tool. A dump does not hold Dolt's own
commit history. Disk snapshots do, so take both if you can.

**To restore**, stop starfixd: a dump recreates the `starfix` database it
came from, dropping each table first.

```sh
systemctl stop starfixd
cd /var/lib/dolt/data
sudo -u dolt -H dolt sql -q "DROP DATABASE starfix"
sudo -u dolt -H dolt sql < /var/backups/starfix/starfix-Mon.sql
systemctl start starfixd
```

On a new machine, follow steps 1 to 6 first. Then put back the config
file's `project`, the host keys and `authorized_keys`, and restore the
dump. If the host key changed, everyone must update `host_key`. `sfx`
refuses the new key until they do.

## Storage growth

The event log is never pruned. It is the history, and Dolt keeps every
version of it. An event keeps a text over 8 KiB as its first 512 bytes,
its length and its SHA-256, so an edit loop over long fields grows the log
by about a kilobyte a write, not by the text. The write rate limit bounds
how fast one principal can grow it.

Watch the size of `/var/lib/dolt/data`. If it grows, run `dolt gc` in a
quiet hour: `cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt gc`.

## starfixd commands

| Command | Does |
|---|---|
| `serve` | Run the daemon. Flags: `--config`, `--dsn`, `--socket`, `--project`, `--prefix`, `--log-level`, `--log-format`, `--dev`, `--allow-unsafe-dolt` |
| `stdio --principal NAME` | The sshd forced command: bridge one SSH session to the daemon |
| `import-bd [--principal NAME] [--dry-run] [--json] FILE` | Import a bd export (`-` for stdin); see [Moving from bd](migrate-from-bd.md) |
| `export-bd [-o FILE]` | Write the store in bd's JSONL format |
| `upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]` | Replace the binary with a verified release ([Upgrades](#upgrades)) |
| `version` | Print the version and protocol range |

## Troubleshooting

| What you see | Cause | Fix |
|---|---|---|
| `sfx: host key mismatch` | The server's key is not the pinned one | Check the new key with the admin, out of band, before you change `host_key` |
| `sfx: … refused your SSH key`, though the key line is right | The `starfix` account is locked (`!` in `/etc/shadow`), or its shell is `nologin` | `usermod -p '*' starfix`; `usermod -s /bin/sh starfix` |
| `sfx: starfixd is not running on the server` | The daemon is stopped, or failed at start | `journalctl -u starfixd` shows why; `systemctl start starfixd` |
| `project "…" is not served by this server` | `.starfix.yaml` and the config file name different projects | Make the two `project` values match |
| starfixd refuses the Dolt account | The account is `root` or has too many rights, or `secure_file_priv` is empty | Run the SQL its fix names, or redo [step 4](#4-create-the-database-and-the-config-file) |
| `sfx` exits with code 3 | Client and server speak incompatible protocols | Upgrade the server, then the client |
| `busy` | A [limit](#limits) | Wait as the fix says, or raise the limit |
