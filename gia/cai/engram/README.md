# Engram — Memoria del Proyecto CAI

Este directorio contiene un backup de todas las memorias de Engram para el proyecto CAI.

## Cómo funciona

- `observations.json` — todas las observaciones exportadas desde Engram
- Se actualiza cada vez que el orquestador modifique memorias en Engram
- El orquestador debe hacer `commit + push` después de cada cambio significativo

## Observaciones (23)

| ID | Tipo | Título | Tema |
|----|------|--------|------|
| 2 | decision | Login de sesion en el panel del control server | auth/panel-login |
| 3 | decision | Persistencia del server en Supabase via DATABASE_URL | architecture/supabase-persistence |
| 4 | decision | Panel de control desplegado en Render via API (LIVE) | deploy/render-control-panel |
| 5 | architecture | Agrupación de tareas OpenCode: líder + subset + serialización por PC | architecture/opencode-distributed-grouping |
| 6 | discovery | Render proxy mata WebSocket: worker y panel usan HTTP polling | architecture/render-websocket-proxy |
| 7 | architecture | Preempción por idle + dashboard multi-vista en control | control/preemption-dashboard |
| 8 | architecture | control: bridge modo interactivo (tmux) para sesion OpenCode ya abierta | architecture/control-dashboard |
| 9 | architecture | Panel control: botones tarea/subtarea + historial | control/panel-task-actions |
| 10 | bugfix | Root cause: opencode run se colgaba en stdin (planner lento/bug) | control/bridge-opencode-hang |
| 11 | bugfix | Root cause: tareas del historial se pierden (solo-memoria en Render) | control/task-persistence |
| 12 | discovery | Bloqueo: DATABASE_URL de Supabase no es alcanzable desde Render | control/supabase-unreachable |
| 13 | discovery | Supabase DB host unreachable — proyecto pausado | control/supabase-unreachable |
| 14 | architecture | Modo porcentaje: reparto de tareas por PC | control/percent-mode |
| 15 | config | Supabase conectado vía pooler (Render) | config/supabase-pooler |
| 16 | discovery | Planner end-to-end OK en pc-test; prompt vacío falla por diseño | discovery/planner-e2e |
| 17 | bugfix | Fix deploy crash: _db_retry_loop NameError | architecture/cai-deploy |
| 18 | discovery | Bridge needs setsid to survive in codespace | architecture/cai-bridge |
| 19 | architecture | Plan definitivo: Bridge → Carlos Code (actualizado) | architecture/carlos-code-plan |
| 20 | architecture | Carlos Free Gateway creado y testeado | architecture/carlos-code-gateway |
| 21 | architecture | Carlos Code orquestador creado e integrado | architecture/carlos-code-orchestrator |
| 22 | architecture | Engram MCP + Context7/grep_app + Setup trabajadoras | architecture/carlos-code-complete |
| 23 | architecture | Carlos Code reconstruido como sistema real de agentes | architecture/carlos-code-real |
| 24 | architecture | Conectado Engram agente al proyecto CAI + skill/MCP Render instalados | architecture/engram-agent-connection |
## Última actualización

2026-09-05 — Agente conectado al Engram de CAI: 22 observaciones históricas importadas + observación 23 (conexión del agente, skill/MCP Render instalados).
