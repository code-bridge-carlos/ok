# Análisis de Arquitectura — OpenCode + Gentle AI

> Fecha: 2026-09-10
> Repos clonados en `gia/opencode` y `gia/gentle`
> Este documento es la base para el desarrollo del sistema multiagente (s1–s5, w6–w10)

---

## PARTE 1: OPENCODE (`gia/opencode`)

### 1.1 Arquitectura general

**Lenguaje:** TypeScript (NO Go). Monorepo con 32 workspaces, `bun` como package manager, Turbo como orquestador.

**Stack dominante:**
- **Effect** (Context, Layer, Schema, Stream, tagged errors) — programación funcional estructurada
- **Drizzle ORM + SQLite** — persistencia
- **Vercel AI SDK** (`ai`) — loop de LLM
- **SolidJS** — UI (TUI y app web)
- **yargs** — CLI

**Insight clave:** CLI, TUI y app web son **todos clientes HTTP del mismo servidor**. `opencode run` local abre un servidor in-process y habla por fetch con `baseUrl: "http://opencode.internal"`. Todo el producto es cliente-servidor sobre el mismo bus HTTP/SSE/WS. **Esto es la base perfecta para un servicio headless.**

### 1.2 Workspaces principales

| Workspace | Función |
|-----------|---------|
| `packages/opencode` | CLI principal (yargs), sesiones, orquestación del agente |
| `packages/tui` | TUI de terminal (OpenTUI + SolidJS) |
| `packages/app` | App web (SolidJS) que apunta a `localhost:4096` |
| `packages/core` | Servicios base: session, agent, model, provider, filesystem, permission, config |
| `packages/llm` | Cliente LLM (`@opencode-ai/llm`) con providers |
| `packages/server` | Handlers HTTP del servidor |
| `packages/protocol` | Contratos API (grupos de endpoints) |
| `packages/schema` | Esquemas (Session, SessionMessage, etc.) |
| `packages/sdk` | SDK cliente para TS |

### 1.3 TUI

- **Paquete:** `packages/tui` (OpenTUI `@opentui/core` + `@opentui/solid` + SolidJS)
- La TUI corre en un **worker** (`packages/opencode/src/cli/cmd/tui.ts`) con capa RPC; los eventos llegan vía `client.on<GlobalEvent>("global.event")`
- **Render de mensajes** (`routes/session/index.tsx`, 2706 líneas):
  - `PART_MAPPING`: `text → TextPart`, `tool → ToolPart`, `reasoning → ReasoningPart`
  - Header: `▣` + color del agente + modo + modelo + duración
  - `ToolPart` usa `toolDisplay(part.tool)`: componentes para bash, glob, read, grep, webfetch, websearch, write, edit, task, apply_patch, todowrite, question, skill, execute
- **Prompt:** `component/prompt/index.tsx` con autocompletado de comandos slash, historial, stash, paste

### 1.4 Rendering del "thinking"

- **Estado** (`context/thinking.ts`): `ThinkingMode = "show" | "hide"`, persistido en KV (`thinking_mode`)
- **Render** (`ReasoningPart` en `routes/session/index.tsx`):
  - Modo `hide`: colapsado a una línea
  - Filtra `[REDACTED]` (OpenRouter cifra bloques)
  - Mientras no termina: Spinner "Thinking: {title}"
  - Al terminar: "Thought: {title} · {duración}", con color warning
  - Render: bloque code markdown streaming con sintaxis sutil
- **CLI no-interactivo** (`run.ts`): flag `--thinking`, imprime "Thinking: {text}" en dim/italic

### 1.5 Herramientas (tools)

**Registro** (`packages/opencode/src/tool/registry.ts`): PlanExitTool, QuestionTool, ShellTool, EditTool, GlobTool, GrepTool, ReadTool, TaskTool, TodoWriteTool, WebFetchTool, WriteTool, InvalidTool, SkillTool, WebSearchTool, LspTool, Truncate, ApplyPatchTool.

