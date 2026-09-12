# Design: persistence-history

## Technical Approach

Mirror every RAM-only C1 state to Supabase (one pool config), restore at boot; add fire-and-forget history writers in C2/C3 and an asyncpg mirror in C4; make keepalive and services view durable and complete. All changes additive (`CREATE TABLE IF NOT EXISTS`), reusing the proven pooler-safe params (`ssl=require`, `statement_cache_size=0`, `_ipv4`) from `db.py:96-106` and `main.go:86-99`. One Supabase project `qqaocynurdhtmmxktzcz`; one logical Engram store.

## Architecture Decisions

| # | Decision | Option | Tradeoff | Choice |
|---|---|---|---|---|
| D1 | C1 write strategy | Write-per-change vs debounce | amplification from status churn | ~5 s per-worker debounce (per-worker dirty dict + periodic flush) |
| D2 | Persistence failure | fail-open vs fail-closed | availability vs durability | fail-open: RAM-only no-`DATABASE_URL`, log + skip (`db.py:79-120` pattern) |
| D3 | History write | sync vs async fire-and-forget | durability vs latency | fire-and-forget goroutine with timeout; never blocks request |
| D4 | Engram store hierarchy | dual-write both / sqlite primary / Supabase primary | durability vs cache | Supabase primary; sqlite cache/fallback; boot-load-Supabase-when-empty |
| D5 | Keepalive boot order | DB-only / file-first / DB-first | consistency vs recovery | DB first, `.cai_keepalive.json` second |
| D6 | Service ping | GET only / GET+SELECT 1 | coverage | GET `/health` for HTTP; `SELECT 1` on pool for Supabase |

## Data Flow

```
C1 boot: init_db → load_workers → load_worker_memory → load_panel_state → load_keepalive(DB,file) → loops
C2/C3: proxy/plan/update handler ──(fire-and-forget goroutine)──► INSERT ──► response (never blocked)
C1 read: GET /api/*-history ──► db.py pool ──► rows ──► view
C4: /save ──► sqlite + SUPABASE(primary); boot(sqlite empty) ──► Supabase ──► sqlite
```

## Sequence Diagrams

**Boot recovery** (C1):
```
[server.py:1779 startup] --> db.init_db()
  --> CREATE TABLE IF NOT EXISTS worker_memory, panel_state
  --> load_workers()[:workers]  load_worker_memory()[:worker_memory]
  --> load_panel_state()[:principal_id,:mode,:paused,:current_task]
  --> load_keepalive()(DB) then .cai_keepalive.json fallback
  --> workers re-register via bridge.js heartbeat (no new protocol)
```

**History write path** (gateway + C1 read):
```
browser --> C1 GET /api/gateway-history :limit
  C1 --> gateway_history (db.py SELECT ... ORDER BY ts DESC LIMIT n) --> JSON --> view
client --> C2 chatCompletionsHandler [main.go:780]
  C2 --> go saveGatewayHistory(...)   (fire-and-forget, 5s ctx timeout)
  C2 --> response                     (unaffected on write failure)
```

**Keepalive toggle persistence**:
```
user click --> toggleKeepalive [services.js:171] (optimistic flip)
  --> POST /api/keepalive --> save_keepalive(DB) + .cai_keepalive.json
  --> success: toast, render : failure : revert + error toast
  --> 30s poll /api/keepalive self-heals label/box (no reload)
```

**Engram dual-write + import**:
```
POST /import (23 obs) --> engram_mcp --> upsert by id / dedup by norm(title+content) --> sqlite+Supabase (idempotent)
POST /save (bridge/carlso/C1) --> engram_mcp --> Supabase(primary) + sqlite(cache) --> response
boot: sqlite empty? --> load from Supabase --> sqlite (C4 redeploys stay durable)
```

## File Changes

### C1 (control/server)
| File | Action | Description |
|---|---|---|
| `db.py` | Modify | DDL for `worker_memory`; `load_workers`, `save_worker_full`, `save_panel_state`, `load_panel_state`; history CRUD (`gateway_history`, `carlos_code_history`); debounce flush |
| `server.py` | Modify | boot recovery (1779-1795); debounce task (flush every 5 s); GET `/health`; `/api/gateway-history` + `/api/carlos-history` CRUD; add SVC-C1 + SVC-DB to `KEEPALIVE_SERVICES` (53-59); remove `broadcast_panel keepalive` (1901); ENGRAM_URL post-task logging |
| `static/js/services.js` | Modify | SVC-C6 + SVC-DB entries, `urls` chips, 30 s keepalive poll, optimistic toggle revert |
| `static/index.html` | Modify | remove dead inline `renderServices` (1377-1457) + "En desarrollo" (1439/1453); history views |

