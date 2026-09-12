#!/bin/bash
# setup-worker.sh — Script de setup para trabajadoras de Carlos Code
#
# Uso:
#   ./setup-worker.sh pc-linux1
#   ./setup-worker.sh pc-win1 /ruta/al/cai

set -e

WORKER_ID="${1:-pc-$(hostname)}"
CAI_REPO="${2:-$(pwd)}"
PANEL_URL="${3:-https://bridgecarlos.onrender.com}"
CONTROL_TOKEN="${4:-rnd_rmDmQ4gb7yGMGLnTVhYrDTWG0NgR}"

echo "=========================================="
echo "  Setup Trabajadora — Carlos Code"
echo "=========================================="
echo ""
echo "Worker ID:    $WORKER_ID"
echo "Repo CAI:     $CAI_REPO"
echo "Panel URL:    $PANEL_URL"
echo ""

# Verificar Node.js
if ! command -v node &> /dev/null; then
    echo "❌ Node.js no encontrado. Instalar Node.js 18+ primero."
    exit 1
fi
echo "✅ Node.js: $(node --version)"

# Verificar Git
if ! command -v git &> /dev/null; then
    echo "❌ Git no encontrado. Instalar Git primero."
    exit 1
fi
echo "✅ Git: $(git --version)"

# Verificar que el repo existe
if [ ! -d "$CAI_REPO" ]; then
    echo "❌ Directorio del repo no encontrado: $CAI_REPO"
    exit 1
fi
echo "✅ Repo encontrado"

# Verificar bridge.js
if [ ! -f "$CAI_REPO/control/worker/bridge.js" ]; then
    echo "❌ bridge.js no encontrado en $CAI_REPO/control/worker/"
    exit 1
fi
echo "✅ bridge.js encontrado"

# Crear directorio de configuración
mkdir -p ~/.bridge

# Crear configuración
cat > ~/.bridge/config.json << EOF
{
  "workerId": "$WORKER_ID",
  "panelUrl": "$PANEL_URL",
  "controlToken": "$CONTROL_TOKEN",
  "gitRepo": "$CAI_REPO",
  "gitPush": true
}
EOF
echo "✅ Configuración guardada en ~/.bridge/config.json"

# Instalar dependencias del bridge
cd "$CAI_REPO/control/worker"
if [ -f "package.json" ]; then
    npm install --silent 2>/dev/null || echo "⚠️  npm install falló (puede que no haya package.json)"
fi
echo "✅ Dependencias instaladas"

# Verificar conexión al panel
echo ""
echo "Verificando conexión al panel..."
if curl -s --max-time 5 "$PANEL_URL/health" > /dev/null 2>&1; then
    echo "✅ Panel accesible"
else
    echo "⚠️  Panel no accesible (puede estar iniciándose)"
fi

echo ""
echo "=========================================="
echo "  Setup completado!"
echo "=========================================="
echo ""
echo "Para iniciar el bridge:"
echo "  cd $CAI_REPO/control/worker"
echo "  setsid node bridge.js &"
echo ""
echo "Para verificar en el panel:"
echo "  $PANEL_URL"
echo "  Login: cmpf / Carlos1*"
echo ""