**Contrato** (`tool/tool.ts`): `ToolInvalidArgumentsError` (Schema.TaggedErrorClass con mensaje orientado al modelo) y `Context { sessionID, messageID, agent, abort, callID, extra, messages, metadata(), ask() }`.

**Ciclo de vida** (`ToolState`): `pending → running → completed | error`, con `input`, `structured`, `content`, `result`, `outputPaths`, `attachments`.

**Permisos previos a tools** (`core/src/permission.ts`): servicio `PermissionV2` con `Request`, `AssertInput`, `Reply`; reglas `{permission, action: allow|deny, pattern}` — clave para workers seguros.

### 1.6 Cliente LLM

- **Paquete `packages/llm`** (`@opencode-ai/llm`): `LLMClient`, `Auth`, `Provider`, `generate`/`stream`
- **Providers:** anthropic, openai (+options), openai-compatible (+profile), google, azure, amazon-bedrock, cloudflare, github-copilot, openrouter, xai
- **Orquestación** (`session/llm.ts`): **Vercel AI SDK** (`streamText`, `wrapLanguageModel`) — siempre exactamente un `llm.stream(request)` por turno
- **Prompts de sistema** (`session/prompt/`): `default.txt`, `plan.txt`, `plan-mode.txt`, `build-switch.txt` y variantes por familia (anthropic, gpt, gemini, codex...)
  - `plan.txt` es el **system-reminder de modo plan**: prohíbe ediciones, solo leer/inspeccionar, construir plan con preguntas al usuario

### 1.7 Modelos y agentes

- **`core/src/agent.ts`** (AgentV2): `defaultID = ID.make("build")`; schema: `{ id, model?, request, system?, description?, mode: "subagent"|"primary"|"all", hidden, color?, steps?, permissions }`
- **Carga desde markdown** (`config/agent.ts`): `Glob.scan("{agent,agents}/**/*.md")` para agentes y `{mode,modes}/*.md` para modos (frontmatter + cuerpo como prompt) — **esto es lo que hay que replicar para `plan`/`build`**
- **Selección en runtime:** TUI (dialogs model/agent/variant), CLI (`--agent/--model/--variant`), endpoints `session.switchAgent` y `session.switchModel`

### 1.8 Comandos slash

- **Definición** (`command/index.ts`): `Info { name, description, agent, model, source, template, subtask, hints }`
- Exposición HTTP: `GET /api/command` (handler solo expone `command.list`)
- Consumo: TUI con `useCommandSlashes()`, CLI con `--command <name>`
- Comandos de proyecto en config: tabla `project_commands` (SQLite)

### 1.9 Filesystem

- **Servicio** (`core/src/filesystem.ts`): lectura/escritura/listado/glob/grep con `ReadInput { path: RelativePath }`
- **Exposición HTTP** (`protocol/src/groups/fs.ts`): `GET /api/fs/read/*`, `GET /api/fs/list`, `GET /api/fs/find`
- **Control de acceso:** todo pasa por `PermissionV2`/rulesets (safePath equivalente)

### 1.10 Configuración

- **Archivo:** `opencode.json`/`.jsonc` (jsonc-parser)
- **Esquema** (`core/src/config.ts`): `$schema, shell, model, default_agent, telemetry, share, compact, add_to_gitignore...` y sub-esquemas: ConfigAgent, ConfigCompaction, ConfigCommand, ConfigExperimental, ConfigLSP, ConfigMCP, ConfigPlugin, ConfigProvider, ConfigToolOutput
- **Agentes/modos:** `.opencode/agent/*.md`, `.opencode/modes/*.md`, skills en `.opencode/skill/*.md`, comandos en `core/src/command.ts`
- **Auth:** `OPENCODE_SERVER_PASSWORD` / `OPENCODE_SERVER_USERNAME` (default `opencode`)
- **Env/flags:** módulo Flag `@opencode-ai/core/flag/flag`

### 1.11 Servidor HTTP

**Núcleo** (`packages/opencode/src/server/server.ts`): `Server.listen(opts)` → fallback de puerto `0 → 4096 → libre`; `HttpRouter.serve(HttpApiApp.createRoutes(opts))`.

