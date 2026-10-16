#!/usr/bin/env bash
# Container entrypoint: point DeerFlow at OpenSandbox, then run the server.
set -euo pipefail

echo "=============================================================="
echo " OpenSandbox x DeerFlow all-in-one image"
echo "   Server    : http://localhost:8080  (runtime=docker)"
echo "   DeerFlow  : /opt/deer-flow         (backend venv on PATH)"
echo "   OpenSandbox checkout: /opt/OpenSandbox"
echo "   Commands:"
echo "     enable-opensandbox     switch config.yaml back to OpenSandboxProvider"
echo "     deerflow --print \"...\" one headless agent turn (model required)"
echo "     make setup             (in /opt/deer-flow) configure models"
echo "=============================================================="

# Generate config.yaml on first boot: copy the example, switch the provider.
if [ ! -f /opt/deer-flow/config.yaml ]; then
  enable-opensandbox
fi

echo "[image] Starting OpenSandbox server on :8080 (sandboxes run on the host Docker)"
exec opensandbox-server
