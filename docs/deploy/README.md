# Deployment examples

Three worked examples, from one laptop to a company. All three run the same
server, set up as in [Running a server](../server.md). What changes is
where it runs, how people and agents reach it, and how it is protected and
backed up.

| | [Local](local.md) | [Team](team.md) | [Enterprise](enterprise.md) |
|---|---|---|---|
| **For** | One developer and their agents | A small team, with agents on laptops, in cloud sandboxes and in CI | A company that keeps the tracker off the internet |
| **Server** | Your own Linux machine, WSL2 or a Linux VM | A small cloud VM | A private cloud VM with a separate data disk |
| **Clients connect** | Over SSH to `localhost` | Over SSH across the internet, with the host key pinned | Through Google Cloud IAP, with no public address |
| **Who may connect** | You | One key per person and per automated agent | Keys per person, and an IAM group for the network path |
| **Backups** | Daily dumps to another disk | Daily dumps, carried off the machine by daily disk snapshots | Snapshots, and dumps to a bucket nothing on the VM can delete |
| **Monitoring** | None needed | The provider's VM metrics and starfixd's log | Alerts on the VM, its disk and starfixd's errors, and a budget |
| **Cost a month** | $0 | About $19 | About $65 to $114 |

Costs are Google Cloud list prices for us-central1, on demand and before
tax, read on 7 October 2026. Each example links the pricing pages; check
them before you rely on a figure.

To try starfix, or test a branch, on any machine with Docker, use the
[Docker test server](docker.md). It is for testing, not a deployment.

## What every deployment shares

- **One server holds one project.** Several projects need several servers.
- **No high availability.** While the server is down, agents keep working
  on code but cannot claim or finish issues. Recovery is a restart, or a
  [restore](../server.md#backups-and-restore) that loses what changed
  since the last backup.
- **Keys are identity.** There is no single sign-on: you manage key lines
  like any other access list ([Principals](../server.md#principals)).
- **Pre-1.0.** starfix is not ready for production use yet; see the status
  in the [README](../../README.md).

## Moving up

A deployment's data moves with one dump: [back up](../server.md#backups-and-restore)
the old server, restore on the new one, and keep its `project` setting.
Keep the host key too, or update `host_key` in every `.starfix.yaml`.