**Composición de API** (`routes/instance/httpapi/api.ts`):
- `RootHttpApi`: ControlApi + ControlPlaneApi + GlobalApi, middleware `SchemaErrorMiddleware` + `Authorization`
- `InstanceHttpApi`: Config, Experimental, File, Instance, Mcp, Project, ProjectCopy, Pty, Question, Permission, Provider, Session, Sync, Tui, Workspace
- `OpenCodeHttpApi` = Root + EventApi + Instance + ServerApi

**Middlewares:** authorization (Basic auth; `?auth_token=`), schema-error, instance-context (resuelve instancia por directorio vía header `x-opencode-directory`), workspace-routing, cors, compression, proxy, fence, dispose.

**Eventos/streaming:** `GET /api/event` → SSE global (`EventV2.allBounded(events, 256)`, heartbeat cada 15s). `GET /api/session/:id/event` → replay durable + live. WebSocket solo para PTY.

**App web (`packages/app`):** SolidJS. Entry `src/entry.tsx`/`src/index.ts`. Páginas: home, session, new-session, layout. Componente `prompt-input-v2.tsx`.

### 1.12 Persistencia

- **Motor:** SQLite + Drizzle. `core/src/database/`: `sqlite.bun.ts` (Bun) y `sqlite.node.ts` (node), `schema.gen.ts` (generado), ~40 migraciones
- **Modelo de sesión V2** (`core/src/session.ts`): tablas `SessionTable` + `SessionMessageTable`, `SessionStore`, `SessionProjector`, `SessionRunner`, `SessionExecution`
- **Event-sourcing:** `EventV2` con eventos durables `{aggregateID, seq, version}`; `SessionInput` durable en inbox; `session.prompt` admite antes de despertar; `SessionRevert`; `Snapshot`; epoch persistido

### 1.13 Puntos de adaptación para servicio headless (AGENT_MODE=plan|build)

El repo YA está diseñado como servicio headless; la adaptación es **configuración + envoltura**, no reescritura:

1. **Entrada headless:** `opencode serve` → `Server.listen({port, hostname})`; o embebido `Server.Default().app.fetch(req)`
2. **Autoinstancia por directorio:** header `x-opencode-directory` (multi-tenant por proyecto — ideal para workers de cola)
3. **Ciclo de tarea externa:**
   - `POST /api/session` `{agent?, model?, location?}` → `{data: {id, ...}}`
   - `POST /api/session/:id/prompt` con `parts: [{type:"file"|"text", ...}]`
   - Suscribirse a `GET /api/event` (SSE) para `session.status` (idle = fin), `permission.asked`, `session.error`, eventos `message.part.updated` (tool, text, reasoning)
   - `POST /api/session/:id/wait` para bloquear hasta idle
   - `POST /api/session/:id/interrupt` para cancelar
4. **Mapeo AGENT_MODE=plan|build:** modos como archivos `modes/*.md` (frontmatter + prompt); `plan.txt`/`plan-mode.txt` ya son el system-reminder read-only. Definir `.opencode/agent/plan.md` y `build.md` y pasar `--agent plan|build`
5. **Permisos headless:** responder a `permission.asked` (once/reject) o `--dangerously-skip-permissions`; política estricta por sesión con rulesets
6. **Auth:** `OPENCODE_SERVER_PASSWORD`/`OPENCODE_SERVER_USERNAME` + CORS configurable
7. **UI web incluida:** `packages/app` sirve de panel para el servicio
8. **SDK:** `@opencode-ai/sdk`/`v2` (`createOpencodeClient({baseUrl, directory, headers})`)
9. **Reporte de resultado:** recoger parts (text/reasoning/tool) hasta `session.status.idle`
10. **Cuidados:** un solo `llm.stream()` por turno; esperar `time.end` en parts; `session.prompt` admite de forma durable (reintentos seguros)

---

## PARTE 2: GENTLE AI (`gia/gentle`)

### 2.1 Arquitectura general

