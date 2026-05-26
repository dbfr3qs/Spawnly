#!/bin/sh
set -e

# Run Go bootstrap binary — registers with SPIRE/registry/IS, prints agentId to stdout
AGENT_ID=$(weather-bootstrap)
if [ -z "$AGENT_ID" ]; then
  echo "Bootstrap failed: no agentId returned" >&2
  exit 1
fi
export AGENT_ID
echo "[start] Agent ID: $AGENT_ID"

# Start heartbeat loop in background
node /app/heartbeat.mjs &

# Start Flue server (foreground) — HOST=0.0.0.0 ensures it binds all interfaces, not just loopback
export HOST=0.0.0.0
exec node /app/dist/server.mjs