### C2 (control/gateway)
| `main.go` | Modify | `gateway_history` DDL + `saveGatewayHistory` fire-and-forget in `chatCompletionsHandler` (780) |

### C3 (control/carlos_code)
| `main.go` | Modify | add pgx pool (same params); `carlos_code_history` DDL; async write after `/plan` (94) & `/update` (885); leader `/save` post-plan |

### C4 (control/engram_mcp)
| `server.py` | Modify | asyncpg Supabase mirror, dual-write, boot-load-if-empty, `POST /import` |
| `requirements.txt` | Modify | add `asyncpg` |

### C5 context7 / C6 worker_code / setup/bridge / engram/observations.json
Not changed (C5/C6 out of scope; bridge gets `ENGRAM_URL` post-task logging config).

## Interfaces / Contracts

**DDL sketches** (pooler-safe: `ssl=require`, `statement_cache_size=0`, `_ipv4`):
```sql
CREATE TABLE IF NOT EXISTS worker_memory (
  id TEXT PRIMARY KEY, snapshot_json TEXT, last_seen TIMESTAMPTZ DEFAULT now());
CREATE TABLE IF NOT EXISTS panel_state (
  id TEXT PRIMARY KEY, snapshot_json TEXT, updated_at TIMESTAMPTZ DEFAULT now());
CREATE TABLE IF NOT EXISTS gateway_history (
  id BIGSERIAL PRIMARY KEY, path TEXT, prompt TEXT, response_snippet TEXT,
  status INT, latency_ms INT, ts TIMESTAMPTZ DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_gw_ts ON gateway_history(ts);
CREATE TABLE IF NOT EXISTS carlos_code_history (
  id BIGSERIAL PRIMARY KEY, prompt TEXT, plan_summary TEXT, mode TEXT,
  workers TEXT, status TEXT, ts TIMESTAMPTZ DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_cc_ts ON carlos_code_history(ts);
CREATE TABLE IF NOT EXISTS engram_observations (
  id TEXT PRIMARY KEY, title TEXT, type TEXT, topic TEXT, content TEXT,
  tags TEXT, created_at TEXT, updated_at TEXT);
```

**C1 signatures (db.py)**: `load_workers() -> dict`, `save_worker_full(wid: str, w: dict)`, `save_panel_state(snapshot: dict)`, `load_panel_state() -> dict`, `save_worker_memory(wid)`.

**Debounce (server.py)**: `_dirty_workers: set[str]`; mark on change; periodic flush task every 5 s calls `save_worker_full` for dirty ids and clears set.

**C1 routes**: `GET /health`; `GET /api/gateway-history?limit=N`; `DELETE /api/gateway-history/{id}`; `DELETE /api/gateway-history`; mirror `/api/carlos-history`.

**C4 endpoints**: `POST /import` (array; upsert by id else dedup by norm(title+content)); `/save` dual-writes.

## Testing Strategy

| Layer | What | How |
|---|---|---|
| C1 | boot restore, debounce, history CRUD, /health | `go test` n/a (Python, none configured); manual + db fns |
| C2/C3 | fire-and-forget write, failure isolation | `go test ./...` per project |
| C4 | dual-write, /import idempotency, boot-load | manual (no test cfg) |
| e2e | keepalive reload recovery, 30 s poll | manual panel checks |

Per `config.yaml`: `strict_tdd: false`, per-project test commands only.

## Threat Matrix

N/A — no routing, shell, subprocess, VCS/PR automation, executable-file classification, or process-integration boundary introduced. (Additive DDL + HTTP endpoints + net/http pgx pool only.)

## Migration / Rollout

Additive-only. Deploy order: C4 (Engram + imports the 23) → C2/C3 (history writers) → C1 (reader UI + boot recovery), so tables/endpoints exist before UI reads. Push via `env -u GITHUB_TOKEN -u GH_TOKEN git push`. Rollback = git revert + redeploy; old frontend stays compatible (new views simply disappear). No destructive deltas; `.cai_tasks.json` fallback retained.

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Pooler/SSL drift across 4 services | Med | single proven params reused everywhere |
| Write amplification | Med | 5 s per-worker debounce; flush only on meaningful change |
| Engram divergence | Med | Supabase primary + boot-load-if-empty + idempotent /import |
| Fire-and-forget loss on crash | Low | accepted; retention bounds growth |
| `.cai_keepalive.json` stale vs DB | Low | DB-first boot, file as fallback only |
| History growth | Med | hourly 30-day cleanup + manual delete |

## Open Questions

- None blocking design; decisions were specified as closed in proposal+specs.
