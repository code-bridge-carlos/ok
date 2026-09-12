# Carlos Code (C3)

Orquestador distribuido para el sistema Carlos AI. Planifica y distribuye tareas entre workers.

## Endpoints

- `GET /health` - Health check
- `POST /plan` - Planificar distribución de tareas
- `GET /tools` - Listar herramientas disponibles

## Variables de Entorno

- `PORT` - Puerto del servidor (default: 8002)
- `GATEWAY_URL` - URL del Gateway (C2)
- `ENGRAM_URL` - URL de Engram MCP (C4)
- `CONTEXT7_URL` - URL de Context7 + grep_app (C5)
- `EDIT_MODE` - Modo edición (default: false)
- `PROJECT_PATH` - Ruta del proyecto (default: /workspaces/cai)

## Compilación

```bash
# Linux (Render)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o carlos-code-linux .

# Windows
GOOS=windows GOARCH=amd64 go build -o carlos-code.exe .
```

## Uso

```bash
# Ejecutar
./carlos-code-linux

# Con configuración
GATEWAY_URL=https://carlos-gateway.onrender.com ./carlos-code-linux
```

## Contrato /plan

Entrada:
```json
{
  "prompt": "string",
  "workers": ["pc1", "pc2"],
  "existing_files": ["src/main.py"],
  "project_path": "/workspaces/cai"
}
```

Salida:
```json
{
  "mode": "percent",
  "note": "string",
  "assignments": {
    "pc1": ["archivo1", "archivo2"]
  }
}
```
