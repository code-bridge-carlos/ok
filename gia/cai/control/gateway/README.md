# Carlos Gateway (C2)

Gateway proxy a DeepSeek sin API Key. Usa Chrome CDP para hacer requests via browser.

## Endpoints

- `GET /health` - Health check (`{"auth": bool}`)
- `GET /v1/models` - Lista modelos disponibles
- `POST /v1/chat/completions` - Chat completions (formato OpenAI)
- `POST /api/auth` - Cargar credenciales DeepSeek frescas `{"bearer": "...", "cookies": "..."}` (rechaza bearer vacío)
- `GET/POST /c2/ws` - WebSocket del runner (registro + auth, contrato heredado)
- `/proxy/*` - Gestión de proxies (status, change, refresh, ui, logs)

## Variables de Entorno

- `PORT` - Puerto del servidor (default: 8001)
- `TOKEN_FREE_GATEWAY_HEADLESS` - Modo headless (default: true)
- `TOKEN_FREE_GATEWAY_AUTH_PROFILE` - Perfil de autenticación (default: deepseek)
- `DEEPSEEK_API_KEY` - API key directa (fallback si no hay credenciales de browser)

## Compilación

```bash
# Linux (Render)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o carlos-gateway-linux .

# Windows
GOOS=windows GOARCH=amd64 go build -o carlos-gateway.exe .
```

## Uso

```bash
# Ejecutar
./carlos-gateway-linux

# Con credenciales
TOKEN_FREE_GATEWAY_HEADLESS=true ./carlos-gateway-linux
```

## Autenticación

El gateway usa credenciales de browser (bearer + cookies) para acceder a DeepSeek sin API key.
El auth se guarda **en memoria** (`authStore`) y se pierde con cada redeploy (Render no da disco persistente en free; hay persistencia efímera a `/tmp/auth-store.json` que solo sobrevive reinicios del proceso dentro de la misma instancia).

DeepSeek bloquea IPs de datacenter (WAF anti-bot), así que la captura de credenciales frescas
solo funciona desde una IP residencial (la máquina del usuario).

### Cargar credenciales frescas

```bash
# 1. Desde la máquina del usuario (IP residencial, navegador logueado en chat.deepseek.com):
#    - correr el runner: node runner.js start (baja desde /install.js; setup.bat en Windows)
#    - o usar la extensión de Chrome (captura tras login en chat.deepseek.com)

# 2. Una vez obtenidas las credenciales, reactivar el gateway desde cualquier lado:
curl -X POST https://carlos-gateway.onrender.com/api/auth \
  -H "Content-Type: application/json" \
  -d '{"bearer": "<token>", "cookies": "<cookies>"}'

# 3. Verificar
curl https://carlos-gateway.onrender.com/health   # → {"auth": true}

# Alternativa local: node credenciales/seed-gateway-auth.js
# (lee auth-profiles.json local; usa HTTP y cae a WS en gateways antiguos)
```
