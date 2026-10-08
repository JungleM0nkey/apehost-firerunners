# Runner VM image (#9): upstream's ubuntu24.04 runner image plus
#   - our fireactions agent (per-pool env, #8, lives in the agent),
#   - the current actions/runner release (rebuilt within 30 days of each release),
#   - Node, so jobs don't silently run Node tooling under Bun.
# Build context is the repository root:
#   docker build -f images/ubuntu24.04/Dockerfile --build-arg RUNNER_VERSION=... --build-arg RUNNER_SHA256=... \
#     --build-arg NODE_VERSION=... .
ARG BASE_IMAGE=ghcr.io/hostinger/fireactions-images/ubuntu24.04:20260901114140

FROM golang:1.26 AS agent
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/hostinger/fireactions.Version=${VERSION} -X github.com/hostinger/fireactions.Commit=${COMMIT}" \
      -o /out/fireactions ./cmd/fireactions

FROM ${BASE_IMAGE}

# actions/runner: replace the base image's (older) runner with the requested release.
ARG RUNNER_VERSION
ARG RUNNER_SHA256
RUN test -n "$RUNNER_VERSION" && test -n "$RUNNER_SHA256" \
    && cd /tmp \
    && curl -fsSLo runner.tar.gz "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \
    && echo "${RUNNER_SHA256}  runner.tar.gz" | sha256sum -c - \
    && rm -rf /opt/runner && mkdir -p /opt/runner \
    && tar xzf runner.tar.gz -C /opt/runner && rm runner.tar.gz \
    && chown -R runner:docker /opt/runner \
    && test "$(/opt/runner/bin/Runner.Listener --version)" = "$RUNNER_VERSION"

# Node from the official release, checksum-verified.
ARG NODE_VERSION
RUN test -n "$NODE_VERSION" \
    && cd /tmp \
    && curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz" \
    && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" | grep " node-v${NODE_VERSION}-linux-x64.tar.gz\$" | sha256sum -c - \
    && tar xzf "node-v${NODE_VERSION}-linux-x64.tar.gz" -C /usr/local --strip-components=1 --no-same-owner \
       --exclude CHANGELOG.md --exclude LICENSE --exclude README.md \
    && rm "node-v${NODE_VERSION}-linux-x64.tar.gz" \
    && test "$(node --version)" = "v${NODE_VERSION}" && npm --version

COPY --from=agent /out/fireactions /usr/bin/fireactions

LABEL org.opencontainers.image.source="https://github.com/JungleM0nkey/apehost-firerunners" \
      org.opencontainers.image.description="Firecracker runner VM image for apehost-firerunners"
