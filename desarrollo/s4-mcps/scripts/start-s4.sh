#!/usr/bin/env bash
set -u; BUN="${BUN:-$HOME/.bun/bin/bun}"; DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; PID_FILE=/tmp/opencode/s4.pid
[ "${1:-}" = "stop" ] && { [ -f "$PID_FILE" ] && kill "$(cat "$PID_FILE")" 2>/dev/null && rm -f "$PID_FILE" && echo "s4 detenido" || echo "s4 no estaba corriendo"; exit 0; }
export S1_URL="${S1_URL:-http://localhost:8080}"; export CONTROL_TOKEN="${CONTROL_TOKEN:-dev-token-b4}"; export ENGRAM_URL="${ENGRAM_URL:-http://127.0.0.1:7437}"
mkdir -p /tmp/opencode; setsid "$BUN" "$DIR/src/index.ts" > /tmp/opencode/s4.log 2>&1 &
echo $! > "$PID_FILE"; sleep 1
kill -0 "$(cat "$PID_FILE")" 2>/dev/null && echo "s4 arrancado: pid $(cat "$PID_FILE") · :9004" || { echo "s4 FALLO"; tail -10 /tmp/opencode/s4.log; rm -f "$PID_FILE"; exit 1; }
