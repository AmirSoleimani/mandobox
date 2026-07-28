# syntax=docker/dockerfile:1
#
# Golden guest image (PLAN §7.2). Built in CI, exported to an ext4 rootfs by image/build.sh.
# fc-supervisor is PID 1 — there is no systemd, sshd, cron, dbus, or cloud-init. Per D1 the
# guest verifies with linters + language unit tests; full e2e is delegated to GitHub CI, so
# no Docker is installed here. Pin every version; a PR that opens under one and resumes under
# another produces behaviour changes mid-review (§7.2).

# --- Stage 1: build fc-supervisor (static) ---
ARG GO_VERSION=1.25.5
FROM golang:${GO_VERSION}-bookworm AS supervisor
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/fc-supervisor ./cmd/fc-supervisor

# --- Stage 2: golden rootfs ---
FROM debian:bookworm-slim AS rootfs
ARG NODE_MAJOR=22
ARG CLAUDE_CODE_VERSION=2.1.220
ARG GH_VERSION=2.96.0
ARG GOLANGCI_LINT_VERSION=2.12.2
ARG GO_VERSION=1.25.5
ENV DEBIAN_FRONTEND=noninteractive

# Tools Claude Code shells out to constantly; their absence degrades output in ways that look
# like model problems (§7.2). fd ships as fdfind on Debian.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq ripgrep fd-find \
      python3 python3-venv openssh-client less procps iproute2 e2fsprogs \
    && ln -sf "$(command -v fdfind)" /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/*

# Node LTS (Claude Code runtime) via NodeSource.
RUN curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Claude Code CLI, pinned.
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && npm cache clean --force

# GitHub CLI, pinned.
RUN curl -fsSL -o /tmp/gh.tgz \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
    && tar -xzf /tmp/gh.tgz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh \
    && rm -rf /tmp/gh*

# Go toolchain + golangci-lint (language build/test/lint).
RUN curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
RUN curl -fsSL -o /tmp/gcl.tgz \
      "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz" \
    && tar -xzf /tmp/gcl.tgz -C /tmp \
    && install -m 0755 "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint" /usr/local/bin/ \
    && rm -rf /tmp/gcl* "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64"

# Python linter/formatter.
RUN python3 -m venv /opt/pytools \
    && /opt/pytools/bin/pip install --no-cache-dir ruff \
    && ln -sf /opt/pytools/bin/ruff /usr/local/bin/ruff

# fc-supervisor as PID 1 (§8.1).
COPY --from=supervisor /out/fc-supervisor /sbin/fc-supervisor
RUN ln -sf /sbin/fc-supervisor /sbin/init

# Module/build caches live on the workspace volume, not the image (§7.2).
ENV GOMODCACHE=/workspace/.cache/go/mod \
    GOCACHE=/workspace/.cache/go/build \
    npm_config_cache=/workspace/.cache/npm

# Record the pinned versions for provenance.
RUN printf 'claude-code=%s\nnode_major=%s\ngh=%s\ngo=%s\ngolangci-lint=%s\n' \
      "${CLAUDE_CODE_VERSION}" "${NODE_MAJOR}" "${GH_VERSION}" "${GO_VERSION}" "${GOLANGCI_LINT_VERSION}" \
      > /etc/fleet-image-versions
