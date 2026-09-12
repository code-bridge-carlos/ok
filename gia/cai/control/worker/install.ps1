# Instalador del bridge para Windows (PC controlada, recien reinstalada).
# Orden: 1) Node.js (runtime)  2) BRIDGE (primero como componente)  3) Git/OpenCode (opcionales, despues).
# Uso (PowerShell como Administrador):
#   .\install.ps1 -Host 192.168.1.10 -Port 8000 -Token mitoken -Git -OpenCode
param(
  [string]$Host = "",
  [string]$Port = "8000",
  [string]$Token = "",
  [switch]$Git,
  [switch]$OpenCode
)

if (-not $Host -or -not $Token) {
  Write-Host "Uso: .\install.ps1 -Host <ip-del-panel> -Port 8000 -Token <token> [-Git] [-OpenCode]"
  exit 1
}

Write-Host "==> 1) Node.js (runtime)"
if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
  if (Get-Command winget -ErrorAction SilentlyContinue) {
    winget install -e --id OpenJS.NodeJS.LTS --silent
  } elseif (Get-Command choco -ErrorAction SilentlyContinue) {
    choco install nodejs -y
  } else {
    Write-Host "No se pudo instalar Node automaticamente. Instalalo desde nodejs.org y volve a correr este script."
    exit 1
  }
  # recargar PATH en esta sesion
  $env:Path = [System.Environment]::GetEnvironmentVariable("Path","Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path","User")
} else {
  Write-Host "Node ya presente: $(node -v)"
}

Write-Host "==> 2) BRIDGE (primero como componente)"
npm install
npm link
bridge config set serverUrl "ws://$Host`:$Port/ws"
bridge config set token "$Token"
bridge autostart enable
bridge config set autoLaunchOpencode $true
bridge status
Write-Host "Bridge conectado a ws://$Host`:$Port/ws"

Write-Host "==> 3) Git / OpenCode (opcionales, despues del bridge)"
if ($Git) {
  Write-Host "Instalando Git..."
  if (Get-Command winget -ErrorAction SilentlyContinue) { winget install -e --id Git.Git --silent }
  elseif (Get-Command choco -ErrorAction SilentlyContinue) { choco install git -y }
}
if ($OpenCode) {
  Write-Host "Instalando OpenCode (opcional)..."
  npm install -g opencode 2>$null; if ($LASTEXITCODE -ne 0) { Write-Host "OpenCode no se pudo instalar auto; hacelo manual." }
}

Write-Host "==> Listo. El bridge arrancara solo al prender la PC. Para encenderlo ya: bridge"
