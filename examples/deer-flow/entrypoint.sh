#!/usr/bin/env bash
# Container entrypoint: point DeerFlow at OpenSandbox, start the Web UI
# stack in the background, then run the OpenSandbox server in the foreground.
set -euo pipefail

echo "=============================================================="
echo " OpenSandbox x DeerFlow all-in-one image"
echo "   DeerFlow Web UI : http://localhost:2026   (nginx entry point)"
echo "   OpenSandbox API : http://localhost:8080   (runtime=docker)"
echo "   DeerFlow source : /opt/deer-flow          (backend venv on PATH)"
echo "   OpenSandbox checkout: /opt/OpenSandbox"
echo "   Commands:"
echo "     enable-opensandbox   switch config.yaml back to OpenSandboxProvider"
echo "     make stop            stop the Web UI stack (gateway/frontend/nginx)"
echo "     deerflow --print \"...\"  one headless agent turn (model required)"
echo "     make setup           (in /opt/deer-flow) configure models via wizard"
echo "=============================================================="

# Generate config.yaml on first boot: copy the example, switch the provider.
if [ ! -f /opt/deer-flow/config.yaml ]; then
  enable-opensandbox
fi

# Nginx is the only externally bound entry (gateway/frontend stay on
# loopback), so it must listen on all interfaces for port mapping to work.
export BIND_HOST=0.0.0.0

echo "[image] Starting DeerFlow Web UI stack (prod mode, background)..."
# --skip-install: dependencies are baked at build time (uv sync would drop
#   the opensandbox extra if UV_EXTRAS were unset; it is set in the image).
# --skip-frontend-build: frontend/.next is pre-built; reuse via next start.
bash scripts/serve.sh --prod --daemon --skip-install --skip-frontend-build

echo "[image] Starting OpenSandbox server on :8080 (sandboxes run on the host Docker)"
exec opensandbox-server