**Lenguaje:** Go 1.25.10. Módulo `github.com/gentleman-programming/gentle-ai/v2`. CLI+TUI binario único (`gentle-ai`).

**Dependencias clave:** charmbracelet/bubbletea + bubbles + lipgloss (TUI), modernc.org/sqlite (SQLite puro-Go), BurntSushi/toml, go-minisign.

**Paquetes `internal/` (41):** `agents/` (16 adapters), `app/`, `cli/`, `tui/` (77 screens), `components/` (18 subpaquetes: engram, sdd, skills, mcp, permissions, persona, gga, theme, opencodeplugin...), `state/`, `opencode/`, `model/`, `assets/` (embebidos), `installcmd/`, `skillregistry/`, `backup/`, `verify/`, `update/`.

### 2.2 Qué hace

**Ecosistema configurador:** detecta, instala y configura agentes de IA (16 agentes) y componentes del ecosistema. NO es un instalador de agentes: adapta los agentes que ya tienes.

**Comandos:** `install`, `sync`, `upgrade`, `uninstall`, `restore`, `doctor`, `review`, `skill-registry`, `sdd-status`, `sdd-continue`, `version`.

### 2.3 Componentes

#### Engram (memoria persistente)
- **Repo separado:** `github.com/Gentleman-Programming/engram` (binario Go, `cmd/engram`)
- **Instalación:** `installcmd.NewResolver().ResolveComponentInstall(profile, model.ComponentEngram)` con verificación minisign; fallback `go install github.com/Gentleman-Programming/engram/cmd/engram@main`
- **MCP server:** `engram mcp --tools=agent` registrado por agente. Para OpenCode (1.3.3+): `{mcp: {engram: {__replace__: {command: [cmd, "mcp", "--tools=agent"], type: "local"}}}}`
- **Protocolo:** `engram/protocol.md` embebido, floor versión 1.4.0 (canal MCP `instructions` si ≥ 1.4.0)
- **Tools de memoria:** mem_save, mem_search, mem_context, mem_session_summary, mem_session_start, mem_session_end, mem_get_observation, mem_suggest_topic_key, mem_capture_passive, mem_save_prompt, mem_update, mem_current_project, mem_judge, mem_review
- **Sistema de prompts:** inyecta sección `engram-protocol` vía `filemerge.InjectMarkdownSection` (CLAUDE.md, AGENTS.md)

#### SDD (Spec-Driven Development)
- **11 fases:** sdd-init, sdd-explore, sdd-research, sdd-propose, sdd-spec, sdd-design, sdd-tasks, sdd-apply, sdd-verify, sdd-archive (+ sdd-onboard)
- Prompts compartidos en `~/.config/opencode/prompts/sdd/` referenciados por OpenCode vía `{file:...}`
- **Doble sección por capacidad:** cada SKILL.md tiene `<!-- section:model-capable -->` y `<!-- section:model-small -->`; `extractModelSection(content, capability)` elige una — **plan = capable, build = small**
- **Perfiles:** `Profile{Name, OrchestratorModel, PhaseAssignments}` para asignar modelos distintos por fase

#### Skills
- Copia `skills/{id}/SKILL.md` al SkillsDir del adapter atómicamente (`filemerge.WriteFileAtomic`)
- `IsSDDSkill` salta las `sdd-*` (las gestiona el componente SDD)
- 26 skills embebidas en `internal/assets/skills/`

#### MCP / Context7
- Para OpenCode: servidor **remoto** `{"type":"remote","url":"https://mcp.context7.com/mcp","enabled":true}` con `__replace__`
- Estrategias por agente: `StrategyMergeIntoSettings` (OpenCode), `StrategySeparateMCPFiles` (Claude), `StrategyMCPConfigFile` (Cursor), `StrategyTOMLFile` (Codex)

#### Permisos
- Solo Claude Code por ahora; para OpenCode se usan permisos nativos del servicio PermissionV2

#### GGA (Gentleman Guardian Angel)
- Cambiador de proveedores AI; config en `~/.config/gga`

