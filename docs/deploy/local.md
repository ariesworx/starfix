# Local: one machine

**For** one developer whose agents should share a backlog across sessions:
several Claude Code, Codex or Cursor sessions at once, each claiming its
own issue, with nothing leaving the machine.

**Costs** nothing beyond the machine you have.

```text
your machine
  agents ──MCP──▶ sfx ──SSH──▶ sshd on localhost ──▶ starfixd ──▶ Dolt
  you ────CLI───▶ sfx
```

## What you need

- **Linux with systemd.** A Linux desktop or laptop works. So does WSL2 on
  Windows, once systemd is turned on in `/etc/wsl.conf`. On a Mac, use a
  Linux VM and run your agents inside it, or point `host` at the VM's
  address. To try starfix or test a change on a Mac, the
  [Docker test server](docker.md) runs a server in a container instead.
- **An SSH server**: `apt install openssh-server` on Ubuntu.
- **About 200 MB of free memory.**

## Build it

1. Follow [Running a server](../server.md#set-up-a-server), steps 1 to 5.
   In step 4, put your own name in `admins` in place of `alice`.
2. In step 6, add your own public key (`~/.ssh/id_ed25519.pub`) under that
   name. If nothing else needs sshd from the network, keep it on this
   machine: add `ListenAddress 127.0.0.1` at the top of the drop-in. On
   Ubuntu 23.04 and later, sshd starts from `ssh.socket`, so then also run
   `systemctl daemon-reload` and `systemctl restart ssh.socket`.
3. Read the host key from its file. Nothing crosses a network, so there is
   nothing to verify:

   ```sh
   ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
   ```

4. In each repository, commit a `.starfix.yaml` that points at
   `localhost`:

   ```yaml
   project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
   server:
     host: localhost
     host_key: SHA256:…
   ```

5. [Install sfx](../install.md#install-sfx), make sure your key is in
   ssh-agent (`ssh-add -l`), and set your agents up:

   ```sh
   sfx ready
   sfx setup --all --write
   ```

## Day to day

Your agents call `prime` when a session starts, then `start` and `finish`
for each issue. You can drive the same loop from a terminal:

```console
$ sfx create "Add a sign-out button" -p 2 -t feature
sf-3v33w75r
$ sfx dep add sf-3v33w75r sf-aukz3rip
$ sfx blocked
sf-3v33w75r  P2  open  Add a sign-out button  blocked by sf-aukz3rip
$ sfx start
sf-aukz3rip  P1  in_progress  bug  rev 2
Fix the login redirect
…
branch: fix/sf-aukz3rip-fix-the-login-redirect
$ sfx finish sf-aukz3rip --reason "fixed the redirect" --handoff "redirect now honors ?next=" --discovered "Add a test for ?next= redirects"
sf-aukz3rip rev 3
created sf-okyyzsck
$ sfx ready
sf-3v33w75r  P2  open  Add a sign-out button
sf-okyyzsck  P2  open  Add a test for ?next= redirects
```

- **Run agents side by side.** Each `start` claims a different issue, and
  `sfx who` lists the sessions at work and what each holds.
- **Nothing stays stuck.** If an agent crashes, its claim lapses within
  15 minutes and the issue is ready again
  ([Claims and leases](../concepts.md#claims-and-leases)).
- **See the day.** `sfx digest` summarizes what closed, started, stalled
  and was found on the way.

## Back it up

Use the daily dump from
[Backups and restore](../server.md#backups-and-restore), and copy
`/var/backups/starfix` to another disk or a cloud drive.

## What this setup cannot do

- Only this machine reaches the tracker. Teammates, cloud sandboxes and CI
  cannot. That is the [Team](team.md) example.
- When the machine sleeps, the tracker sleeps too. Agents on the machine
  sleep with it, so this rarely matters.
