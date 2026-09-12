# Migración C2 y C3 a Go - Resumen

## Estado de la Migración

| Componente | Estado | Lenguaje | Notas |
|------------|--------|----------|-------|
| **C1 - Control Server** | ✅ Sin cambios | Python | Panel web principal |
| **C2 - Gateway** | ✅ **Migrado a Go** | Go | Proxy a DeepSeek sin API Key |
| **C3 - Carlos Code** | ✅ **Migrado a Go** | Go | Orquestador distribuido |
| **C4 - Engram MCP** | ✅ Sin cambios | Python | Memoria persistente |
| **C5 - Context7+grep** | ✅ Sin cambios | Python | Documentación y búsqueda |

## Cambios Realizados

### C2 Gateway (`control/gateway/`)

**Antes:** Python FastAPI con httpx
**Ahora:** Go con chromedp para browser automation

- Endpoint `/v1/chat/completions` compatible con OpenAI
- Endpoint `/health` para health check
- Endpoint `/v1/models` para listar modelos
- Fallback a API key si no hay credenciales de browser
- Binario Linux compilado: `carlos-gateway-linux` (15MB)

**Variables de entorno:**
- `PORT` (default: 8001)
- `TOKEN_FREE_GATEWAY_HEADLESS` (default: true)
- `TOKEN_FREE_GATEWAY_AUTH_PROFILE` (default: deepseek)
- `DEEPSEEK_API_KEY` (fallback si no hay credenciales)

### C3 Carlos Code (`control/carlos_code/`)

**Antes:** Python FastAPI con agent loop complejo
**Ahora:** Go simple con endpoints esenciales

- Endpoint `/health` para health check
- Endpoint `/plan` para planificación de tareas (contrato intacto)
- Endpoint `/tools` para listar herramientas
- MCP client para conectar a Engram y Context7
- Binario Linux compilado: `carlos-code-linux` (8.6MB)

**Variables de entorno:**
- `PORT` (default: 8002)
- `GATEWAY_URL` (obligatorio)
- `ENGRAM_URL` (opcional)
- `CONTEXT7_URL` (opcional)
- `EDIT_MODE` (default: false)
- `PROJECT_PATH` (default: /workspaces/cai)

## Contrato /plan (sin cambios)

El contrato `/plan` se mantiene idéntico al original:

**Entrada:**
```json
{
  "prompt": "string",
  "workers": ["pc1", "pc2"],
  "existing_files": ["src/main.py"],
  "project_path": "/workspaces/cai"
}
```

**Salida:**
```json
{
  "mode": "percent",
  "note": "string",
  "assignments": {
    "pc1": ["archivo1", "archivo2"]
  }
}
```

## Compilación

### Linux (Render)
```bash
# C2 Gateway
cd control/gateway
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o carlos-gateway-linux .

# C3 Carlos Code
cd control/carlos_code
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o carlos-code-linux .
```

### Windows (local)
```bash
# C2 Gateway
cd control\gateway
GOOS=windows GOARCH=amd64 go build -o carlos-gateway.exe .

# C3 Carlos Code
cd control\carlos_code
GOOS=windows GOARCH=amd64 go build -o carlos-code.exe .
```

## Configuración de Render

El `render.yaml` ha sido actualizado con todos los servicios:

- **C1**: Python (cai-control)
- **C2**: Go (carlos-gateway)
- **C3**: Go (carlos-code)
- **C4**: Python (engram-mcp)
- **C5**: Python (context7-grep)

## Próximos Pasos

1. **Autenticar DeepSeek**: Ejecutar `actualizar_credenciales.bat` en Windows
2. **Deploy en Render**: Los servicios se desplegarán automáticamente desde GitHub
3. **Probar flujo completo**: Panel → C3 (/plan) → C2 (Gateway) → DeepSeek

## Notas Importantes

- Los binarios Go están compilados estáticamente (CGO_ENABLED=0)
- El Gateway usa chromedp para browser automation (requiere Chrome en el sistema)
- Si Chrome no está disponible, usa fallback a DEEPSEEK_API_KEY
- C4 y C5 se mantienen en Python (no se tocaron)
- El contrato `/plan` no cambió (compatibilidad con el orquestador principal)
