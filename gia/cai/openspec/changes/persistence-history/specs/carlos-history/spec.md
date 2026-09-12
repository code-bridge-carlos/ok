# carlos-history Specification

## Purpose

Record Carlos Code request history (`carlos_code_history`) written asynchronously fire-and-forget by the C3 carlos_code service after `/plan` and `/update` succeed, and surfaced through C1 with a "Historial Carlos Code" view. Carlos code (plain net/http) gains a pgx pool with the same proven parameters. Retention is 30 days via the same hourly cleanup plus manual delete. (proposal §3)

## Requirements

### Requirement: Carlos code writes history asynchronously

The system MUST write a `carlos_code_history` row (prompt, plan_summary, mode, workers, status, ts) fire-and-forget after `/plan` and `/update` succeed, not breaking the request flow on failure.

#### Scenario: Write after plan succeeds

- GIVEN a `/plan` request succeeds
- WHEN carlos_code completes planning
- THEN a `carlos_code_history` row is written with prompt, plan_summary, mode, workers, status, and timestamp

#### Scenario: Write failure never breaks plan/update

- GIVEN the history write fails or the pool is unavailable
- WHEN a plan or update completes
- THEN the failure is logged only and the plan/update result is unaffected

### Requirement: Carlos code uses a compatible pgx pool

The system MUST add a pgx pool to carlos_code (plain net/http) using the same parameters as the gateway/C1 (`ssl=require`, `statement_cache_size=0`, `_ipv4`).

#### Scenario: Pooler-compatible connection

- GIVEN Supabase pooler via `DATABASE_URL`
- WHEN carlos_code connects to write history
- THEN the proven parameters are used and remain compatible with pgbouncer

### Requirement: C1 exposes history CRUD

The system MUST expose `GET /api/carlos-history` (list, limit), `DELETE /api/carlos-history/{id}` (one), and `DELETE /api/carlos-history` (all), mirroring gateway-history.

#### Scenario: List, delete one, delete all

- GIVEN stored `carlos_code_history` rows
- WHEN a client lists, deletes one, or deletes all
- THEN the corresponding rows are returned or removed consistently

### Requirement: Frontend "Historial Carlos Code" view

The system SHOULD provide a "Historial Carlos Code" frontend view with the same list/delete UX as DeepSeek history.

#### Scenario: View lists and deletes

- GIVEN the Carlos Code history view is open
- WHEN the user opens it and clicks a delete control
- THEN the list shows history entries and the chosen one(s) are deleted

### Requirement: 30-day retention cleanup

The system MUST subject `carlos_code_history` to the same hourly 30-day retention cleanup and manual delete as gateway history.

#### Scenario: Old rows are pruned

- GIVEN rows older than 30 days
- WHEN the hourly cleanup runs
- THEN those rows are deleted and newer rows are retained

### Requirement: Additive table schema

The system MUST create `carlos_code_history` via `CREATE TABLE IF NOT EXISTS`, leaving existing tables untouched.

#### Scenario: Idempotent creation

- GIVEN an existing database
- WHEN the table is created
- THEN `carlos_code_history` is created if absent without disturbing existing tables
