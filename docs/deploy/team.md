# Team: laptops, cloud agents and CI on one server

**For** a small team whose agents run in several places at once: on
laptops, in cloud sandboxes and in CI. Everyone shares one backlog, sees
who is working on what, and hands work between people and agents.

**Costs** about $19 a month on Google Cloud, or about $12 on a 2 GB
DigitalOcean droplet ([Costs](#costs)).

```text
laptops: people and their agents ──┐
cloud sandboxes: agents ───────────┼──SSH, host key pinned──▶ starfix.example.com:22
CI runners: ci-bot ────────────────┘                          sshd ─▶ starfixd ─▶ Dolt
```

## Choices this example makes

| Choice | This example | Why |
|---|---|---|
| Server | One e2-small VM (2 shared vCPUs, 2 GB) with Ubuntu 24.04 and a 20 GB balanced disk | Room to spare for a team ([What a server needs](../server.md#what-a-server-needs)) |
| Address | A static public IPv4 address and a DNS name, `starfix.example.com` | Cloud sandboxes and CI runners cannot easily join a private network |
| Port | 22, open to the internet, keys only, rate-limited | sshd refuses everything but the listed keys |
| Identity | One principal per person, and one per automated agent (`ci-bot`, `cloud-agent`) | A leaked CI key is revoked without touching anyone else |
| Admins | Two people | Someone can always override a stuck claim |
| Backups | The daily dump from [Running a server](../server.md#backups-and-restore), and daily disk snapshots kept a week | The snapshots carry the dumps and Dolt's own history off the machine; either one restores the tracker |

## Build it

1. **Create the VM.** Any provider works. On Google Cloud, with example
   names:

   ```sh
   gcloud compute addresses create starfix --region us-central1
   gcloud compute instances create starfix --zone us-central1-a --machine-type e2-small --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud --boot-disk-size 20GB --boot-disk-type pd-balanced --address starfix
   gcloud compute resource-policies create snapshot-schedule starfix-daily --region us-central1 --daily-schedule --start-time 04:00 --max-retention-days 7
   gcloud compute disks add-resource-policies starfix --zone us-central1-a --resource-policies starfix-daily
   ```

   In a project with the default network, its `default-allow-ssh` rule
   already admits port 22. Point `starfix.example.com` at the address.

2. **Set the server up** with [Running a server](../server.md#set-up-a-server),
   steps 1 to 6. List both admins in step 4, as `admins: [alice, bob]`.
   Because the port is public, turn on the firewall's rate limit too:

   ```sh
   ufw limit 22/tcp
   ufw --force enable
   ```

   Then add the daily dump from
   [Backups and restore](../server.md#backups-and-restore). It runs at
   03:15 on the VM's clock, which is UTC by default, before the 04:00 UTC
   snapshot that carries it off the machine.

3. **Add a principal per person.** Each person sends their public key, and
   you add one line for each ([Principals](../server.md#principals)).

4. **Give automated agents their own keys.** Make a key for CI on your own
   machine, and keep the private half only in the CI's secret store:

   ```sh
   ssh-keygen -t ed25519 -N '' -C ci-bot -f ci-bot
   ```

   Add `ci-bot.pub` on the server under the principal `ci-bot`, store the
   contents of `ci-bot` as the repository secret `STARFIX_KEY`, then
   delete both files. Do the same for a cloud agent's sandbox, with its
   own name.

5. **Connect the repositories.** Commit a `.starfix.yaml` to each:

   ```yaml
   project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
   server:
     host: starfix.example.com
     host_key: SHA256:…
   ```

   Then run `sfx setup --all --write` and commit what it writes
   ([agent guide](../agents.md)).

6. **Load the key in CI.** In GitHub Actions, one step installs `sfx` and
   loads the key into ssh-agent for the steps after it, whether they run
   `sfx` or an agent:

   ```yaml
   - name: Connect to starfix
     env:
       STARFIX_KEY: ${{ secrets.STARFIX_KEY }}
     run: |
       curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
       echo "$HOME/.local/bin" >> "$GITHUB_PATH"
       eval "$(ssh-agent -s)"
       printf '%s\n' "$STARFIX_KEY" | ssh-add -
       echo "SSH_AUTH_SOCK=$SSH_AUTH_SOCK" >> "$GITHUB_ENV"
   ```

   A cloud sandbox does the same in its setup script, with the key from
   its own secret store.

## Day to day

- **Who is doing what.** `sfx who` lists every session at work, with its
  principal, machine and agent, and the issues it holds.
- **Hand work across.** `sfx handoff ID NOTE --to ci-bot --release` passes
  an issue to another principal's inbox, with what is done and what is
  next ([Handoffs](../concepts.md#handoffs)).
- **Get noticed.** `@bob` in a comment puts a mention in bob's inbox. An
  agent hears about new items on its next tool call.
- **Standups write themselves.** `sfx digest --since 7d` gives the week's
  closed, started, stalled and blocked work as data, for a person or an
  agent to summarize.
- **Keep an eye on it.** The provider's console shows the VM's CPU,
  memory and disk, and `journalctl -u starfixd` shows refusals and errors
  ([Logs](../server.md#logs)).
- **Upgrades:** the server first, then each person runs `sfx upgrade`
  ([Upgrades](../server.md#upgrades)).

## Costs

Google Cloud list prices, us-central1, on demand, read on 7 October 2026:

| Item | A month |
|---|---|
| e2-small VM | $12.23 |
| 20 GB balanced persistent disk ($0.10 per GB) | $2.00 |
| Static external IPv4 address, in use ($0.005 an hour) | $3.65 |
| Daily snapshots kept a week ($0.05 per GB stored; they are incremental) | under $1.00 |
| Internet egress (first 1 GB free; starfix sends little) | about $0 |
| **Total** | **about $19** |

- **Commit to save.** A 1-year commitment takes 37% off the VM, and 3
  years 55%.
- **The free tier fits a trial.** One e2-micro (1 GB) a month is free in
  us-central1, us-west1 or us-east1, with 30 GB of standard disk, but its
  IPv4 address still costs $3.65. Dolt reached 1.4 GB in the load test, so
  give a busy team 2 GB.
- **Elsewhere:** a 2 GB DigitalOcean droplet is $12 a month, plus backups
  if you turn them on.

Sources: [VM pricing](https://cloud.google.com/products/compute/pricing/general-purpose),
[disks and snapshots](https://cloud.google.com/compute/disks-image-pricing),
[IP addresses and egress](https://cloud.google.com/vpc/network-pricing),
[free tier](https://docs.cloud.google.com/free/docs/free-cloud-features),
[DigitalOcean](https://www.digitalocean.com/pricing/droplets).

## When to move up

When the tracker must not be reachable from the internet, network access
must follow your company's accounts, or backups must survive a compromised
machine, see the [Enterprise](enterprise.md) example.
