#!/usr/bin/env bash
# Instalador del bridge (PC controlada, Windows o Linux recien reinstalada).
# Orden: 1) Node.js (runtime)  2) BRIDGE (primero como componente)  3) Git/OpenCode (opcionales, despues).
# Uso:  ./install.sh <host-o-ip-del-panel> <puerto> <token> [--git] [--opencode]
set -e

if [ "$#" -lt 3 ]; then
  echo "Uso: $0 <host-o-ip-del-panel> <puerto> <token> [--git] [--opencode]"
  echo "Ejemplo: $0 192.168.1.10 8000 mitoken --git --opencode"
  exit 1
fi
HOST="$1"; PORT="$2"; TOKEN="$3"
WANT_GIT=0; WANT_OC=0
for a in "$@"; do case "$a" in --git) WANT_GIT=1;; --opencode) WANT_OC=1;; esac; done

echo "==> 1) Node.js (runtime)"
if ! command -v node >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    curl -fsSL https://deb.nodesource.com/setup_20.x | sudo bash -
    sudo apt-get install -y nodejs
  elif command -v dnf >/dev/null 2>&1; then
    sudo dnf install -y nodejs npm
  elif command -v pacman >/dev/null 2>&1; then
    sudo pacman -S --noconfirm nodejs npm
  else
    echo "No se pudo instalar Node automaticamente. Instalalo y volve a correr este script."; exit 1
  fi
else
  echo "Node ya presente: $(node -v)"
fi

echo "==> 2) BRIDGE (primero como componente)"
npm install
npm link
bridge config set serverUrl "ws://$HOST:$PORT/ws"
bridge config set token "$TOKEN"
bridge autostart enable
bridge config set autoLaunchOpencode true
bridge status
echo "Bridge conectado a ws://$HOST:$PORT/ws"

echo "==> 3) Git / OpenCode (opcionales, despues del bridge)"
if [ "$WANT_GIT" = "1" ]; then
  echo "Instalando Git..."
  sudo apt-get install -y git 2>/dev/null || sudo dnf install -y git 2>/dev/null || sudo pacman -S --noconfirm git 2>/dev/null || echo "Git no se pudo instalar auto; hacelo manual."
fi
if [ "$WANT_OC" = "1" ]; then
  echo "Instalando OpenCode (opcional)..."
  npm install -g opencode 2>/dev/null || echo "OpenCode no se pudo instalar auto; hacelo manual desde su documentacion."
fi

echo "==> Listo. El bridge arrancara solo al prender la PC. Para encenderlo ya: bridge"
