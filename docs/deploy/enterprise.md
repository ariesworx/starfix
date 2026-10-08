# Enterprise: private, controlled and recoverable

**For** a company with many developers and agents, where:

- the tracker must not be reachable from the internet;
- network access must follow employment, through company accounts;
- backups must survive an admin's mistake or a compromised machine.

**Costs** about $65 a month on Google Cloud with an e2-standard-2, or about
$114 with an e2-standard-4 ([Costs](#costs)).

```text
laptops ───┐  gcloud: IAP TCP forwarding,
CI runners ┘  for members of starfix-users@example.com only
     │
     ▼
Google Cloud IAP ──▶ VM with no public address ──▶ sshd ─▶ starfixd ─▶ Dolt (data disk)
                      ├─ Cloud NAT, outbound only: releases and OS updates
                      ├─ daily disk snapshots, kept 14 days
                      └─ daily dumps ─▶ Cloud Storage, under a retention policy
```

## Choices this example makes

| Choice | This example | Why |
|---|---|---|
| Network | No external address. The firewall admits only IAP's range, `35.235.240.0/20`, on port 22 | Nothing on the internet reaches sshd |
| Access | IAP TCP forwarding. The IAP-secured Tunnel User role goes to a Google group | Leaving the group cuts a person off at the network, even before their key line is removed |
| Identity | One principal per person, with keys in ssh-agent; service principals for CI | Every change names a person or a known automation |
| CI | Workload Identity Federation, so no service account key exists, and an SSH key from the CI's secret store | Nothing long-lived to leak from CI but the SSH key, which is revoked alone |
| Server | e2-standard-4 (4 vCPUs, 16 GB), the machine the load test used; e2-standard-2 for a smaller company | Headroom for many agents |
| Disks | A 20 GB boot disk, and a 50 GB data disk mounted at `/var/lib/dolt` | The boot disk can be rebuilt without touching the data |
| Outbound | Cloud NAT | Upgrades download releases from GitHub, and the OS needs updates |
| Backups | Daily snapshots kept 14 days, and daily dumps to a bucket with a retention policy. The VM may create objects there and do nothing else | A compromised VM cannot delete or overwrite a backup |
| Monitoring | Alerts on the VM stopping, on disk use over 80% (reported by the Ops Agent), and on `level=ERROR` lines in starfixd's journal; a budget with alerts | Problems surface before people notice |
| Admins | Two named admins. Key lines are managed by your configuration management | Changes to access are reviewed like code |

## Build it

1. **Network.** Admit IAP's range, and give the subnet outbound access
   through Cloud NAT. With example names:

   ```sh
   gcloud compute firewall-rules create allow-iap-ssh --network default --allow tcp:22 --source-ranges 35.235.240.0/20 --target-tags starfix
   gcloud compute routers create starfix-router --network default --region us-central1
   gcloud compute routers nats create starfix-nat --router starfix-router --region us-central1 --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges
   ```

2. **The VM.** Give it its own service account, with only the roles to
   write logs and metrics. Then create it with no address and a separate
   data disk:

   ```sh
   gcloud compute instances create starfix --zone us-central1-a --machine-type e2-standard-4 --no-address --shielded-secure-boot --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud --boot-disk-size 20GB --boot-disk-type pd-balanced --create-disk name=starfix-data,size=50GB,type=pd-balanced,auto-delete=no --tags starfix --service-account starfix-vm@example-project.iam.gserviceaccount.com --scopes cloud-platform
   ```

   Add a 14-day snapshot schedule for both disks, as in the
   [Team](team.md#build-it) example.

3. **The server.** Format the data disk and mount it at `/var/lib/dolt`
   (in `/etc/fstab`) before [Running a server](../server.md#set-up-a-server),
   steps 1 to 6. Add `RequiresMountsFor=/var/lib/dolt` to the `[Unit]`
   section of `dolt.service`. Skip the firewall rate limit: everyone
   arrives from IAP's few addresses, so it would throttle the whole
   company. For the same reason, on OpenSSH 9.8 or later, add
   `PerSourcePenaltyExemptList 35.235.240.0/20` above the `Match` line.

4. **Access.** Grant the group the tunnel role on the instance. Members
   also need to look the instance up, for example with Compute Viewer on
   the project:

   ```sh
   gcloud compute instances add-iam-policy-binding starfix --zone us-central1-a --member group:starfix-users@example.com --role roles/iap.tunnelResourceAccessor
   ```

5. **The host key.** Publish the fingerprint where developers already
   trust what they read. For example, have the VM write it to a guest
   attribute at boot, so `gcloud compute instances get-guest-attributes`
   shows it to anyone who may look the instance up.

6. **The repositories.** Each `.starfix.yaml` gets an `iap` block
   ([Servers without a public IP](../cli.md#servers-without-a-public-ip-google-cloud-iap)):

   ```yaml
   project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
   server:
     host: starfix
     host_key: SHA256:…
     iap:
       project: example-project
       zone: us-central1-a
   ```

7. **Backups.** Make a bucket with a retention policy, and let the VM's
   service account create objects in it and nothing else:

   ```sh
   gcloud storage buckets create gs://example-starfix-backups --location us-central1 --uniform-bucket-level-access --retention-period 30d
   gcloud storage buckets add-iam-policy-binding gs://example-starfix-backups --member serviceAccount:starfix-vm@example-project.iam.gserviceaccount.com --role roles/storage.objectCreator
   ```

   Extend the daily [dump](../server.md#backups-and-restore) to copy each
   file to the bucket under a new, dated name. The role cannot overwrite
   or delete an object, and the retention policy stops anyone else from
   deleting one for 30 days. Once you are sure of the period, lock the
   policy. Locking cannot be undone.

8. **Monitoring and budget.** Create the alerts in the table above, and a
   budget with alerts on the project.

9. **CI.** Give CI an identity through
   [Workload Identity Federation](https://cloud.google.com/iam/docs/workload-identity-federation)
   with the tunnel role, and load its SSH key as in the
   [Team](team.md#build-it) example.

## Day to day

- **Overrides are on the record.** When a claim is stuck, an admin can
  change the issue or force a close, and each override is an
  `admin.override` event naming the holder.
- **Every change is an event.** `sfx history ID` shows who changed an
  issue and when, and `sfx digest --by PRINCIPAL` shows one person's or
  automation's work over a window.
- **Offboarding:** remove the person from the group, delete their key
  lines, and stop their open sessions
  ([Principals](../server.md#principals)).
- **Upgrades:** take a dump, upgrade the server, then tell people to run
  `sfx upgrade` ([Upgrades](../server.md#upgrades)).

## Costs

Google Cloud list prices, us-central1, on demand, read on 7 October 2026:

| Item | e2-standard-2 | e2-standard-4 |
|---|---|---|
| VM | $48.92 | $97.84 |
| 20 GB balanced boot disk | $2.00 | $2.00 |
| 50 GB balanced data disk | $5.00 | $5.00 |
| Snapshots, 14 days kept ($0.05 per GB stored; incremental, estimated) | about $3.00 | about $3.00 |
| Cloud NAT: gateway ($0.0014 an hour), NAT address ($0.005 an hour), data ($0.045 per GB) | about $4.72 | about $4.72 |
| IAP TCP forwarding | $0 | $0 |
| Backup bucket (Standard, a few GB) | under $1.00 | under $1.00 |
| Logging and monitoring, within the free allotments | $0 | $0 |
| **Total** | **about $65** | **about $114** |

- **Commit to save.** A 1-year commitment takes 37% off the VM line, and 3
  years 55%. With e2-standard-4, that brings the total to about $77 or
  about $60.
- **Customer-managed keys,** if your policy requires them, add $0.06 per
  key version a month and $0.03 per 10,000 operations.
- **Alerting is free for now.** Cloud Monitoring starts charging for
  alerting no sooner than 1 September 2027, at $0.35 per metric reference
  a month. Logging's first 50 GB a month per project is free, and starfixd
  at `info` logs little.

Sources: [VM pricing](https://cloud.google.com/products/compute/pricing/general-purpose),
[disks and snapshots](https://cloud.google.com/compute/disks-image-pricing),
[Cloud NAT](https://cloud.google.com/nat/pricing),
[IAP](https://cloud.google.com/iap/pricing),
[Cloud Storage](https://cloud.google.com/storage/pricing),
[Cloud KMS](https://cloud.google.com/kms/pricing),
[observability](https://cloud.google.com/stackdriver/pricing).

## On other clouds

The same shape works anywhere a VM can have no public address. starfix
supports only Google Cloud IAP natively. Elsewhere, put the server behind
a VPN such as WireGuard, and point `host` at its private address.

## What this setup does not give you

- **High availability.** It is still one server. Snapshots and dumps bound
  what an outage can lose, not how long it lasts.
- **Single sign-on.** The group controls the network path, and keys
  control identity. Manage both from your identity and configuration
  tools.
