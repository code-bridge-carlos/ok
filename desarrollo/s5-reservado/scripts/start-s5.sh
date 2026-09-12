#!/usr/bin/env bash
set -u; BUN="${BUN:-$HOME/.bun/bin/bun}"; DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"; PID_FILE=/tmp/opencode/s5.pid
[ "${1:-}" = "stop" ] && { [ -f "$PID_FILE" ] && kill "$(cat "$PID_FILE")" 2>/dev/null && rm -f "$PID_FILE" && echo "s5 detenido" || echo "s5 no estaba corriendo"; exit 0; }
export S1_URL="${S1_URL:-http://localhost:8080}"; export CONTROL_TOKEN="${CONTROL_TOKEN:-dev-token-b4}"
mkdir -p /tmp/opencode; setsid "$BUN" "$DIR/src/index.ts" > /tmp/opencode/s5.log 2>&1 &
echo $! > "$PID_FILE"; sleep 1
kill -0 "$(cat "$PID_FILE")" 2>/dev/null && echo "s5 arrancado: pid $(cat "$PID_FILE") · :9005" || { echo "s5 FALLO"; tail -10 /tmp/opencode/s5.log; rm -f "$PID_FILE"; exit 1; }
