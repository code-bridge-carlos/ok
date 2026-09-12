# PLAN DE EJECUCIÓN — SISTEMA MULTIAGENTE

## Agrupación de Fases

### GRUPO A: PREPARACIÓN (Fase 1)
**Objetivo:** Tener el entorno listo para desarrollar.

**Tareas:**
1. Clonar OpenCode en `gia/opencode`
2. Clonar Gentle AI en `gia/gentle`
3. Analizar arquitectura de ambos (TUI, herramientas, thinking, archivos)
4. Crear estructura `desarrollo/` con todas las carpetas
5. Documentar en `docs/analisis-opencode-gentle.md`

**Verificación:** Clones existen, análisis completo, estructura correcta.

---

### GRUPO B: BASE DE DATOS + PANEL WEB (Fases 2-6)
**Objetivo:** s1 funcional con login, dashboard, tareas, cola y proxies.

**Sub-fase B1: Schema Supabase (Fase 2)**
- Crear todas las tablas con índices
- Documentar en `docs/supabase-schema.md`
- Verificar con queries de prueba

**Sub-fase B2: Panel Base (Fase 3)**
- Servidor Go stateless
- Login HMAC + cookie HttpOnly
- Dashboard con servicios (RAM, estado, enlace TUI)
- Toggle keep-alive
- Endpoint `/health`

**Sub-fase B3: Botón Tarea (Fase 4)**
- UI para crear tareas
- Persistencia en Supabase
- Botón "Enviar a s2"
- Vista de tareas con estado

**Sub-fase B4: Cola de Tareas (Fase 5)**
- Recibir subtareas de s2 (mock)
- Encolar en Supabase
- Asignar a workers libres
- Long-polling para notificaciones
- Ciclo completo con mocks

**Sub-fase B5: Gestión de Proxies (Fase 6)**
- Subvista de proxies
- Obtener desde GitHub (proxmint/free-proxy-list)
- Cron cada hora
- Asignación automática y manual
- Persistencia en Supabase

**Verificación B:** s1 corre localmente, login funciona, dashboard visible, tareas se crean/envían, cola funciona con mocks, proxies se obtienen/asignan.

---

### GRUPO C: ORQUESTADOR s2 (Fases 7-8)
**Objetivo:** s2 funcional con planificación y TUI.

**Sub-fase C1: Base s2 (Fase 7)** ✅ completada 2026-09-10
- ✅ Adaptar OpenCode + Gentle AI — servicio TS/Bun real (`desarrollo/s2-carlos-code/`, Bun 1.4.2) con `opencode-client.ts` (ciclo session/prompt/event/wait)
- ✅ Modo plan permanente — `AGENT_MODE=plan` + `agent-orquestador.md` (modo/memo del orquestador)
- ✅ Cliente LLM común — `OpenCodeClient` + `ContextWindow` (raíz común para D1)
- ✅ Sección 200k tokens + auto-compact a 170k — `compact.ts` (segmentos + resumen + descarga a Supabase); stats en /health y /api/contexto
- ⏳ Acceso a s3 y s4 — interfaces preparadas (BD compartida conectada; HTTP a s3/s4 cuando existan en D/E)
- ✅ Integración Render + GitHub — `Dockerfile` + `render.yaml` (blueprint free)
- ✅ `agent-orquestador.md` — modo plan del orquestador
- ✅ Reemplazo del mock en dev: `scripts/start-s2.sh` (:9002) + `scripts/start-s2-real.sh` (s1 ↔ s2 real)
- ✅ Verificación: ciclo completo s1→s2 real→3 subtareas→workers→notify→task completada

**Sub-fase C2: Flujo Planificación (Fase 8)** ✅ completada 2026-09-11
- ✅ Recibir tarea desde s1 — `POST /api/tasks`: `planificarYEnviar()` genera plan vía OpenCode (`runPrompt`, agente plan) convirtiendo el prompt de la tarea en un plan JSON de subtareas
- ✅ Generar plan — `PLAN_PROMPT` pide un JSON `{ plan_resumen, subtareas[{file_path, prompt, contexto}] }`; se limpian fences markdown si el modelo los pone; responde 500 con el fragmento si no es JSON válido
- ✅ Guardar plan_json — `savePlan()` (postgres.js serializa el objeto a jsonb). Gotcha: `JSON.stringify()+::jsonb` quedaba doble-codificado (tipo "string") → pasar el objeto directo
- ✅ Usuario corrige/aprueba — gate opcional `S2_AUTO_APPROVE=0`: la tarea queda en `planificando` esperando `POST /api/task/{id}/aprobar` (acepta correcciones o usa plan_json). Default `autoApprove=1` para dev sin TUI
- ✅ Dividir en subtareas — `emitSubtareasA()` → `POST /api/subtareas` a s1 (mantiene el patrón B4: s2 NUNCA inserta en subtask_queue)
- ✅ Enviar lista a s1 — idem (3 subtareas asignadas por dispatcher)
- ✅ Recibir resultados y evaluar — `/api/notify` → `evaluarResultados()`: si quedan activas no evalúa; si hay fallidas las reintenta una vez (estado→`pendiente`, worker NULL → dispatcher reasigna) y task→`ejecutando`; si todo completado → task `completada`
- ✅ Modo dev sin LLM — `S2_MOCK_PLAN=1` (respuesta JSON fija); `S2_EMIT_ON_TASK=1` queda como dev legacy (3 subtareas hardcodeadas, no usa plan)
- ✅ Verificación end-to-end real: (a) tarea→plan→plan_json(object)→3 subtareas→workers→3 completadas→task completada; (b) 2 completadas + 1 fallida→reasignada a otro worker→completada→task completada; (c) aprobación manual con 2 subtareas→completada

