# Carlos Code — Plan de Implementación Completo

## Resumen Ejecutivo

Evolucionar el sistema actual (bridge + web panel distribuido) en **Carlos Code** — un entorno de desarrollo distribuido optimizado para Render free tier con 5+ cuentas, usando DeepSeek vía API key configurable, Engram para memoria, y SDD para planificación.

---

## Tabla de Contenidos

1. [Estado Actual](#1-estado-actual)
2. [Objetivo](#2-objetivo)
3. [Arquitectura Objetivo](#3-arquitectura-objetivo)
4. [Componentes](#4-componentes)
5. [Flujo de Trabajo](#5-flujo-de-trabajo)
6. [Qué tomar de OpenCode](#6-qué-tomar-de-opencode)
7. [Qué tomar de Gentle AI](#7-qué-tomar-de-gentle-ai)
8. [Qué NO tomar](#8-qué-no-tomar)
9. [Skills](#9-skills)
10. [Carlos Free Gateway](#10-carlos-free-gateway)
11. [Distribución de RAM](#11-distribución-de-ram)
12. [Seguridad y API Keys](#12-seguridad-y-api-keys)
13. [Orden de Implementación](#13-orden-de-implementación)
14. [Preguntas Frecuentes](#14-preguntas-frecuentes)

---

## 1. Estado Actual

### Qué funciona hoy
- **Web Panel** en Render (FastAPI): `/login`, `/api/state`, `/api/task/{id}/conversation`
- **Bridge** en codespace local: HTTP polling, envía prompts a Opencode CLI
- **Planner** integrado en bridge: analiza tareas y distribuye a trabajadoras
- **Worker endpoints**: `worker-poll`, `worker-action`, `worker-kick`
- **Persistencia**: Supabase (asyncpg pool) + fallback a `.cai_tasks.json`
- **Conversation API**: historial completo por tarea/subtarea

### Problemas actuales
- Bridge depende de una PC líder encendida
- DeepSeek solo funciona con API key (no web scraping en Render)
- No hay memoria persistente entre sesiones
- No hay planificación estructurada (SDD)

---

## 2. Objetivo

Crear **Carlos Code**: un sistema distribuido donde:
- Un **orquestador** planifica en la nube (Render)
- **Múltiples PCs** ejecutan trabajo localmente
- **DeepSeek** provee inteligencia vía API
- **Engram** guarda memoria entre sesiones
- Todo optimizado para **Render free tier** (512MB RAM por servicio)

---

## 3. Arquitectura Objetivo

```
┌─────────────────────────────────────────────────────────┐
│  CUENTA RENDER 1: WEB PANEL (ya existe)                │
│  FastAPI + estáticos + bridge endpoints                 │
│  RAM: ~300MB                                            │
└────────────────────┬────────────────────────────────────┘
                     │ HTTP
┌────────────────────▼────────────────────────────────────┐
│  CUENTA RENDER 2: CARLOS CODE (orquestador)            │
│  FastAPI service                                        │
│  - Recibe prompt del web panel                          │
│  - Consulta DeepSeek vía gateway                        │
│  - Planifica distribución de trabajo                    │
│  - Devuelve {assignments, files, mode}                  │
│  - Modo default: solo planificar                        │
│  - Flag EDIT_MODE: puede editar archivos                │
│  Skills: sdd-explore, sdd-propose, sdd-spec,           │
│          sdd-tasks, work-unit-commits                   │
│  Write access: sí (para modo edición)                   │
│  RAM: ~400MB                                            │
└────────────────────┬────────────────────────────────────┘
                     │ HTTP
┌────────────────────▼────────────────────────────────────┐
│  CUENTA RENDER 3: ENGRAM MCP                            │
│  Memoria persistente entre sesiones                     │
│  - SQLite local                                        │
│  - Temas: planes, decisiones, convenciones              │
│  - Connecta a Carlos Code via HTTP                      │
│  RAM: ~100MB                                            │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│  CUENTA RENDER 4: CARLOS FREE GATEWAY                   │
│  FastAPI — proxy a DeepSeek API                         │
│  - Endpoint /v1/chat/completions (OpenAI compatible)    │
│  - API key configurable via env o admin panel           │
│  - Health check /health                                 │
│  - Solo DeepSeek (optimizado, sin Chrome)               │
│  RAM: ~150MB                                            │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│  CUENTA RENDER 5: CONTEXT7 + GREP_APP                   │
│  Context7: documentación de librerías                   │
│  grep_app: búsqueda en repos de GitHub                  │
│  Connecta a Carlos Code via HTTP                        │
│  RAM: ~100MB                                            │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│  PCs TRABAJADORAS (Linux/Windows)                       │
│  Bridge local en cada una                               │
│  - Recibe tareas del web panel via HTTP polling         │
│  - Ejecuta código localmente                            │
│  - Reporta progreso al web panel                        │
│  Skills: sdd-apply, go-testing, work-unit-commits       │
│  RAM: local (no depende de Render)                      │
└─────────────────────────────────────────────────────────┘
```

---

## 4. Componentes

### 4.1 Carlos Free Gateway (C4)

**Propósito**: Proxy ligero a DeepSeek API que permite cambiar la API key en cualquier momento.

**Endpoint principal**:
```
POST /v1/chat/completions
  - model: "deepseek-chat"
  - messages: [...]
  - stream: true/false
  - Authorization: Bearer <api_key>
```

**Cambio de API key**:
```
POST /admin/api-key
  - api_key: "nueva-key"
  - admin_token: "token-secreto"
  → Guarda en memoria, reinicia conexión
```

**Health check**:
```
GET /health
  → {status: "ok", provider: "deepseek", latency_ms: 230, api_key_set: true}
```

**Modelos**:
```
GET /v1/models
  → [{id: "deepseek-chat", name: "DeepSeek Chat"}, ...]
```

**Estadísticas**:
```
GET /admin/stats
  → {request_count: 150, error_count: 2, api_key_prefix: "sk-d3f5..."}
```

**Stack**: Python 3.11 + FastAPI + httpx + uvicorn

**Archivo**: `control/gateway/server.py`

### 4.2 Carlos Code — Orquestador (C2)

**Propósito**: Recibe prompts del web panel, planifica usando DeepSeek, devuelve distribución de trabajo.

**Endpoint principal**:
```
POST /plan
  - prompt: "texto del usuario"
  - workers: ["pc-test", "pc-win", ...]
  → {
      "mode": "percent" | "dynamic",
      "note": "resumen del plan",
      "assignments": {"pc-test": ["archivo1.py"], "pc-win": ["archivo2.py"]}
    }
```

**Modos de operación**:
- **Default (plan-only)**: Solo planifica, distribuye a workers
- **EDIT_MODE**: Puede editar archivos directamente (cuando no hay workers conectados)

**Skills de orquestación**:
- `sdd-explore`: Explorar codebase antes de planificar
- `sdd-propose`: Crear propuesta de cambio
- `sdd-spec`: Especificar requisitos
- `sdd-tasks`: Dividir en tareas de implementación
- `work-unit-commits`: Planificar commits por unidad

**Stack**: Python 3.11 + FastAPI + httpx + SQLite + uvicorn

**Archivo**: `control/carlos_code/server.py`

### 4.3 Engram MCP (C3)

**Propósito**: Memoria persistente entre sesiones.

**Qué guarda**:
- Planes de desarrollo
- Decisiones tomadas
- Convenciones del proyecto
- Lo que funcionó / no funcionó

**Stack**: Python + SQLite + FastAPI

**Archivo**: `control/engram_mcp/server.py`

### 4.4 Web Panel (C1) — Ya existe

**Cambios necesarios**:
- Agregar endpoint para enviar prompts a Carlos Code
- UI para ver planes devueltos por Carlos Code
- Toggle para modo EDIT_MODE de Carlos Code
- Mostrar qué workers están activos

### 4.5 Trabajadoras (PCs locales)

**En cada PC**:
- Bridge (ya existe en `control/worker/bridge.js`)
- Skills instalados según proyecto
- Conexión al web panel via HTTP polling

---

## 5. Flujo de Trabajo

```
1. Tú escribes prompt en Web Panel
2. Web Panel → POST a Carlos Code (C2)
3. Carlos Code → POST a Gateway (C4) → DeepSeek API
4. DeepSeek responde con análisis
5. Carlos Code planifica distribución
6. Carlos Code devuelve al Web Panel:
   {
     "mode": "percent",
     "note": "Agrego test unitarios para src/auth.py y src/api/routes.py",
     "assignments": {
       "pc-test": ["src/auth.py", "tests/test_auth.py"],
       "pc-win": ["src/api/routes.py", "tests/test_routes.py"]
     }
   }
7. Web Panel distribuye a PCs trabajadoras vía bridge
8. Trabajadoras ejecutan → reportan al Web Panel
9. Si falla → Web Panel pide re-planificación a Carlos Code
10. Cuando todo termina → Web Panel notifica al usuario
```

---

## 6. Qué tomar de OpenCode

| Qué tomar | Para qué | Archivo fuente |
|-----------|----------|----------------|
| Framework de agentes | Orquestación de Carlos Code | `agent/` |
| Tool calling framework | Ejecución de herramientas | `tool/` |
| Modelos gratuitos Zen API | Fallback si DeepSeek falla | `provider/zen/` |
| Streaming de respuestas | Respuestas en tiempo real | `provider/` |
| Config de modelos por agente | Personalizar comportamiento | `opencode.json` |

---

## 7. Qué tomar de Gentle AI

| Qué tomar | Para qué | Archivo fuente |
|-----------|----------|----------------|
| Engram | Memoria persistente | `engram/` |
| SDD (explore→propose→spec→tasks) | Planificación estructurada | `skills/sdd-*` |
| Skills | Sistema de habilidades | `skills/` |
| Work-unit commits | Commits por unidad | `skills/work-unit-commits/` |

---

## 8. Qué NO tomar

| Qué NO tomar | Por qué |
|--------------|---------|
| Chrome/CDP | No cabe en Render free (512MB RAM) |
| TUI de OpenCode | Innecesario, todo es vía web |
| Reglas de pago | Solo uso gratuito |
| Plugins innecesarios | Solo los esenciales |
| Receipt-driven development | No aplica para este caso |
| Review authority | No aplica |

---

## 9. Skills

### Skills de Carlos Code (orquestador)
1. `sdd-explore` — Explorar codebase antes de planificar
2. `sdd-propose` — Crear propuesta de cambio
3. `sdd-spec` — Especificar requisitos
4. `sdd-tasks` — Dividir en tareas de implementación
5. `work-unit-commits` — Planificar commits por unidad

### Skills de Trabajadoras (PCs)
- `sdd-apply` — Implementar tareas
- Skills específicos del proyecto (go-testing, etc.)
- `work-unit-commits` — Commits por unidad

---

## 10. Carlos Free Gateway — Detalles

### Inspiración: Token Free Gateway
- Endpoint OpenAI-compatible: ✅
- Auto-refresh tokens: ✅
- Soporte para key rotation: ✅
- Health check: ✅

### Simplificaciones vs Token Free Gateway
- Sin Chrome/CDP: usa API key directa
- Solo DeepSeek: no 13 providers
- Sin webauth wizard: configuración via env vars
- ~200MB RAM vs ~1GB

### Configuración de DeepSeek API key

**Cambiar vía env var** (antes de deploy):
```
DEEPSEEK_API_KEY=sk-nueva-key
```

**Cambiar vía admin endpoint** (en caliente):
```bash
curl -X POST https://gateway.onrender.com/admin/api-key \
  -H "Content-Type: application/json" \
  -d '{"api_key": "sk-nueva-key", "admin_token": "tu-token"}'
```

**Cambiar vía panel de control** (futuro):
- Botón "Cambiar API key" en el panel
- Input field + botón guardar

---

## 11. Distribución de RAM

| Servicio | Cuenta | RAM estimada | Notas |
|----------|--------|-------------|-------|
| Web Panel | C1 | ~300MB | FastAPI + estáticos |
| Bridge worker | C1 (local) | 0 | Corre en la PC |
| Carlos Code | C2 | ~400MB | FastAPI + planificación |
| Engram MCP | C3 | ~100MB | SQLite, ligero |
| Gateway | C4 | ~150MB | FastAPI + HTTP client |
| Context7 + grep_app | C5 | ~100MB | Cache de docs |
| **Total** | **5 cuentas** | **~1.05GB** | **Dentro de 5x512MB free** |

---

## 12. Seguridad y API Keys

### API keys que necesitas

| Servicio | Key | Dónde va |
|----------|-----|----------|
| DeepSeek | `sk-d3f5...dc4b` | Gateway (C4) |
| Supabase | Connection string | Web Panel (C1) |
| Render API | Token de deploy | Bridge (local) |
| GitHub | Token de repo | Bridge (local) |

### Reglas de seguridad
- **NUNCA** commitear API keys
- Usar env vars en Render
- Admin token para cambiar keys en caliente
- Gateway valida admin_token antes de aceptar cambios

---

## 13. Orden de Implementación

1. **Carlos Free Gateway** (C4) — primero, para que Carlos Code tenga DeepSeek
2. **Carlos Code** (C2) — orquestador con conexión al gateway
3. **Engram MCP** (C3) — memoria persistente
4. **Context7 + grep_app** (C5) — herramientas de conocimiento
5. **Integración con Web Panel** — conectar Carlos Code al panel existente
6. **Trabajadoras** — configurar bridge en cada PC con skills

---

## 14. Preguntas Frecuentes

### ¿Por qué Render free y no otra cosa?
- El usuario tiene 5+ cuentas Render free
- Ya tiene el web panel funcionando en Render
- Free tier = 512MB RAM por servicio, suficiente para cada componente

### ¿Por qué DeepSeek API y no web scraping?
- Web scraping requiere Chrome/CDP que no cabe en Render free
- API key es más estable y rápido
- El usuario ya tiene API key de DeepSeek

### ¿Qué pasa si se acaba la RAM?
- Los servicios se separan en cuentas diferentes
- Si un servicio se cae, los demás siguen funcionando
- Engram y los MCPs son muy ligeros (~100MB cada uno)

### ¿Cómo cambio la API key de DeepSeek?
- **En caliente**: `POST /admin/api-key` en el gateway
- **En deploy**: cambiar env var `DEEPSEEK_API_KEY` en Render

### ¿Carlos Code puede editar archivos?
- **Default**: No, solo planifica
- **EDIT_MODE**: Sí, cuando no hay workers conectados
- Se activa con flag `EDIT_MODE=true` en env vars

### ¿Qué pasa si DeepSeek se cae?
- Gateway reporta error en `/health`
- Carlos Code puede usar fallback a OpenCode Zen API (modelos gratuitos)
- Web Panel notifica al usuario

---

## Archivos Relevantes

- `control/gateway/server.py` — Gateway principal
- `control/gateway/requirements.txt` — Dependencias del gateway
- `control/gateway/Dockerfile` — Docker para deploy
- `control/carlos_code/server.py` — Orquestador (por crear)
- `control/engram_mcp/server.py` — Engram MCP (por crear)
- `control/server/server.py` — Web Panel existente
- `control/worker/bridge.js` — Bridge existente
- `setup/CARLOS_CODE_PLAN.md` — Este documento

---

*Documento generado automáticamente — Carlos Code Planner*
*Última actualización: 2026-08-30*
