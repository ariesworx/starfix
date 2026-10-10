# A local starfix test server: sshd, starfixd and Dolt in one container,
# set up as docs/server.md sets up a machine. For testing only, never for
# production. Build from the repository root, so the image holds whatever
# branch is checked out; docs/deploy/docker.md has the commands.
#
# Behind a TLS-intercepting proxy, pass its CA as a build secret, which is
# never stored in the image: --secret id=ca,src=/path/to/ca.pem

ARG TARGETARCH

# Dolt, at version.Dolt (internal/version; a test keeps the two equal, and
# keeps the amd64 checksum equal to CI's). BuildKit verifies each download.
ARG DOLT_VERSION=2.4.2
ARG DOLT_SHA256_AMD64=5d95a271149dc6ea97f47157af7ba9a0df5e8d4e9bbac4d79c169f2faa3cfb9e
ARG DOLT_SHA256_ARM64=c1c610e22c6bacd4c5b2fbf5697a0f453d759d075c4abff380c6e646553e22d8

FROM scratch AS dolt-amd64
ARG DOLT_VERSION DOLT_SHA256_AMD64
ADD --checksum=sha256:${DOLT_SHA256_AMD64} \
    https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-amd64.tar.gz /dolt.tar.gz

FROM scratch AS dolt-arm64
ARG DOLT_VERSION DOLT_SHA256_ARM64
ADD --checksum=sha256:${DOLT_SHA256_ARM64} \
    https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-arm64.tar.gz /dolt.tar.gz

FROM dolt-${TARGETARCH} AS dolt

# golang:1.27.2-trixie
FROM --platform=$BUILDPLATFORM golang:1.27-trixie@sha256:2f84bc93ecfb2689f782b153fdcd368b5a7ab96c1386c65cdaccf35e726d6a44 AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=ca,required=false \
    if [ -f /run/secrets/ca ]; then export SSL_CERT_FILE=/run/secrets/ca; fi; \
    go mod download && go mod verify
COPY cmd cmd
COPY internal internal
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/ariesworx/starfix/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/starfixd ./cmd/sfx

# ubuntu:24.04.5, the release docs/server.md's steps were tested on
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_*
RUN --mount=from=dolt,target=/mnt/dolt \
    tar -xzf /mnt/dolt/dolt.tar.gz -C /tmp \
 && install -m 0755 /tmp/dolt-linux-*/bin/dolt /usr/local/bin/dolt \
 && rm -rf /tmp/dolt-linux-*
COPY --from=build /out/starfixd /out/sfx /usr/local/bin/

# Every piece of state lives on the /state volume. The paths docs/server.md
# names link into it, so its commands work here unchanged. Fixed ids keep
# the volume's files owned by the same accounts after a rebuild.
RUN groupadd --system --gid 900 dolt \
 && useradd --system --uid 900 --gid dolt --no-create-home --home-dir /var/lib/dolt --shell /usr/sbin/nologin dolt \
 && groupadd --system --gid 901 starfix \
 && useradd --system --uid 901 --gid starfix --create-home --home-dir /var/lib/starfix --shell /bin/sh starfix \
 && usermod -p '*' starfix \
 && install -d -m 0755 /state /etc/dolt \
 && ln -s /state/dolt /var/lib/dolt \
 && ln -s /state/starfix /etc/starfix \
 && ln -s /state/ssh /var/lib/starfix/.ssh
COPY deploy/docker/dolt.yaml /etc/dolt/config.yaml
COPY deploy/docker/sshd.conf /etc/ssh/sshd_config.d/00-starfix.conf
COPY deploy/docker/entrypoint.sh /usr/local/bin/starfix-entrypoint
VOLUME /state
EXPOSE 22
HEALTHCHECK --start-period=60s --start-interval=1s CMD ["test", "-e", "/run/starfix-ready"]
ENTRYPOINT ["/usr/local/bin/starfix-entrypoint"]