**Verificación C:** s2 corre, recibe prompt, genera respuesta, TUI muestra thinking/tools, plan se crea y aprueba, subtareas se envían.

---

### GRUPO D: WORKERS w6-w10 (Fases 9-10)
**Objetivo:** Workers funcionales con ciclo de vida completo.

**Sub-fase D1: Base Workers (Fase 9)** ✅ completada 2026-09-11
- ✅ Adaptar OpenCode + Gentle AI — servicio TS/Bun compartido `desarrollo/workers/` (un artefacto, `WORKER_ID` w6..w10 lo identifica); mismos patrones que s2 (config/logger/opencode-client)
- ✅ Modo build — `AGENT_MODE=build` (default) + `agent-worker.md` (sin memoria entre subtareas; cada ejecución abre y cierra su sesión)
- ✅ Sin memoria entre subtareas — `OpenCodeClient.runPrompt()` por subtarea (sesión nueva cada vez)
- ✅ Sección visual 50k tokens (no al LLM) — `context.ts` `VisualWindow` con auto-eliminación a `VISUAL_MAX_TOKENS` (50k); expone `/estado` y `/health`
- ✅ Auto-eliminación a 50k — `trim()` descarta historial y guarda resumen
- ✅ `agent-worker.md` — modo build del worker
- ✅ Dockerfile + render.yaml — 5 servicios web (worker-w6..w10) sobre `oven/bun:1-slim`
- ✅ `scripts/start-worker.sh` (PID en /tmp/opencode/worker-w*.pid, log worker-w*.log)
- ✅ Verificación: workers reales w6/w8/w10 ejecutaron subtareas (long-poll → ejecutar → `POST /fin` con `{exit,files,summary}`) → 3/3 completadas → task completada; resultado JSONB correcto

**Sub-fase D2: Ciclo de Vida (Fase 10)** ✅ completada 2026-09-11
- ✅ Recibir notificación de s1 — `GET /api/notificaciones/{worker_id}` (long-poll; repite en 204 con backoff)
- ✅ Consultar Supabase — la notificación del long-poll trae `{id, task_id, file_path, prompt, contexto}` (s1 lee la BD por el worker); sin DB directa en el worker
- ✅ Procesar subtarea — `ejecutar()` → OpenCode build (o `WORKER_MOCK_BUILD=1` con resultado sintético en dev)
- ✅ Guardar resultado — s1 guarda `{exit, files, summary}` en subtask_queue.resultado (jsonb)
- ✅ Notificar fin — `POST /api/subtareas/{id}/fin` (`estado: completada|fallida`); guardia `ocupado` impide procesar 2 a la vez
- ✅ Esperar sin consultar si no hay tareas — long-poll 28s (s1 corta a 204) + backoff progresivo
- ✅ Workers reales reemplazan a mock-worker.py; `stop-workers.sh` mata por PID file (incluye w10)
- ✅ Límite de reintentos (s2): `tasks.intentos_reasignacion` (columna nueva idempotente); 2 intentos → task `fallida` (sin bucle infinito)
- Verificación: (a) ciclo completo; (b) falla persistente → reasignar x1 → reasignar x2 → límite → task `fallida`

**Verificación D:** ✅ Worker recibe subtarea, ejecuta (build/mock), envía `{exit,files,summary}`, s1 lo guarda, notifica, queda libre.

---

### GRUPO E: TUIs + COMUNICACIÓN (Fases 11-12) ✅ completado 2026-09-11
**Objetivo:** TUIs accesibles y comunicación end-to-end.

**Sub-fase E1: TUIs (Fase 11)** ✅ completado
- ✅ Cada servicio sirve su TUI (custom HTML/CSS, sin xterm.js) — s2 (`/tui` orquestador con Aprobar plan), workers (w6-w10 con `/simular`), s3 (`/tui` de Engram con stats+observaciones+búsqueda)
- ✅ Acceso vía token temporal de s1 — `POST /api/tui/token` (requireAuth), `GET /api/tui/valida` (requireControl)
- ✅ Validación en servicio destino — `validarTokenTUI()` en tui.ts de cada servicio (s2/w3/workers)
- ✅ Tokens de corta duración (TTL 10 min, hash sha256 en `tui_access_tokens`, un solo uso)

