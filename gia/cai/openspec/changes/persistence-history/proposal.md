# Proposal: persistence-history

## Intent

Make the CAI distributed system fully durable: everything the control panel (C1) keeps in RAM survives redeploys by mirroring to Supabase with boot recovery; add DeepSeek and Carlos Code history views with retention and manual delete; make the keepalive toggle reliable (no more silent state loss, no reload needed); complete the services view (add C6 + Supabase, per-service URL buttons, ping all every 5 min); and converge on ONE logical Engram for the whole ecosystem — local agent Engram (`~/.engram`, project `cai`) ⟷ GitHub backup (`cai/engram/observations.json`, repo `1000carlospena-prog/cai`) ⟷ the Render Engram MCP service backed by Supabase — with the leader and workers logging every completed task into it. Client-facing services connect to the Render Engram service; Supabase is the durable store so it survives redeploys; the agent keeps its backup contract (commit + push `observations.json`).

## Context

### Persistence today (split model)

| State | Where | Survives redeploy? |
|---|---|---|
| workers, worker_memory, principal_id, mode, current_task | C1 RAM (`server.py:70-99`) | No |
| KEEPALIVE_STATE | C1 RAM (`server.py:60-64`) + `keepalive_state` row | Partial (PG only) |
| tasks/subtasks | Supabase + `.cai_tasks.json` file fallback (`db.py:31-50, 186-212`) | Yes (PG) / No (file on new deploy) |
| gateway auth | RAM + `/tmp` file + `gateway_auth` PG row (`gateway/main.go:63-159`) | Yes (PG) |
| Engram observations | sqlite3 (`engram_mcp/server.py:45-60`) + `cai/engram/observations.json` (23 rows) | No (sqlite is image-local) |
| WebSocket / poll connections | RAM only (`server.py:72, 79`) | N/A — inherently ephemeral |

### The 6 live services (canonical numbering: `static/js/services.js:3-53`, `KEEPALIVE_SERVICES` `server.py:53-59`, `config.yaml`)

| ID | Component | URL |
|---|---|---|
| C1 | Panel Web — `control/server` (FastAPI) | https://bridgecarlos.onrender.com |
| C2 | Gateway DeepSeek — `control/gateway` (Go) | https://carlos-gateway.onrender.com |
| C3 | Carlos Code — `control/carlos_code` (Go) | https://carlos-code.onrender.com |
| C4 | Engram MCP — `control/engram_mcp` (FastAPI) | https://engram-mcp-q9ln.onrender.com |
| C5 | Context7+grep — `control/context7_grep` (FastAPI) | https://context7-grep.onrender.com |
| C6 | Worker Code — `control/worker_code` (Go) | https://worker-code.onrender.com |

Supabase project: `qqaocynurdhtmmxktzcz` (pooler `aws-0-us-east-2.pooler.supabase.com:6543`, `ssl=require`, `statement_cache_size=0` — proven params from `db.py:96-106`).

### Naming note (C-numbering)

