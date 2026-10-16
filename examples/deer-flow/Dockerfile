# syntax=docker/dockerfile:1
#
# All-in-one lab image: OpenSandbox server + DeerFlow backend.
#
#   - OpenSandbox server (pip package, runtime.type = "docker")
#   - DeerFlow backend (installed from source with the `opensandbox` extra,
#     which ships OpenSandboxProvider)
#   - Python 3.12 + uv, Docker CLI (client only), common debugging tools
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

# ---- System and debugging tools ----
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git vim nano less htop procps psmisc lsof strace \
      iputils-ping net-tools dnsutils jq openssh-client build-essential tzdata \
    && rm -rf /var/lib/apt/lists/*

# ---- Docker CLI: inspect sandbox containers on the host (no daemon) ----
COPY --from=docker:cli /usr/local/bin/docker /usr/local/bin/docker

# ---- uv: the package manager used by the DeerFlow checkout ----
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# ---- OpenSandbox: server + Python SDK + osb CLI ----
RUN pip install opensandbox-server opensandbox opensandbox-cli

# ---- DeerFlow backend (2.1.0+ ships OpenSandboxProvider) ----
RUN git clone --depth 1 --branch "${DEERFLOW_REF}" "${DEERFLOW_REPO}" /opt/deer-flow \
    && cd /opt/deer-flow/backend \
    && uv sync --all-packages --extra opensandbox
ENV PATH="/opt/deer-flow/backend/.venv/bin:${PATH}"
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

EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s \
  CMD curl -sf http://127.0.0.1:8080/health

ENTRYPOINT ["entrypoint.sh"]