#### Persona
- `gentleman`/`neutral`/`custom`; escribe bloque markdown con marcadores, agente `gentleman` en opencode.json (`mode: primary`, `prompt: {file:./AGENTS.md}`)

### 2.4 Integración con OpenCode

**Adapter** (`internal/agents/opencode/adapter.go`): `Agent() = opencode`, `Tier() = full`.

- **Paths:** `ConfigPath` → `~/.config/opencode/`; `SettingsPath` prefiere `opencode.jsonc`; `SystemPromptFile = AGENTS.md`; `SkillsDir = .../skills`; `CommandsDir = .../commands`
- **Estrategias:** `SystemPromptStrategy = StrategyFileReplace` (AGENTS.md reemplazo entero); `MCPStrategy = StrategyMergeIntoSettings`
- **Efecto en `opencode.json`:** claves `agent.gentle-orchestrator` (SDD), `agent.gentleman` (persona), `mcp.engram`, `mcp.context7`, `commands/`, `skills/`
- Merge comentario-preservante JSON con `__replace__` para reemplazos atómicos

### 2.5 TUI y CLI

- **TUI** (`internal/tui/`, bubbletea): router con `linearRoutes`, 77 screens: welcome, detection, agents, persona, preset, dependency_tree, installing, complete, model pickers, sdd_mode, skill_picker, scope
- Requiere TTY (error explícito para no interactivo)
- **CLI** (`internal/cli/`): `InstallFlags`: `--agent(s)`, `--component(s)`, `--skill(s)`, `--persona`, `--preset`, `--sdd-mode`, `--scope global|workspace`, `--channel stable|beta|nightly`, `--dry-run`

### 2.6 Persistencia

- **`internal/state/state.go`:** `stateDir = ".gentle-ai"`, `stateFile = "state.json"` → `.gentle-ai/state.json`
- `InstallState{InstalledAgents, InstalledBinaryVersion, ManagedAssetDigest, SelectionConfigured, Components, CommunityTools...}`
- **Assignments persistidos:** `ModelAssignmentState{ProviderID, ModelID, Effort}`, perfiles SDD, intenciones background
- **Backups** (`internal/backup/`): manifiestos versionados, restaurables con `restore`
- **Idempotencia:** toda escritura pasa por `filemerge.WriteFileAtomic`; overlays JSON con `__replace__`

### 2.7 Relación con el repo Engram

Gentle AI es el **instalador/configurador del ecosistema**; Engram es su **componente de memoria persistente**:
- Fuente: releases GitHub (verificación minisign) o `go install github.com/Gentleman-Programming/engram/cmd/engram@main`
- Contrato: floor 1.4.0 para canal `instructions`
- Setup: prueba de protocolo con `engram setup --help` (timeout 5s)

### 2.8 Puntos de adaptación orquestador (s2) + workers (w6-w10)

1. **Patrón MCP "orquestador sí, workers no":** inyectar `mcp.engram` solo en agent del orquestador; omitir en workers. Los workers quedan con config MCP base (context7 o nada)
2. **AGENT_MODE=plan|build → secciones capable/small:** `plan = capable` (modelo grande con memoria), `build = small` (workers)
3. **Prompts compartidos por fase con `{file:...}`:** orquestador apunta a prompts de fases, workers a los suyos sin duplicar
4. **Perfiles con asignación de modelos:** `SDDProfileStrategyGeneratedMulti` + `Profile{Name, OrchestratorModel, PhaseAssignments}` = modelos/effort distintos por worker
5. **Protocolo de memoria Engram:** el orquestador debe *empezar* cada sesión con `mem_context`/`mem_session_summary` antes de distribuir trabajo
6. **Persistencia e idempotencia:** `.gentle-ai/state.json` + escritura atómica + overlays `__replace__` como capa de config de servicios
7. **Skill registry delegador:** `.atl/skill-registry.md` con políticas de exclusión (`sdd-`, `_shared`)

---

## PARTE 3: DECISIONES DE ARQUITECTURA PARA EL NUEVO SISTEMA

### 3.1 Tecnologías por servicio

