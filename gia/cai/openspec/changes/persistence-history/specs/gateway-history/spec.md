# gateway-history Specification

## Purpose

Record DeepSeek request/response history (`gateway_history`) written asynchronously fire-and-forget by the C2 gateway, and surfaced through C1 with a "Historial DeepSeek" view that lists and deletes entries. Retention is 30 days via an hourly cleanup plus manual delete. Failures must never break the request flow. (proposal §2)

## Requirements

### Requirement: Gateway writes history asynchronously

The system MUST write a `gateway_history` row (request path, prompt, response snippet, status, latency, ts) fire-and-forget in the proxy handler, reusing the existing pgx pool pattern.

#### Scenario: Successful write after a request

- GIVEN a DeepSeek proxy request completes
- WHEN the gateway handler runs
- THEN a `gateway_history` row is written asynchronously with path, prompt, snippet, status, latency, and timestamp

#### Scenario: Write failure never breaks the request

- GIVEN the history write fails or the pool is unavailable
- WHEN the proxy handler responds
- THEN the failure is logged only and the client request/response is unaffected

### Requirement: C1 exposes history CRUD

The system MUST expose `GET /api/gateway-history` (list, with limit), `DELETE /api/gateway-history/{id}` (one), and `DELETE /api/gateway-history` (all) via the existing `db.py` pool.

#### Scenario: List history with limit

- GIVEN stored `gateway_history` rows
- WHEN a client calls `GET /api/gateway-history?limit=N`
- THEN the list returns up to N rows ordered by timestamp

#### Scenario: Delete one row

- GIVEN a stored history row with id
- WHEN a client calls `DELETE /api/gateway-history/{id}`
- THEN the row is removed and subsequent lists omit it

#### Scenario: Delete all rows

- GIVEN at least one stored history row
- WHEN a client calls `DELETE /api/gateway-history`
- THEN all `gateway_history` rows are removed

### Requirement: Frontend "Historial DeepSeek" view

The system SHOULD provide a "Historial DeepSeek" frontend view with a list and delete one/all controls.

#### Scenario: View lists and deletes

- GIVEN the DeepSeek history view is open
- WHEN the user opens it and clicks a delete control
- THEN the list shows history entries and the chosen one(s) are deleted

### Requirement: 30-day retention cleanup

The system MUST run an hourly cleanup that deletes rows where `ts < now() - interval '30 days'`, in addition to manual delete.

#### Scenario: Old rows are pruned

- GIVEN rows older than 30 days
- WHEN the hourly cleanup runs
- THEN those rows are deleted and newer rows are retained

### Requirement: Additive table schema

The system MUST create `gateway_history` via `CREATE TABLE IF NOT EXISTS`, leaving existing tables untouched.

#### Scenario: Idempotent creation

- GIVEN an existing database
- WHEN the table is created
- THEN `gateway_history` is created if absent without disturbing existing tables
