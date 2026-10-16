# syntax=docker/dockerfile:1
#
# All-in-one lab image: OpenSandbox server + DeerFlow (backend + Web UI).
#
#   - OpenSandbox server (pip package, runtime.type = "docker")
#   - DeerFlow backend (installed from source with the `opensandbox` extra,
#     which ships OpenSandboxProvider)
#   - DeerFlow frontend: pre-built (pnpm install + next build), served by the
#     bundled nginx on :2026 — the single entry point for the Web UI
#   - Python 3.12 + uv, Node.js 22 + corepack/pnpm, Docker CLI (client only),
#     common debugging tools
#   - The OpenSandbox checkout itself (model-free example script + docs)
#
# Sandbox containers run on the HOST Docker daemon: mount docker.sock when
# running this image and the server drives the host daemon through it
# (same topology as server/docker-compose.example.yaml).

# ---- Overridable for network-restricted environments ----
ARG DEERFLOW_REPO=https://github.com/bytedance/deer-flow.git
ARG DEERFLOW_REF=main
ARG OPENSANDBOX_REPO=https://github.com/opensandbox-group/OpenSandbox.git

FROM python:3.12-slim

ARG DEERFLOW_REPO
ARG DEERFLOW_REF
ARG OPENSANDBOX_REPO

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# ---- System and debugging tools (nginx + make serve the DeerFlow Web UI) ----
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git vim nano less htop procps psmisc lsof strace \
      iputils-ping net-tools dnsutils jq openssh-client build-essential tzdata \
      make nginx \
    && rm -rf /var/lib/apt/lists/*

# ---- Docker CLI: inspect sandbox containers on the host (no daemon) ----
COPY --from=docker:cli /usr/local/bin/docker /usr/local/bin/docker

# ---- uv + Node.js (corepack resolves pnpm from DeerFlow's packageManager) ----
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv
COPY --from=node:22-bookworm-slim /usr/local /usr/local
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable

# ---- OpenSandbox: server + Python SDK + osb CLI ----
RUN pip install opensandbox-server opensandbox opensandbox-cli

# ---- DeerFlow: backend (with the opensandbox extra) + pre-built frontend ----
# UV_EXTRAS keeps the extra across DeerFlow's own `uv sync --locked` runs
# (scripts/serve.sh re-syncs on every start); without it the provider import
# disappears after the first restart.
RUN git clone --depth 1 --branch "${DEERFLOW_REF}" "${DEERFLOW_REPO}" /opt/deer-flow \
    && cd /opt/deer-flow/backend \
    && uv sync --all-packages --extra opensandbox \
    && cd ../frontend \
    && pnpm install --frozen-lockfile \
    && pnpm run build
ENV PATH="/opt/deer-flow/backend/.venv/bin:${PATH}" \
    UV_EXTRAS=opensandbox
WORKDIR /opt/deer-flow

# ---- OpenSandbox checkout: the model-free example script + example docs ----
RUN git clone --depth 1 "${OPENSANDBOX_REPO}" /opt/OpenSandbox

# ---- Server config (container topology: mounted socket + host-gateway) ----
COPY opensandbox-server.toml /etc/opensandbox/config.toml
ENV SANDBOX_CONFIG_PATH=/etc/opensandbox/config.toml

# ---- Helper scripts ----
COPY enable-opensandbox /usr/local/bin/enable-opensandbox
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/enable-opensandbox /usr/local/bin/entrypoint.sh

# 8080: OpenSandbox server API; 2026: DeerFlow Web UI (nginx entry point).
EXPOSE 8080 2026
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s \
  CMD curl -sf http://127.0.0.1:8080/health

ENTRYPOINT ["entrypoint.sh"]
