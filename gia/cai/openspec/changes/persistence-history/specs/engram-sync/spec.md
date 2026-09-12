# engram-sync Specification

## Purpose

Converge on ONE logical Engram for the ecosystem: local agent Engram (`~/.engram`, project `cai`) ⟷ GitHub backup (`cai/engram/observations.json`) ⟷ the Render Engram MCP service (C4) backed by Supabase. engram_mcp keeps sqlite3 as cache/fallback, adds an asyncpg Supabase mirror (`engram_observations`), loads from Supabase on boot when sqlite is empty, and dual-writes on every `/save` (Supabase primary). `POST /import` seeds existing observations idempotently. The leader and workers log "Task {id} completed" observations after task completion, fire-and-forget, so failures never break the task flow. (proposal §8, §9)

## Requirements

### Requirement: Supabase mirror in engram_mcp

The system MUST keep sqlite3 as cache/fallback and add an asyncpg Supabase mirror on table `engram_observations` (id, title, type, topic, content, tags, created_at, updated_at), dual-writing on every `/save` with Supabase as primary.

#### Scenario: Save writes to both stores

- GIVEN a `/save` call
- WHEN engram_mcp processes it
- THEN the observation is written to Supabase (primary) and mirrored to sqlite
- AND the response identifies Supabase as the durable primary

#### Scenario: Supabase write failure keeps sqlite

- GIVEN Supabase is unreachable during `/save`
- WHEN a save is attempted
- THEN sqlite still records the observation and the failure is surfaced without corrupting the response

### Requirement: Boot load from Supabase when sqlite empty

The system MUST load observations from Supabase into sqlite when sqlite starts empty, so the store survives redeploy.

#### Scenario: C4 redeploy restores observations

- GIVEN sqlite is empty after a fresh deploy
- WHEN engram_mcp boots
- THEN observations are loaded from Supabase, preserving prior state

#### Scenario: Non-empty sqlite is authoritative

- GIVEN sqlite already contains observations
- WHEN engram_mcp boots
- THEN it does not overwrite the existing sqlite data

### Requirement: Idempotent /import seeding

The system MUST provide `POST /import` (body: observations array) that upserts by `id` when present, else dedups by normalized title+content, such that repeated imports are idempotent.

#### Scenario: First import seeds observations

- GIVEN an empty store and a 23-observation array
- WHEN `POST /import` is called
- THEN the observations are stored once

#### Scenario: Repeated import does not duplicate

- GIVEN the same 23-observation array was already imported
- WHEN `POST /import` is called again
- THEN upsert/dedup prevents duplicate rows and the count is unchanged

### Requirement: Post-task logging from bridge / carlos_code / C1

The system MUST log "Task {id} completed" observations after task completion from `setup/bridge` (tags `["worker"]`), `carlos_code` after `/plan` (tags `["leader"]`), and C1 (tags `["leader"/"worker"]` as applicable), configured via `ENGRAM_URL` (default `https://engram-mcp-q9ln.onrender.com`).

#### Scenario: Worker logs completion

- GIVEN a worker task completes
- WHEN the bridge posts `/save`
- THEN a "Task {id} completed" observation with tags `["worker"]` is recorded

#### Scenario: Leader logs plan

- GIVEN `/plan` succeeds in carlos_code
- WHEN carlos_code posts `/save`
- THEN a "Task {id} completed" observation with tags `["leader"]` is recorded

### Requirement: Fire-and-forget isolation

The system MUST make all Engram writes fire-and-forget with a timeout; failures MUST NOT break the task flow.

#### Scenario: Engram down does not block tasks

- GIVEN Engram is down or slow
- WHEN a task completes and a save is attempted
- THEN the save times out and is dropped while the task flow proceeds unaffected

### Requirement: Additive dependency

The system MUST add `asyncpg` to `engram_mcp/requirements.txt` and create `engram_observations` idempotently, without disturbing the existing sqlite store.

#### Scenario: Dependency and schema installed cleanly

- GIVEN a fresh engram_mcp environment
- WHEN the service installs and boots
- THEN `asyncpg` is available and `engram_observations` exists without altering existing stores
