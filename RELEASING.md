# Releasing starfix

A release is a pushed `vMAJOR.MINOR.PATCH` tag. The `release` workflow does
the rest in two jobs:

| Job | Secrets | Does |
|---|---|---|
| `build` | none | Downloads and verifies the modules (`go mod verify`) into a clean cache and runs `govulncheck`, failing on a known vulnerability; builds `sfx` for linux, darwin and windows on amd64 and arm64, and `starfixd` for linux on amd64 and arm64 (`CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X …version.Version=<tag>"`); packs each into `<bin>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) with the LICENSE and README; writes `checksums.txt` (sha256); attests build provenance for every archive; uploads them as a workflow artifact |
| `sign-and-publish` | `STARFIX_RELEASE_KEY`, in the `release` environment | Waits for the maintainer's approval; checks the archives against `checksums.txt`; signs it into `checksums.txt.sig` with `internal/tools/releasekey`; creates the GitHub release with every file |

`checksums.txt.sig` is one line: the standard base64 of a raw Ed25519
signature over the exact bytes of `checksums.txt`. `sfx upgrade` and
`starfixd upgrade` accept a release only when that signature verifies
against a key in `internal/release/keys.go` and the archive's sha256 is in
`checksums.txt`. The signing step refuses a key that `keys.go` at the tag
does not list, so a release clients would refuse is never published.

The release notes name the Dolt version the release was tested with
(`version.Dolt`, which a test keeps equal to CI's `DOLT_VERSION`).

## Cutting a release

When to cut the first one is the maintainer's call.

1. Make sure the commit to tag is on `main` and its CI is green.
2. Tag and push; only the maintainer can (the tag ruleset below):

   ```sh
   git tag -s v0.3.0 -m v0.3.0
   git push origin v0.3.0
   ```

3. When `build` passes, approve the `sign-and-publish` deployment
   (Actions → the run → Review deployments → `release`).
4. Check the release page lists eight archives, `checksums.txt` and
   `checksums.txt.sig`, then on any machine: `sfx upgrade --check`.

A tag with a `-` (`v0.3.0-rc.1`) is published as a prerelease, which
`sfx upgrade` ignores; `starfixd upgrade --to v0.3.0-rc.1` installs it.

If a run fails after the release was created, delete the release (not the
tag) and re-run the workflow.

## One-time setup

The maintainer does these by hand. Replace `<maintainer>` with the
maintainer's GitHub login.

1. **Generate the key** on a trusted machine, from a checkout:

   ```sh
   go run ./internal/tools/releasekey gen release.key
   ```

   It writes the private key to `release.key` (mode 0600) and prints only
   the public key, as the line to add to `keys.go`.

2. **Create the `release` environment**: the maintainer as the only
   required reviewer, self-review allowed (he is the only reviewer), and
   only `v*` tags may deploy to it.

   ```sh
   id=$(gh api users/<maintainer> --jq .id)
   gh api -X PUT repos/ariesworx/starfix/environments/release --input - <<JSON
   {"reviewers": [{"type": "User", "id": $id}],
    "prevent_self_review": false,
    "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}
   JSON
   gh api -X POST repos/ariesworx/starfix/environments/release/deployment-branch-policies \
     -f name='v*' -f type=tag
   ```

3. **Store the key** as an environment secret, keep an offline backup
   (for example on encrypted removable media in a safe), then delete the
   local file:

   ```sh
   gh secret set STARFIX_RELEASE_KEY --env release < release.key
   # back release.key up offline, then:
   shred -u release.key   # rm -P on macOS
   ```

4. **Restrict `v*` tags** to the maintainer with a tag ruleset that blocks
   creating, updating and deleting them for everyone but repository
   admins (actor 5 is the built-in Admin role; keep the maintainer the
   only admin):

   ```sh
   gh api -X POST repos/ariesworx/starfix/rulesets --input - <<'JSON'
   {"name": "release tags", "target": "tag", "enforcement": "active",
    "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
    "rules": [{"type": "creation"}, {"type": "update"}, {"type": "deletion"}],
    "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}]}
   JSON
   ```

   Check under Settings → Rules that the bypass list shows only Repository
   admin.

5. **Publish the public key**: paste the printed line into
   `internal/release/keys.go` by pull request. Until that merges, every
   build refuses every release (`no release signing key is configured`).

### What this protects, and what it does not

The key is usable only by a `release` run that the maintainer approved,
on a `v*` tag that only the maintainer can push. Nobody can read it back
out of GitHub through the UI or API. But GitHub itself, and owners of the
`ariesworx` organization (who can change environments, rulesets and
workflows), can still reach it. That is the accepted trade-off for
signing in CI rather than on the maintainer's machine. Build provenance
(`gh attestation verify`) is an independent check that an archive came
from this repository's workflow.

## Rotating the key

Clients trust whatever `keys.go` held when they were built, so a new key
must ship in a release before anything is signed with it.

1. `go run ./internal/tools/releasekey gen new.key`; add the new line to
   `keys.go` **beside** the old one, by pull request.
2. Release (signed with the old key). Every client that installs it now
   trusts both keys.
3. Replace the secret: `gh secret set STARFIX_RELEASE_KEY --env release < new.key`;
   back up `new.key` offline and delete it locally.
4. Release again (signed with the new key). Clients older than step 2
   refuse this release; they upgrade to the step-2 release first by hand.
5. After a while, remove the old key from `keys.go` by pull request.

If the old key is compromised, skip the overlap: remove it from `keys.go`
in the same pull request that adds the new one, replace the secret, and
release. Every client older than that release refuses it and has to be
reinstalled by hand (README, Install); say so in the release notes.
