#!/usr/bin/env bash
# Arranca/detiene s3-engram (puerto 9003, upstream engram serve en 7437).
set -u
BUN="${BUN:-$HOME/.bun/bin/bun}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_FILE=/tmp/opencode/s3.pid

if [ "${1:-}" = "stop" ]; then
  [ -f "$PID_FILE" ] && kill "$(cat "$PID_FILE")" 2>/dev/null && rm -f "$PID_FILE" && echo "s3 detenido" || echo "s3 no estaba corriendo"; exit 0
fi

export DATABASE_URL="${DATABASE_URL:-postgres://postgres:test@localhost:5433/testdb?sslmode=disable}"
export S1_URL="${S1_URL:-http://localhost:8080}"
export CONTROL_TOKEN="${CONTROL_TOKEN:-dev-token-b4}"
export ENGRAM_URL="${ENGRAM_URL:-http://127.0.0.1:7437}"

mkdir -p /tmp/opencode
setsid "$BUN" "$DIR/src/index.ts" > /tmp/opencode/s3.log 2>&1 &
echo $! > "$PID_FILE"
sleep 1
if kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  echo "s3 arrancado: pid $(cat "$PID_FILE") · :9003 · log /tmp/opencode/s3.log"
else
  echo "s3 FALLO" && tail -20 /tmp/opencode/s3.log; rm -f "$PID_FILE"; exit 1
fi
