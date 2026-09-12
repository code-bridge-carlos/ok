# services-monitor Specification

## Purpose

Complete and make reliable the services view and keepalive control. The service inventory covers C1–C6 plus a "Supabase DB" entry, each card gains per-service URL chips opening in new tabs, and all 7 targets are pinged every 5 minutes (GET `/health` for HTTP services, `SELECT 1` for Supabase). C1 gains a `GET /health` route. The keepalive toggle becomes reliable: a `.cai_keepalive.json` file fallback plus boot load, 30 s polling of `/api/keepalive`, an optimistic toggle with revert and toast, and removal of dead WS broadcast and inline `renderServices`. (proposal §4, §5, §6, §7)

## Requirements

### Requirement: Complete service inventory

The system MUST include C1–C6 and a "Supabase DB" entry in the services inventory, with a health check for each: GET `/health` for HTTP services and `SELECT 1` on the Supabase pool.

#### Scenario: All 7 targets reachable

- GIVEN the services view loaded
- WHEN the ping loop runs every 5 minutes
- THEN the 7 targets (C1–C6 + Supabase) are checked and their status updated

### Requirement: Per-service URL chips

The system MUST render each service card's `urls: [{label, url}]` as chips that open in a new tab, while preserving the primary card-click behavior.

#### Scenario: Open endpoint in new tab

- GIVEN a service card with multiple `urls`
- WHEN the user clicks a URL chip
- THEN the endpoint opens in a new tab without changing the primary card behavior

### Requirement: C1 /health route

The system MUST add a `GET /health` route to C1 and use `healthUrl https://bridgecarlos.onrender.com/health` in the C1 card.

#### Scenario: C1 health check succeeds

- GIVEN the C1 service is up
- WHEN the ping loop calls GET `/health`
- THEN a 200 response marks the C1 card active

### Requirement: Keepalive file fallback and boot load

The system MUST write the keepalive toggle to `.cai_keepalive.json` and, at boot, load the DB value first and the file second, reviving the previous state without a manual reload.

#### Scenario: Keepalive state survives redeploy

- GIVEN keepalive was left ON
- WHEN C1 boots with the file present (and DB absent/missing the flag)
- THEN the toggle restores to ON from the file fallback

### Requirement: Keepalive 30 s polling

The system MUST poll `/api/keepalive` every 30 s in `services.js` so the label and status box refresh without reload.

#### Scenario: Label self-heals via polling

- GIVEN keepalive toggled elsewhere or DB value changes
- WHEN 30 s poll returns the new value
- THEN the label and status box update without a page reload

### Requirement: Optimistic keepalive toggle

The system MUST flip the toggle immediately on click, revert on failure, and show a toast, removing the dead WS broadcast and dead inline `renderServices`.

#### Scenario: Optimistic flip succeeds

- GIVEN the user clicks the keepalive toggle
- WHEN the request succeeds
- THEN the UI reflects the new state immediately with a toast

#### Scenario: Toggle reverts on failure

- GIVEN the keepalive update request fails
- WHEN the user clicks the toggle
- THEN the UI reverts to the prior state and shows an error toast

### Requirement: Single source of truth for services rendering

The system MUST render services only via `services.js` (`refreshServiceStatuses` + `serviceStatusCache`), removing dead code paths ("En desarrollo" branch and inline `renderServices`).

#### Scenario: Dead code removed, live health is truth

- GIVEN the panel loaded
- WHEN the services view renders
- THEN the live health check drives status and the removed dead branches are absent
