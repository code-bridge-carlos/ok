# c1-state-persistence Specification

## Purpose

Make C1's RAM-only control-panel state durable across redeploys by mirroring it to Supabase with boot recovery: the workers registry, worker_memory (historial de equipos conectados), per-worker stats/tokens, and panel_state (principal_id, mode, paused, current_task). Live WebSocket connections rebuild naturally via worker re-registration, and writes are debounced to avoid amplification. Graceful degradation keeps the panel RAM-only when `DATABASE_URL` is absent. (proposal §1, §10)

## Requirements

### Requirement: Worker and panel state survives redeploy

The system MUST persist the C1 workers registry, per-worker stats/tokens, worker_memory ("historial de equipos conectados"), and panel_state (principal_id, mode, paused, current_task) to Supabase and MUST restore them into RAM at boot.

#### Scenario: Full recovery after redeploy

- GIVEN Supabase is reachable and `worker_memory`/`panel_state` rows exist
- WHEN C1 boots and runs `init_db`
- THEN the workers registry, worker_memory, principal_id, mode, paused, and current_task are restored from Supabase into RAM

#### Scenario: Live connections rebuild without new protocol

- GIVEN restored state and running workers
- WHEN a worker re-registers via its heartbeat (bridge.js)
- THEN its live WebSocket connection is re-established under the restored worker entry, no new protocol added

### Requirement: Diligent writes are debounced

The system SHOULD debounce Supabase writes to ~5 s per worker using a per-worker dirty dict and a periodic flush, writing only on meaningful change.

#### Scenario: Status churn does not amplify writes

- GIVEN a worker with frequent status changes
- WHEN multiple updates occur within 5 s
- THEN a single debounced write flushes the latest state, not one write per change

### Requirement: Graceful degradation without DATABASE_URL

The system MUST keep the panel fully functional RAM-only when `DATABASE_URL` is missing or malformed, reusing the existing degradation behavior.

#### Scenario: No database configured

- GIVEN `DATABASE_URL` is unset
- WHEN C1 boots
- THEN the panel runs with RAM-only state and components that depend on Supabase are skipped without crashing

### Requirement: Reuse proven pool parameters

The system MUST reuse the existing pool configuration (`ssl=require`, `statement_cache_size=0`, `_ipv4` helper) for all new tables.

#### Scenario: Pooler-compatible connection

- GIVEN Supabase pooler connection
- WHEN creating the new tables and querying them
- THEN the proven parameters are used, remaining compatible with pgbouncer

### Requirement: New tables are additive

The system MUST create the `worker_memory` and `panel_state` tables with `CREATE TABLE IF NOT EXISTS`; `panel_state` MUST be a single row.

#### Scenario: Idempotent schema creation

- GIVEN an existing database
- WHEN C1 boots and creates tables
- THEN new tables are created if absent and existing tables remain untouched
- AND `panel_state` holds a single row