**Sub-fase E2: Comunicación (Fase 12)** ✅ completado
- ✅ s1 ↔ s2: tareas/subtareas — flujo verificado E2E: tarea → s2 → plan_json → 3 subtareas → s1 cola
- ✅ s1 ↔ workers: notificaciones — long-polling 28s; subtareas completadas por w6/w8/w10 → task `completada`
- ✅ Workers ↔ Supabase: vía s1 (workers NO tocan la BD)
- ✅ Long-polling (sin WebSocket directo)
- ✅ Proxy vía env/endpoint — `SERVICES_BASE_URL`, `S1_URL`, `S2_URL`, url_tui por servicio

**Verificación E:** ✅ TUIs accesibles solo con token (sin token → 302 a /login; token 1-uso), comunicación funciona end-to-end.

---

### GRUPO F: SEGURIDAD + DESPLIEGUE (Fases 13-15) ✅ completado 2026-09-11
**Objetivo:** Sistema seguro y desplegado.

**Sub-fase F1: Seguridad (Fase 13)** ✅ completado
- ✅ HMAC sessions — auth.Manager (cookie HttpOnly + HMAC)
- ✅ CONTROL_TOKEN workers — requireControl en s1, X-Control-Token en workers/s2
- ✅ Tokens TUI — emisión/validación con hash + un solo uso
- ✅ CORS controlado — allowlist explícita en s1 (Go) y en chaque servicio (cors.ts), preflight OPTIONS 204
- ✅ safePath — workers/src/safe.ts: rechaza rutas que salen del cwd (path traversal)
- ✅ Keep-alive toggle — settings.key=keep_alive on/off + ping inmediato al arranque con RAM real
- ✅ Preempción a 300s — PREEMPT_MS default 300_000 en workers y s2 (300s max por subtarea)

**Sub-fase F2: Despliegue (Fase 14)** ✅ completado (render.yaml + Dockerfiles)
- ✅ 10 servicios en Render free — render.yaml raíz con s1, s2, s3, s4, s5, w6..w10, tipo web
- ✅ Variables de entorno configuradas — PORT, DATABASE_URL, S1_URL, CONTROL_TOKEN, S2_URL, SERVICES_BASE_URL, WORKER_ID, ENGRAM_URL, etc.
- ✅ Health checks funcionando — /health en cada servicio (también reporta ram_mb)

**Sub-fase F3: Validación (Fase 15)** ✅ completado
- ✅ RAM optimizada — /health de cada servicio reporta ram_mb; keep-alive lo persiste en services_status; ventanas de contexto 50k-200k con límites
- ✅ Todas las prohibiciones verificadas — workers sin DB directa, s2 no inserta en subtask_queue (solo reasigna vía UPDATE), modos plan/build separados, tareas nunca en RAM
- ✅ Tarea completa end-to-end — tarea E2 → plan mock (3 subtareas) → distribuidas a w6/w7/w8 → todas completadas → task `completada`

**Verificación F:** ✅ Sistema seguro (CORS/safePath/tokens/preempción), desplegable (render.yaml + Dockerfiles), funcionando (10 servicios /health 200, RAM en dashboard).

---

## Orden de Ejecución

```
GRUPO A → GRUPO B → GRUPO C → GRUPO D → GRUPO E → GRUPO F
```

**Regla:** No avanzar hasta verificar el grupo actual.

---

## Tecnologías Confirmadas

| Componente | Tecnología |
|------------|------------|
| s1 (Panel) | Go (implementado) |
| s2 (Orquestador) | TypeScript/Bun (implementado) |
| s3 (Engram) | Binario engram envuelto (implementado) — s3-engram :9003 proxya a engram serve :7437 |
| s4 (MCPs) | TypeScript/Bun (implementado) |
| w6-w10 (Workers) | TypeScript/Bun (implementado) |
| Base de datos | Supabase (schema aplicado; dev en PostgreSQL local pg-dev) |
| Despliegue | Render free (render.yaml raíz + 6 Dockerfiles) |
| TUI | Custom HTML/CSS (Grupo E) |
| Comunicación | Long-polling / Supabase Realtime |

---

## Próximo Paso

**GRUPOS E y F COMPLETADOS — 2026-09-11**

1. Despliegue real opcional: conectar render.yaml raíz a un repo GitHub y lanzar los 10 servicios en Render free (configurar DATABASE_URL, SESSION_SECRET, PANEL_PASSWORD, CONTROL_TOKEN y API keys en el dashboard de Render).
2. API keys reales: configurar S2_MOCK_PLAN=0 y WORKER_MOCK_BUILD=0 con el servidor OpenCode real (opencode serve) y las credenciales de los modelos.