Some handoff lines label Engram MCP as "C3", gateway as "C4", carlos_code as "C2". That matches the **original pre-deploy plan numbering**; the **deployed code** (services.js, KEEPALIVE_SERVICES, config.yaml, and requirement-5's own service list) uses C2=gateway, C3=carlos_code, C4=engram_mcp. This proposal standardizes on the deployed numbering. URLs and paths are unambiguous everywhere.

## Goals

- **C1 full state survives redeploy**: workers registry, worker_memory (historial de equipos conectados), per-worker stats/tokens, principal_id, mode, paused/current_task → Supabase mirror + boot recovery; workers re-register naturally (`bridge.js:222-244`).
- **DeepSeek history in C1**: gateway writes `gateway_history` (async fire-and-forget, pgx pattern `main.go:80-159`); C1 `/api/gateway-history` list + delete one/all; frontend "Historial DeepSeek" view. Retention 30 days + manual delete.
- **Carlos Code history in C1**: same pattern → `carlos_code_history`, `/api/carlos-history`, "Historial Carlos Code" view. Retention 30 days.
- **Keepalive toggle reliable**: `.cai_keepalive.json` file fallback + boot load; 30s polling of `/api/keepalive`; remove dead WS broadcast (`server.py:1901`) and dead inline `renderServices` (`index.html:1377-1457`); optimistic UI + revert + toast.
- **Complete services view**: add C6 worker-code and "Supabase DB" entries; per-service `urls` chips; ping all 7 targets every 5 min (GET `/health`, `SELECT 1` for Supabase).
- **ONE logical Engram**: Supabase mirror in `engram_mcp` (sqlite as cache/fallback, load-from-Supabase on boot when sqlite empty) + `POST /import` (idempotent upsert/dedup) to seed the 23 observations; leader/workers log every task.
- **Umbrella**: every RAM-only state maps to a Supabase table; only WebSocket/poll connection objects stay ephemeral.

## Non-Goals

- No changes to task distribution / rebalancing / planning logic.
- No auth changes beyond the existing `gateway_auth` pattern.
- No new frontend framework — vanilla JS panel as-is.
- No multi-project Engram — single logical Engram per confirmed decision.
- No queueing/retry machinery for history or Engram writes (fire-and-forget by design).

## Proposed Change

### 1. Persist C1 RAM state to Supabase (`control/server`)

- `db.py`: add tables `worker_memory` (id, snapshot_json, last_seen) and `panel_state` (single row: principal_id, mode, paused, current_task). Add `load_workers()` and `save_worker_full(wid, w)` storing stats_json (existing `save_worker` `db.py:168-183` persists status only on heartbeat timeout — insufficient).
- Boot (`server.py:1779-1796`): after `init_db`, load workers + worker_memory into RAM and restore `panel_state`; live connections rebuild via existing worker re-registration (`bridge.js:222-244`), no new protocol.
- **Decide-and-justify**: debounce writes ~5 s per worker (per-worker dirty dict + periodic flush) to avoid write amplification from status churn; reuses existing pool + graceful no-`DATABASE_URL` degradation (`db.py:79-120`).

### 2. DeepSeek history (`gateway_history`)

- `control/gateway`: new table `gateway_history` + async fire-and-forget INSERT (request path, prompt, response snippet, status, latency, ts) in the proxy handler, reusing the pgx pool pattern (`main.go:80-159`); failures log only, never break the request.
- C1: `GET /api/gateway-history` (list, limit), `DELETE /api/gateway-history/{id}`, `DELETE /api/gateway-history` (all) via existing `db.py` pool; `db.py` gains the table + CRUD.
- Frontend: "Historial DeepSeek" view — list + delete one/all buttons.
- Retention: hourly cleanup task `DELETE WHERE ts < now() - interval '30 days'` + manual delete.

### 3. Carlos Code history (`carlos_code_history`)

- `control/carlos_code` (plain net/http, no pgx yet — add pool, same params): table `carlos_code_history` (prompt, plan_summary, mode, workers, status, ts), async write after `/plan` and `/update` succeed.
- C1: `GET /api/carlos-history` + delete one/all mirroring gateway-history.
- Frontend: "Historial Carlos Code" view, same list/delete UX. Retention 30 days + manual delete, same cleanup task.

### 4. Keepalive button ON/OFF reliable

- (a) File fallback: toggle also writes `.cai_keepalive.json`; boot loads DB first, file second (`db.py:281-313` single-row pattern stands).
- (b) `services.js`: poll `/api/keepalive` every 30 s so the label and status box update without reload (today only fetched on `DOMContentLoaded`, `services.js:223-237`).
- (c) Remove dead `broadcast_panel({"tipo": "keepalive", ...})` at `server.py:1901`; keep `broadcast_panel` for live events (events, principal_update).
- (d) Delete dead inline `renderServices` (`index.html:1377-1457`) — `services.js` is the single source of truth.
- (e) `toggleKeepalive` (`services.js:171-196`) already reverts + toasts on error; make it optimistic (flip immediately, revert on failure).

### 5. Services view: add C6 + Supabase, ping all every 5 min

- `services.js`: add SVC-C6 worker-code (`healthUrl https://worker-code.onrender.com/health`) and SVC-DB Supabase (no `healthUrl`; status "gestionado/activo" derived from `db_stage` in `/api/state`).
- C1 currently has NO `/health` route (routes list `server.py:798-1652`) → add one `GET /health`; add `healthUrl https://bridgecarlos.onrender.com/health` to the C1 card.
- `KEEPALIVE_SERVICES` (`server.py:53-59`) already covers C2–C6; add SVC-C1 (GET `/health`) and SVC-DB (SELECT 1 on the existing pool instead of GET). Interval already 300 s (`server.py:51`).
- Frontend status: `refreshServiceStatuses` + `serviceStatusCache` (`services.js:55-74`) remains the source of truth.

### 6. URL buttons per service

- Card model gains `urls: [{label, url}]` rendered as small chips opening in a new tab; primary card click behavior kept (`services.js:101-110`). Inventory (base → endpoints):
  - C2 gateway: `/health`, `/v1/models`, `/v1/chat/completions`, `/api/auth`, `/proxy/ui`, `/proxy/status`, `/proxy/logs`
  - C3 carlos-code: `/health`, `/plan`, `/tools`, `/update`, `/finalize`, `/git`
  - C4 engram-mcp: `/health`, `/search`, `/recent`, `/stats`
  - C5 context7: `/health`, `/context7/resolve`, `/context7/docs`, `/grep_app/search`, `/search`
  - C6 worker-code: `/health`
  - C1 panel: `/`, `/login`, `/api/state`
  - Supabase: dashboard https://supabase.com/dashboard/project/qqaocynurdhtmmxktzcz

### 7. Dead "En desarrollo" label

- `index.html:1439/1453` belongs to the dead inline `renderServices` (see 4d). All 6 services are live; after 4d no "En desarrollo" branch exists — the live health check is the source of truth.

### 8. Engram durable + synced (ONE logical Engram — `control/engram_mcp`)

- Keep sqlite3 as cache/fallback; add Supabase mirror (asyncpg, table `engram_observations`: id, title, type, topic, content, tags, created_at, updated_at). Boot: load from Supabase when sqlite is empty; every `/save` writes both (Supabase primary).
- Add `POST /import` (body: observations array) — upsert by `id` when present, else dedup by normalized title+content. Used to push the existing 23 observations (idempotent).
- Seed bootstrap: bundle `cai/engram/observations.json` in the image AND one-time `POST /import` of the 23. Sync flows both ways: local `/save` → sqlite + Supabase; GitHub/local Engram act as backup of the same logical store.
- New dependency: `asyncpg` in `engram_mcp/requirements.txt`.

### 9. Leader and workers update Engram after each task

- `setup/bridge/bridge.js`: new `ENGRAM_URL` config (default `https://engram-mcp-q9ln.onrender.com`); after task completion POST `/save` with `{title: "Task {id} completed", type: "task", topic: "tasks/{id}", content: prompt + result, tags: ["worker"]}`.
- `control/carlos_code`: after `/plan` succeeds, POST `/save` with `tags: ["leader"]`.
- C1 `control/server`: after task completion, server-side POST via `httpx.AsyncClient` (already a dependency, `server.py:1882`) with `tags: ["leader"/"worker"]` as applicable.
- All writes are fire-and-forget with timeout — failures MUST NOT break the task flow.

### 10. Everything RAM → Supabase (umbrella)

| RAM state (`server.py`) | Table | Change |
|---|---|---|
| `workers` (70) | `workers` (`db.py:56`) | + `load_workers` + `save_worker_full` |
| `worker_memory` (85) | `worker_memory` | new |
| `principal_id` / `mode` / `paused` / `current_task` (73-75) | `panel_state` | new, single row |
| `KEEPALIVE_STATE` (60-64) | `keepalive_state` (`db.py:72`) | exists + file fallback (4a) |
| `tasks` (71) | `tasks`/`subtasks` (`db.py:61-68`) | exists |
| — (new) | `gateway_history` | new |
| — (new) | `carlos_code_history` | new |
| — (new) | `engram_observations` (engram_mcp) | new |

Only WebSocket/long-poll connection objects remain RAM-only (inherently ephemeral; rebuilt via re-registration).

## Capabilities

> Contract for the spec phase (sdd-spec): each listed capability gets a full spec at `openspec/changes/persistence-history/specs/<name>/spec.md` and becomes `openspec/specs/<name>/spec.md` at archive.

### New Capabilities

- `c1-state-persistence`: workers registry, worker_memory, panel_state survive redeploy (mirror + boot recovery + debounce).
- `gateway-history`: DeepSeek request/response history — async write, read, delete one/all, 30-day retention.
- `carlos-history`: Carlos Code history — async write (plan/update), read, delete one/all, 30-day retention.
- `services-monitor`: service inventory (C1–C6 + Supabase), per-service URL chips, 5-min ping loop, keepalive reliability UX (file fallback, 30s polling, optimistic toggle, dead-code removal).
- `engram-sync`: one logical Engram — Supabase mirror in engram_mcp, `/import` seeding, boot load, post-task logging from bridge / carlos_code / C1.

### Modified Capabilities

None — `openspec/specs/` is empty; all capabilities are new.

## Migration & Rollback

- **Additive only**: new tables (`worker_memory`, `panel_state`, `gateway_history`, `carlos_code_history`, `engram_observations`) are `CREATE TABLE IF NOT EXISTS`; existing tables untouched; `.cai_tasks.json` fallback stays.
- **Rollback**: git revert + redeploy — new endpoints/tables simply stop being referenced; old frontend remains compatible (new views disappear). No data migration required; no destructive deltas.

## Deploy Ordering

- Push to git remote `1000carlospena-prog/cai` using `env -u GITHUB_TOKEN -u GH_TOKEN git push` (surfaces the SSH/HTTPS credential prompt).
- Deploy via Render API per affected service: C1 `control/server`, C2 `control/gateway`, C3 `control/carlos_code`, C4 `control/engram_mcp`, C5 `control/context7_grep` (only if changed), C6 `control/worker_code` (only if changed). `setup/bridge` is client-side — workers update on git pull, no deploy.
- Suggested order: C4 (Engram `/import` target) → C2/C3 (history writers) → C1 (reader UI + boot recovery), so tables/endpoints exist before the UI reads them.

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Pooler/SSL drift across 4 services | Med | Reuse proven params (`ssl=require`, `statement_cache_size=0`, `_ipv4` helper) from `db.py:96-106` / `main.go:86-99` |
| Write amplification from worker churn | Med | 5 s per-worker debounce + flush on meaningful change only |
| Engram divergence (sqlite vs Supabase vs GitHub) | Med | Supabase primary; load-on-boot-if-empty; `/import` idempotent; GitHub backup contract unchanged |
| Fire-and-forget writes lost on crash | Low | Accepted by design; history is best-effort, retention bounds growth |
| Keepalive toggle UX regressions | Low | Optimistic UI + revert + toast; 30 s polling self-heals label |
| Unbounded history/history-table growth | Med | Hourly 30-day retention cleanup + manual delete |

## Success Criteria

- [ ] Redeploying C1 restores workers registry, worker_memory, principal_id, mode, paused, and keepalive state from Supabase; workers re-register without manual action.
- [ ] "Historial DeepSeek" and "Historial Carlos Code" views list entries and delete one/all; rows older than 30 days disappear.
- [ ] Keepalive toggle survives C1 redeploy (DB + file fallback); label/box refresh within 30 s without reload; instant optimistic feedback on toggle.
- [ ] Services view shows 7 targets (C1–C6 + Supabase DB), all pinged every 5 min; URL chips open each endpoint in a new tab.
- [ ] `engram_mcp /import` ingests the 23 observations idempotently; `/save` persists to Supabase; C4 redeploy keeps all observations.
- [ ] bridge/carlos_code/C1 log `Task {id} completed` observations; task flow unaffected when Engram is down or slow.

## What Follows

1. **Spec phase (sdd-spec)** — 5 capability specs under `openspec/changes/persistence-history/specs/` (Given/When/Then, RFC 2119).
2. **Design phase (sdd-design)** — sequence diagrams for boot recovery, history write path, Engram dual-write sync.
3. **Tasks (sdd-tasks) → Apply (sdd-apply) → Verify (sdd-verify)** — per `config.yaml` conventions (Spanish UI copy preserved; test budget per-project, `strict_tdd: false`).
4. **Archive (sdd-archive)** — merge capability specs into `openspec/specs/`.