| Servicio | Tecnología | Base |
|----------|-----------|------|
| s1 (Panel) | **Go** | Nuevo desarrollo (requisito: ligero, stateless) |
| s2 (Orquestador) | **TypeScript/Bun** | OpenCode headless (`opencode serve`) + Engram MCP + agent plan |
| s3 (Engram) | **Go** | Binario `engram` (repo Gentleman-Programming/engram) |
| s4 (MCPs) | **Go/Node** | Context7 remoto + MCP GitHub + MCP Render |
| w6-w10 (Workers) | **TypeScript/Bun** | OpenCode headless + agent build, sin memoria |
| BD | — | Supabase (Postgres) |

### 3.2 Servicio headless OpenCode (s2 y workers)

- Ejecutar `opencode serve` (o embebido `Server.Default().app.fetch`)
- Definir agentes `.opencode/agent/plan.md` y `build.md` con rulesets de permisos
  - plan: read-only (heredado de `plan.txt` system-reminder)
  - build: edición permitida, safePath estricto
- Ciclo de tarea: `POST /api/session` → `POST /api/session/:id/prompt` → SSE `GET /api/event` → `session.status.idle` → `POST /api/session/:id/wait`
- Auth: `OPENCODE_SERVER_PASSWORD` + token temporal TUI emitido por s1

### 3.3 Memoria (s2 vs workers)

- **s2 tenerá memoria**: MCP Engram (`engram mcp --tools=agent`) + auto-compact a 170k + descarga de secciones a Supabase
- **Workers sin memoria**: sin MCP Engram; contexto completo llega en cada subtarea; sección visual 50k solo para UI

### 3.4 TUI web

- **Opción A:** adaptar `packages/app` (SolidJS) de OpenCode apuntando al puerto del servicio
- **Opción B:** UI custom con xterm.js (más ligero, panel propio)
- Acceso: token temporal emitido por s1, validado por servicio destino (middleware de auth)

### 3.5 Comunicación

- s1 ↔ servicios: long-polling o Supabase Realtime (NO WebSocket directo — Render lo corta)
- s1 → s2: tarea vía HTTP POST + callback
- s2 → s1: lista de subtareas vía HTTP POST
- s1 → workers: notificación long-poll (no consulta activa cuando idle)
- Workers ↔ Supabase: subtarea + resultado

### 3.6 Proxies

- s1 obtiene proxies de `proxmint/free-proxy-list` vía GitHub API
- Tablas `proxy_pool` + `proxy_assignments` en Supabase
- Asignación automática cada hora a las 00; manual con botón `⋮`
- Inyección vía env (`PROXY_URL`) al servicio

---

## VERIFICACIÓN GRUPO A

- [x] Clon OpenCode en `gia/opencode` (6623 archivos)
- [x] Clon Gentle AI en `gia/gentle` (2236 archivos)
- [x] Análisis de arquitectura OpenCode (TUI, tools, thinking, LLM, agentes, servidor, persistencia)
- [x] Análisis de arquitectura Gentle AI (componentes, Engram, SDD, skills, MCP, integración OpenCode)
- [x] Estructura `desarrollo/` creada (s1-panel, s2-carlos-code, s3-engram, s4-mcps, s5-reservado, workers, docs)

## HALLAZGOS IMPORTANTES

1. **OpenCode NO es Go** — es TypeScript/Bun. El requisito "Go como OpenCode" aplica a s1 (panel), pero s2/workers serán TS (OpenCode headless)
2. **El propio entorno actual ES este sistema**: engram memory + SDD + agent-routing configurado por Gentle AI — el sistema que estamos construyendo replica esta arquitectura a escala de 10 servicios
3. **Engram ya es un servicio Go con MCP server** (`engram mcp --tools=agent`) — s3 es casi directo: correr el binario engram
4. **OpenCode ya tiene UI web incluida** (`packages/app` SolidJS) — candidata para la TUI web
5. **Los modos plan/build ya existen en OpenCode** como archivos markdown con frontmatter + `plan.txt`/`build-switch.txt` prompts — AGENT_MODE mapea directamente a agentes plan.md/build.md