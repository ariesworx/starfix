# Installing starfix

starfix has two binaries. Everyone who uses starfix installs `sfx`; only a
server needs `starfixd`.

| Binary | Runs on | Installed by |
|---|---|---|
| `sfx` | Linux, macOS and Windows, on amd64 and arm64 | each person, on each machine where they or their agents work |
| `starfixd` | Linux, on amd64 and arm64 | the server's admin ([Running a server](server.md#3-install-starfixd)) |

## Install sfx

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sh
```

The script installs the latest `sfx` into `~/.local/bin` and prints the
line to add if that directory is not on your `PATH`. It never uses sudo or
edits your shell files. Before it installs anything, it checks:

- the release's Ed25519 signature, with OpenSSL 3 (without it, the script
  warns and checks only the sha256);
- the archive's sha256.

Options go after `sh -s --`, for example
`curl -fsSL … | sh -s -- --version v0.2.0`:

| Option | Variable | Does |
|---|---|---|
| `--version vX.Y.Z` | `STARFIX_VERSION` | Install that release instead of the latest |
| `--dir DIR` | `STARFIX_DIR` | Install into DIR instead of `~/.local/bin` |
| `--server` | | Install `starfixd` instead (Linux only) |
| `--require-signature` | | Fail, rather than warn, when OpenSSL cannot check the signature |

On Windows, download the zip from the
[releases page](https://github.com/ariesworx/starfix/releases) and put
`sfx.exe` on your `PATH`, or run the script inside WSL.

## Download by hand

Each [release](https://github.com/ariesworx/starfix/releases) carries:

- `sfx` for Linux, macOS and Windows, and `starfixd` for Linux, on amd64
  and arm64, as `<binary>_<version>_<os>_<arch>.tar.gz` (`.zip` on
  Windows);
- `checksums.txt`, the sha256 of every archive;
- `checksums.txt.sig`, the Ed25519 signature of `checksums.txt`.

Unpack the binary onto your `PATH`, after you
[verify](#verify-a-download-by-hand) the download.

## Build from source

```sh
go install github.com/ariesworx/starfix/cmd/sfx@latest
go install github.com/ariesworx/starfix/cmd/starfixd@latest
```

Each builds the same code, unsigned, into `$(go env GOBIN)` or
`$(go env GOPATH)/bin`. Under a version manager such as mise, that is the
manager's own directory, which changes with the Go version. Prefer the
script. On a server, put `starfixd` where
[step 3](server.md#3-install-starfixd) installs it.

## Verify a download by hand

Work from a checkout of the release's tag, so the public key comes from the
repository rather than from the download. For release v0.2.0 on Linux:

```sh
git clone --branch v0.2.0 https://github.com/ariesworx/starfix
cd starfix
gh release download v0.2.0 -p checksums.txt -p checksums.txt.sig -p 'sfx_0.2.0_linux_amd64.tar.gz'
go run ./internal/tools/releasekey verify checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify sfx_0.2.0_linux_amd64.tar.gz --repo ariesworx/starfix
```

| Command | Checks |
|---|---|
| `releasekey verify` | The signature on `checksums.txt`, against the keys in `internal/release/keys.go` |
| `sha256sum -c` | The archive against `checksums.txt` (on macOS, `shasum -a 256 -c`) |
| `gh attestation verify` | GitHub's build provenance: the archive was built by this repository's release workflow |

Releases are signed in CI with a key that only the maintainer's approved
release runs can use. [RELEASING.md](../RELEASING.md) has the details and
the trade-offs.

## Upgrade and roll back

| Command | Does |
|---|---|
| `sfx upgrade --check` | Print one line: your version, the latest, and whether to upgrade. Changes nothing |
| `sfx upgrade` | Replace `sfx` with the latest release, after checking its signature and checksum |
| `sfx upgrade --rollback` | Restore the binary the last upgrade replaced |

`sfx upgrade` never runs by itself. When the server runs a newer version
than your `sfx`, each command prints a one-line notice, and `prime`, which
starts an agent's session, includes it. To upgrade the server, see
[Upgrades](server.md#upgrades).

## Uninstall

1. In each repository you set up, run `sfx setup --all --remove`. Add
   `--global` if you set agents up for every repository, and run
   `sfx setup claude-desktop --remove` if you registered Claude Desktop.
2. Delete `sfx`, and the `sfx.prev` that an upgrade leaves beside it, from
   `~/.local/bin` or wherever you installed it.
