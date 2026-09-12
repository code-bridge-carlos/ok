# Tasks: persistence-history

## Review Workload Forecast

| Field | Value |
|-------|-------|
| Estimated changed lines | ~950-1200 |
| 400-line budget risk | High |
| Chained PRs recommended | Yes |
| Suggested split | PR 1 (C4 Engram) → PR 2 (C2/C3 writers) → PR 3 (C1 reader+recovery) → PR 4 (services/monitor) |
| Delivery strategy | auto-chain |
| Chain strategy | stacked-to-main |

Decision needed before apply: No
Chained PRs recommended: Yes
Chain strategy: stacked-to-main
400-line budget risk: High

### Suggested Work Units

| Unit | Goal | Likely PR | Focused test command | Runtime harness | Rollback boundary |
|------|------|-----------|----------------------|-----------------|-------------------|
| 1 | Engram durable mirror+import (C4) | PR 1 | n/a (Python, no cfg) | manual `/import` + `/save` on C4 | revert C4; sqlite stays |
| 2 | History writers C2/C3 (tables+async) | PR 2 | `go test ./...` in `control/gateway` & `control/carlos_code` | manual chat/plan then check table | revert C2/C3 |
| 3 | C1 state persistence + history reader UI | PR 3 | n/a (Python) go build; manual boot | C1 redeploy + views | revert `server.py`/`db.py` |
| 4 | Services monitor + keepalive UX | PR 4 | n/a; manual panel checks | manual services view/poll | revert `services.js`/`index.html` |

## Phase 1: Engram durable mirror (C4) — deploy first

- [x] 1.1 `control/engram_mcp/requirements.txt`: add `asyncpg`
- [x] 1.2 `control/engram_mcp/server.py`: asyncpg pool (proven params), `CREATE TABLE IF NOT EXISTS engram_observations` (design:102-104)
- [x] 1.3 `control/engram_mcp/server.py`: dual-write `/save` — Supabase primary + sqlite cache, fail-open on Supabase error (spec:60-72)
- [x] 1.4 `control/engram_mcp/server.py`: on boot, load Supabase→sqlite when sqlite empty; skip if non-empty (spec:30-40)
- [x] 1.5 `control/engram_mcp/server.py`: `POST /import` idempotent — upsert by id else dedup by normalized title+content (spec:42-56)
- [x] 1.6 Bundle `cai/engram/observations.json` (23 rows) and one-time `POST /import` to seed

## Phase 2: History writers (C2/C3) — deploy second

- [x] 2.1 `control/gateway/main.go`: DDL `gateway_history` + `INDEX` (design:94-97), reusing pgx pool (main.go:80-159)
- [x] 2.2 `control/gateway/main.go`: fire-and-forget `saveGatewayHistory` (5 s ctx timeout) in `chatCompletionsHandler` (main.go:780); log-only on failure, never block (spec:9-25)
- [x] 2.3 `control/carlos_code/main.go`: add pgx pool (same proven params) + DDL `carlos_code_history` + `INDEX` (design:98-101)
- [x] 2.4 `control/carlos_code/main.go`: async post-save after `/plan` (94) and `/update` (885) succeed; log-only on failure (spec:9-23)

## Phase 3: C1 state persistence + reader UI — deploy third

- [ ] 3.1 `control/server/db.py`: DDL `worker_memory` + `panel_state` (design:90-93); `load_workers`, `save_worker_full`, `save_panel_state`, `load_panel_state`, `save_worker_memory` (design:107)
- [ ] 3.2 `control/server/server.py` boot (1779-1795): after `init_db` load workers+memory+panel_state+keepalive (DB then file); skip when no `DATABASE_URL` (spec:15-43)
- [ ] 3.3 `control/server/server.py`: debounce — `_dirty_workers: set`, periodic flush every 5 s (design:109); fail-open
- [ ] 3.4 `control/server/db.py`+`server.py`: `gateway_history`/`carlos_code_history` CRUD + `GET /api/gateway-history`+`/api/carlos-history` (limit), DELETE one/all (design:111; spec:27-45,36-44)

## Phase 4: Services monitor + keepalive UX

- [ ] 4.1 `control/server/server.py`: `GET /health` route; add SVC-C1 + SVC-DB to `KEEPALIVE_SERVICES` (53-59); SVC-DB = `SELECT 1` on pool (proposal:87-88)
- [ ] 4.2 `control/server/server.py`+`db.py`: keepalive also writes `.cai_keepalive.json`; boot loads DB first, file second (design:48-54)
- [ ] 4.3 `control/server/static/js/services.js`: add SVC-C6 + SVC-DB entries, per-service `urls` chips, `healthUrl` for C1 (proposal:85-100)
- [ ] 4.4 `control/server/static/js/services.js`: 30 s poll `/api/keepalive` (223-237); optimistic `toggleKeepalive` flip+revert+toast (171-196) (spec:49-74)
- [ ] 4.5 `control/server/server.py`: remove dead keepalive WS broadcast (1901); `control/server/static/index.html`: delete dead inline `renderServices` (1377-1457) + "En desarrollo" (1439/1453) (spec:75-83)
- [ ] 4.6 Hourly 30-day retention cleanup for `gateway_history` + `carlos_code_history` (design:143, spec:57-65,67-72)

## Phase 5: Post-task Engram logging

- [x] 5.1 `setup/bridge/bridge.js`: `ENGRAM_URL` config + POST `/save` on task completion, `tags:["worker"]`, fire-and-forget (spec:58-72)
- [x] 5.2 `control/carlos_code/main.go`: post-`/plan` `/save`, `tags:["leader"]`, fire-and-forget (spec:70-72)
- [x] 5.3 `control/server/server.py`: post-task `/save` via `httpx.AsyncClient` (already dep, server.py:1882), `tags:["leader"/"worker"]` (spec:58-72)

## Phase 6: Umbrella + verification

- [ ] 6.1 Confirm every RAM-only C1 state maps to a Supabase table; only WS/long-poll connection objects stay ephemeral (proposal:120-133)
- [ ] 6.2 Verify per-project `go build ./...` (C2/C3) and manual C1/C4 boot recovery + views (design:115-124)
