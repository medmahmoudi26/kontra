# The clean laptop of the README demo (DM4-13, DM4-15, DM4-31; the M4 plan's gate run B2): Go 1.26.10 (the golang
# image, pinned by digest) and Node 22 (the official tarball, its sha256 pinned), a user `laptop` with an empty home,
# and nothing else: no kontra, no a2a-cli, no npm package, no credential. scripts/demo-clean-laptop.sh builds it from
# `git archive HEAD` with the docker driver's builder and runs the README's block in it; test/demo checks what it holds.
#
#   target laptop      the laptop, for the run after the commit is on GitHub (the README's go install, unchanged);
#   target laptop-src  the same with the repository at /src (root's, read-only for laptop), for the run before it is,
#                      when the README's `go install kontra.sh/kontra/cmd/kontra@latest` becomes
#                      `(cd /src && go install ./cmd/kontra)` (the script's one substitution of a command).
#
# GOPATH is the default ($HOME/go) and its bin is not on PATH, as on a laptop with a fresh Go install: the README's
# `export PATH` line is needed and exercised.

FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS laptop

ARG NODE_VERSION=22.23.4
ARG NODE_SHA256_AMD64=b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406
ARG NODE_SHA256_ARM64=f7a1e486dbc8b286847815b477d4fb5e9fb963ed4489c3e1588853d685f589f9
RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
      amd64) arch=x64; sum="$NODE_SHA256_AMD64" ;; \
      arm64) arch=arm64; sum="$NODE_SHA256_ARM64" ;; \
      *) echo "test/demo/Dockerfile: no Node pin for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --proto '=https' -o /tmp/node.tar.gz \
      "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.gz"; \
    echo "${sum}  /tmp/node.tar.gz" | sha256sum -c -; \
    mkdir -p /usr/local/node; \
    tar -xzf /tmp/node.tar.gz -C /usr/local/node --strip-components=1 --no-same-owner; \
    rm -f /tmp/node.tar.gz; \
    useradd --create-home --shell /bin/bash --uid 1000 --user-group laptop

# The golang image's GOPATH (/go, world-writable, on PATH) is not a laptop's.
ENV GOPATH= \
    PATH=/usr/local/node/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
USER laptop
WORKDIR /home/laptop

FROM laptop AS laptop-src
USER root
COPY . /src
USER laptop
