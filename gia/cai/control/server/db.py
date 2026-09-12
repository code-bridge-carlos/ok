"""Capa de persistencia en Supabase/Postgres.

El server mantiene las conexiones WebSocket en vivo en memoria (no se pueden
persistir), pero el estado durable (workers, tareas, subtareas, eventos) se
espeja en Supabase via DATABASE_URL. Si DATABASE_URL no esta seteado, el server
funciona igual en memoria (degradacion elegante).
"""
import os
import json
import asyncio
import re
import socket
import asyncpg

# File fallback for keepalive state (design:48-54)
_KEEPALIVE_FILE = os.environ.get("KEEPALIVE_FILE") or os.path.join(os.getcwd(), ".cai_keepalive.json")


def _ipv4(host: str) -> str:
    """Fuerza IPv4: asyncpg a veces intenta IPv6 y la red responde
    'Network is unreachable' sin reintentar IPv4."""
    try:
        return socket.getaddrinfo(host, None, socket.AF_INET)[0][4][0]
    except Exception:  # noqa: BLE001
        return host
_pool = None
last_error = None  # ultimo error de conexion a BD (para diagnostico en el panel)
db_stage = "init"  # no-url | parse-fail | except | connected


# Fallback de persistencia en disco cuando no hay DATABASE_URL (Render free spin-down
# preserva el disco de la instancia, asi que las tareas sobreviven a las pausas; solo se
# pierden en un deploy nuevo). Esto evita que el historial se borre "solo".
STORE_PATH = os.environ.get("TASK_STORE") or os.path.join(os.getcwd(), ".cai_tasks.json")
MEM = {}


def _load_mem() -> None:
    global MEM
    try:
        if os.path.exists(STORE_PATH):
            with open(STORE_PATH, "r", encoding="utf-8") as f:
                MEM = json.load(f) or {}
    except Exception:  # noqa: BLE001
        MEM = {}


def _dump_mem() -> None:
    try:
        with open(STORE_PATH, "w", encoding="utf-8") as f:
            json.dump(MEM, f)
    except Exception:  # noqa: BLE001
        pass


async def _create_tables(pool) -> None:
    async with pool.acquire() as c:
        await c.execute("""
        CREATE TABLE IF NOT EXISTS workers (
            id TEXT PRIMARY KEY, name TEXT, perf INT DEFAULT 5, weight INT DEFAULT 10,
            brand TEXT, model TEXT, os TEXT, status TEXT, stats_json TEXT,
            updated_at TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS tasks (
            id TEXT PRIMARY KEY, prompt TEXT, status TEXT, mode_used TEXT,
            plan_json TEXT, created_at TEXT, finished_at TEXT
        );
        CREATE TABLE IF NOT EXISTS subtasks (
            id TEXT PRIMARY KEY, task_id TEXT, worker TEXT, file TEXT,
            files_json TEXT, status TEXT, output TEXT, updated_at TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS events (
            id SERIAL PRIMARY KEY, ts TIMESTAMPTZ DEFAULT now(), msg TEXT
        );
        CREATE TABLE IF NOT EXISTS keepalive_state (
            id TEXT PRIMARY KEY, enabled BOOLEAN NOT NULL DEFAULT false,
            last_run TEXT, services_json TEXT, updated_at TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS worker_memory (
            id TEXT PRIMARY KEY, snapshot_json TEXT, last_seen TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS panel_state (
            id TEXT PRIMARY KEY, snapshot_json TEXT, updated_at TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS gateway_history (
            id BIGSERIAL PRIMARY KEY, path TEXT, prompt TEXT, response_snippet TEXT,
            status INT, latency_ms INT, ts TIMESTAMPTZ DEFAULT now()
        );
        CREATE INDEX IF NOT EXISTS idx_gw_ts ON gateway_history(ts);
        CREATE TABLE IF NOT EXISTS carlos_code_history (
            id BIGSERIAL PRIMARY KEY, prompt TEXT, plan_summary TEXT, mode TEXT,
            workers TEXT, status TEXT, ts TIMESTAMPTZ DEFAULT now()
        );
        CREATE INDEX IF NOT EXISTS idx_cc_ts ON carlos_code_history(ts);
        CREATE TABLE IF NOT EXISTS carlos_sections (
            id TEXT PRIMARY KEY, title TEXT, messages_json TEXT, tokens INT,
            updated_at TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS custom_services (
            id TEXT PRIMARY KEY, name TEXT, health_url TEXT, dashboard_url TEXT,
            color TEXT, created_at TIMESTAMPTZ DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS service_tables (
            service_id TEXT, key TEXT, value TEXT,
            updated_at TIMESTAMPTZ DEFAULT now(),
            PRIMARY KEY (service_id, key)
        );
        """)


async def init_db() -> bool:
    global _pool, last_error, db_stage
    url = os.environ.get("DATABASE_URL")
    if not url:
        print("[db] DATABASE_URL no seteado: fallback a archivo local")
        db_stage = "no-url"
        last_error = "DATABASE_URL no seteado"
        _load_mem()
        return False
    # parseo manual: urllib.parse falla con passwords que contienen '[' ']'
    m = re.match(r"^postgres(?:ql)?://([^:@/]+):(.*)@([^:/]+):(\d+)/(.+)$", url)
    if not m:
        print("[db] DATABASE_URL mal formado")
        db_stage = "parse-fail"
        last_error = "DATABASE_URL mal formado"
        return False
    try:
        _pool = await asyncpg.create_pool(
            host=_ipv4(m.group(3)),
            port=int(m.group(4)),
            user=m.group(1),
            password=m.group(2),
            database=m.group(5),
            ssl="require",
            min_size=1,
            max_size=5,
            statement_cache_size=0,  # compatible con pgbouncer (pooler Supabase)
        )
    except Exception as e:  # noqa: BLE001
        print(f"[db] init fallo: {e}")
        last_error = str(e)
        db_stage = "except"
        _pool = None
        _load_mem()  # fallback a archivo local si la BD no conecta
        return False

    # exito: crear tablas
    await _create_tables(_pool)
    db_stage = "connected"
    print("[db] conectado a Supabase y tablas listas")
    _load_mem()
    return True


async def retry_init() -> bool:
    """Reintenta conectar a la BD si la conexion inicial fallo (p.ej. Supabase
    pausado o red temporal). Devuelve True si ahora hay pool."""
    global _pool, last_error
    if _pool is not None:
        return True
    url = os.environ.get("DATABASE_URL")
    if not url:
        return False
    m = re.match(r"^postgres(?:ql)?://([^:@/]+):(.*)@([^:/]+):(\d+)/(.+)$", url)
    if not m:
        last_error = "DATABASE_URL mal formado"
        return False
    try:
        _pool = await asyncpg.create_pool(
            host=_ipv4(m.group(3)), port=int(m.group(4)), user=m.group(1),
            password=m.group(2), database=m.group(5),             ssl="require",
            min_size=1, max_size=5, statement_cache_size=0,
        )
        last_error = None
        await _create_tables(_pool)
        print("[db] reconexion exitosa a Supabase")
        return True
    except Exception as e:  # noqa: BLE001
        last_error = str(e)
        _pool = None
        return False


def get_last_error() -> str:
    return last_error or ""


def get_stage() -> str:
    return db_stage


def has_db_url() -> bool:
    return bool(os.environ.get("DATABASE_URL"))


def _ok() -> bool:
    return _pool is not None


async def save_worker(wid: str, w: dict) -> None:
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO workers (id,name,perf,weight,brand,model,os,status,stats_json,updated_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
            ON CONFLICT (id) DO UPDATE SET
                name=$2,perf=$3,weight=$4,brand=$5,model=$6,os=$7,status=$8,stats_json=$9,updated_at=now()
            """, wid, w.get("name", ""), int(w.get("perf", 5)), int(w.get("weight", 10)),
            w.get("brand", ""), w.get("model", ""), w.get("os", ""),
            w.get("status", "disponible"), json.dumps(w.get("stats", {})),
            )
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_worker {wid}: {e}")


async def save_task(t: dict) -> None:
    # Siempre espejar a disco como safety-net (sobrevive crash del pool o restart)
    MEM[t["id"]] = t
    _dump_mem()
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO tasks (id,prompt,status,mode_used,plan_json,created_at,finished_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7)
            ON CONFLICT (id) DO UPDATE SET
                status=$3, mode_used=$4, plan_json=$5, finished_at=$7
            """, t["id"], t.get("prompt", ""), t.get("status", ""), t.get("mode_used"),
            json.dumps(t.get("plan")), t.get("created_at", ""), t.get("finished_at"),
            )
            for sid, s in (t.get("subtasks") or {}).items():
                await c.execute("""
                INSERT INTO subtasks (id,task_id,worker,file,files_json,status,output,updated_at)
                VALUES ($1,$2,$3,$4,$5,$6,$7,now())
                ON CONFLICT (id) DO UPDATE SET
                    worker=$3, file=$4, files_json=$5, status=$6, output=$7, updated_at=now()
                """, sid, t["id"], s.get("worker", ""), s.get("file", ""),
                json.dumps(s.get("files", [])), s.get("status", ""), s.get("output", ""),
                )
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_task {t.get('id')}: {e} (fallback a disco ya persistió)")


async def log_event(msg: str) -> None:
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("INSERT INTO events (msg) VALUES ($1)", msg)
    except Exception as e:  # noqa: BLE001
        print(f"[db] log_event: {e}")


async def load_tasks() -> dict:
    if not _ok():
        return dict(MEM)
    out = {}
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch("SELECT * FROM tasks")
            for r in rows:
                out[r["id"]] = {
                    "id": r["id"], "prompt": r["prompt"], "status": r["status"],
                    "plan": json.loads(r["plan_json"] or "null"), "plan_summary": "",
                    "subtasks": {}, "queue": [], "reserved": [], "items": {},
                    "pqueues": {}, "active": {}, "recalling": {}, "remaining": [],
                    "mode_used": r["mode_used"], "created_at": r["created_at"],
                    "finished_at": r["finished_at"],
                }
            subs = await c.fetch("SELECT * FROM subtasks")
            for s in subs:
                t = out.get(s["task_id"])
                if t:
                    t["subtasks"][s["id"]] = {
                        "worker": s["worker"], "file": s["file"],
                        "files": json.loads(s["files_json"] or "[]"),
                        "status": s["status"], "output": s["output"],
                    }
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_tasks: {e}")
    return out


async def delete_task(tid: str) -> None:
    MEM.pop(tid, None)
    _dump_mem()
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM subtasks WHERE task_id=$1", tid)
            await c.execute("DELETE FROM tasks WHERE id=$1", tid)
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_task {tid}: {e}")


async def clear_tasks() -> None:
    MEM.clear()
    _dump_mem()
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM subtasks")
            await c.execute("DELETE FROM tasks")
    except Exception as e:  # noqa: BLE001
        print(f"[db] clear_tasks: {e}")


async def save_keepalive(enabled: bool, last_run: str = "", services: dict | None = None) -> None:
    """Persiste el estado del keep-alive (toggle + último heartbeat)."""
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO keepalive_state (id,enabled,last_run,services_json,updated_at)
            VALUES ('default',$1,$2,$3,now())
            ON CONFLICT (id) DO UPDATE SET
                enabled=$1, last_run=$2, services_json=$3, updated_at=now()
            """, enabled, last_run, json.dumps(services or {}))
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_keepalive: {e}")


async def load_keepalive() -> dict:
    """Devuelve {enabled, last_run, services} persistido, o defaults."""
    if not _ok():
        return {"enabled": False, "last_run": "", "services": {}}
    try:
        async with _pool.acquire() as c:
            row = await c.fetchrow("SELECT * FROM keepalive_state WHERE id='default'")
            if not row:
                return {"enabled": False, "last_run": "", "services": {}}
            return {
                "enabled": bool(row["enabled"]),
                "last_run": row["last_run"] or "",
                "services": json.loads(row["services_json"] or "{}"),
            }
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_keepalive: {e}")
        return {"enabled": False, "last_run": "", "services": {}}


# ---------------------------------------------------------------------------
# C1 state persistence: worker_memory + panel_state
# ---------------------------------------------------------------------------
async def load_workers_from_db() -> dict:
    """Load full worker snapshots from worker_memory table. Returns {wid: dict}."""
    if not _ok():
        return {}
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch("SELECT id, snapshot_json FROM worker_memory")
            out = {}
            for r in rows:
                try:
                    out[r["id"]] = json.loads(r["snapshot_json"] or "{}")
                except Exception:
                    pass
            return out
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_workers_from_db: {e}")
        return {}


async def save_worker_full(wid: str, w: dict) -> None:
    """Persist a full worker snapshot to worker_memory (debounced caller)."""
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO worker_memory (id, snapshot_json, last_seen)
            VALUES ($1, $2, now())
            ON CONFLICT (id) DO UPDATE SET snapshot_json=$2, last_seen=now()
            """, wid, json.dumps(w))
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_worker_full {wid}: {e}")


async def save_worker_memory_snapshot(wid: str, snapshot: dict) -> None:
    """Save worker_memory entry (called from _remember in server.py)."""
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO worker_memory (id, snapshot_json, last_seen)
            VALUES ($1, $2, now())
            ON CONFLICT (id) DO UPDATE SET snapshot_json=$2, last_seen=now()
            """, wid, json.dumps(snapshot))
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_worker_memory_snapshot {wid}: {e}")


async def save_panel_state(snapshot: dict) -> None:
    """Persist panel state (principal_id, mode, paused, current_task) as single row."""
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO panel_state (id, snapshot_json, updated_at)
            VALUES ('default', $1, now())
            ON CONFLICT (id) DO UPDATE SET snapshot_json=$1, updated_at=now()
            """, json.dumps(snapshot))
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_panel_state: {e}")


async def load_panel_state() -> dict:
    """Load panel state from DB. Returns {principal_id, mode, paused, current_task} or empty dict."""
    if not _ok():
        return {}
    try:
        async with _pool.acquire() as c:
            row = await c.fetchrow("SELECT snapshot_json FROM panel_state WHERE id='default'")
            if not row:
                return {}
            return json.loads(row["snapshot_json"] or "{}")
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_panel_state: {e}")
        return {}


# ---------------------------------------------------------------------------
# History CRUD: gateway_history + carlos_code_history
# ---------------------------------------------------------------------------
async def load_gateway_history(limit: int = 50) -> list:
    if not _ok():
        return []
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch(
                "SELECT id, path, prompt, response_snippet, status, latency_ms, ts "
                "FROM gateway_history ORDER BY ts DESC LIMIT $1", limit)
            return [dict(r) for r in rows]
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_gateway_history: {e}")
        return []


async def delete_gateway_history(row_id: int) -> bool:
    if not _ok():
        return False
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM gateway_history WHERE id=$1", row_id)
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_gateway_history {row_id}: {e}")
        return False


async def delete_gateway_history_all() -> bool:
    if not _ok():
        return False
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM gateway_history")
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_gateway_history_all: {e}")
        return False


async def load_carlos_history(limit: int = 50) -> list:
    if not _ok():
        return []
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch(
                "SELECT id, prompt, plan_summary, mode, workers, status, ts "
                "FROM carlos_code_history ORDER BY ts DESC LIMIT $1", limit)
            return [dict(r) for r in rows]
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_carlos_history: {e}")
        return []


async def delete_carlos_history(row_id: int) -> bool:
    if not _ok():
        return False
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM carlos_code_history WHERE id=$1", row_id)
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_carlos_history {row_id}: {e}")
        return False


async def delete_carlos_history_all() -> bool:
    if not _ok():
        return False
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM carlos_code_history")
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_carlos_history_all: {e}")
        return False


async def save_gateway_history(entry: dict) -> bool:
    """Guardar una nueva entrada en gateway_history.
    entry debe tener: path, prompt, response_snippet, status, latency_ms, ts
    """
    if not _ok():
        return False
    try:
        async with _pool.acquire() as c:
            await c.execute(
                """INSERT INTO gateway_history (path, prompt, response_snippet, status, latency_ms, ts)
                   VALUES ($1, $2, $3, $4, $5, $6)""",
                entry.get("path"),
                entry.get("prompt"),
                entry.get("response_snippet"),
                entry.get("status"),
                entry.get("latency_ms"),
                entry.get("ts"),
            )
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_gateway_history: {e}")
        return False


async def save_carlos_history(entry: dict) -> bool:
    """Guardar una nueva entrada en carlos_code_history.
    entry debe tener: prompt, plan_summary, mode, workers, status, ts
    """
    if not _ok():
        return False
    try:
        async with _pool.acquire() as c:
            await c.execute(
                """INSERT INTO carlos_code_history (prompt, plan_summary, mode, workers, status, ts)
                   VALUES ($1, $2, $3, $4, $5, $6)""",
                entry.get("prompt"),
                entry.get("plan_summary"),
                entry.get("mode"),
                entry.get("workers"),
                entry.get("status"),
                entry.get("ts"),
            )
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_carlos_history: {e}")
        return False


async def cleanup_old_history() -> None:
    """Delete history rows older than 30 days (hourly retention cleanup)."""
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM gateway_history WHERE ts < now() - interval '30 days'")
            await c.execute("DELETE FROM carlos_code_history WHERE ts < now() - interval '30 days'")
    except Exception as e:  # noqa: BLE001
        print(f"[db] cleanup_old_history: {e}")


# ---------------------------------------------------------------------------
# F2 conversation sections: carlos_sections (Supabase + MEM/disk fallback)
# ---------------------------------------------------------------------------
def _sections_mem() -> dict:
    """RAM/disk mirror for sections, namespaced inside MEM."""
    secs = MEM.get("carlos_sections")
    if not isinstance(secs, dict):
        secs = {}
        MEM["carlos_sections"] = secs
    return secs


async def load_sections() -> dict:
    """Return {section_id: {id, title, messages, tokens, updated_at}}.

    Without DB, serves the MEM/disk mirror (same fallback as tasks).
    With DB, Supabase rows win, overlaid with any MEM-only entries.
    """
    mem_secs = dict(_sections_mem())
    if not _ok():
        return mem_secs
    out = {}
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch(
                "SELECT id, title, messages_json, tokens, updated_at FROM carlos_sections")
            for r in rows:
                try:
                    msgs = json.loads(r["messages_json"] or "[]")
                except Exception:
                    msgs = []
                out[r["id"]] = {
                    "id": r["id"], "title": r["title"] or "",
                    "messages": msgs, "tokens": r["tokens"] or 0,
                    "updated_at": str(r["updated_at"]) if r["updated_at"] else "",
                }
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_sections: {e}")
        return mem_secs
    for sid, sec in mem_secs.items():
        out.setdefault(sid, sec)
    return out


async def save_section(sec: dict) -> None:
    """Upsert one section. Always mirrors to MEM/disk as safety-net."""
    sid = (sec or {}).get("id")
    if not sid:
        return
    entry = {
        "id": sid, "title": sec.get("title", ""),
        "messages": sec.get("messages", []), "tokens": sec.get("tokens", 0),
        "updated_at": sec.get("updated_at", ""),
    }
    _sections_mem()[sid] = entry
    _dump_mem()
    if not _ok():
        return
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO carlos_sections (id, title, messages_json, tokens, updated_at)
            VALUES ($1, $2, $3, $4, now())
            ON CONFLICT (id) DO UPDATE SET
                title=$2, messages_json=$3, tokens=$4, updated_at=now()
            """, sid, entry["title"], json.dumps(entry["messages"]), int(entry["tokens"] or 0))
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_section {sid}: {e} (fallback a disco ya persistió)")


# ---------------------------------------------------------------------------
# .cai_keepalive.json file fallback (design:48-54)
# ---------------------------------------------------------------------------
def save_keepalive_file(enabled: bool, last_run: str = "", services: dict | None = None) -> None:
    """Write keepalive state to .cai_keepalive.json as file fallback."""
    try:
        data = {"enabled": enabled, "last_run": last_run, "services": services or {}}
        with open(_KEEPALIVE_FILE, "w", encoding="utf-8") as f:
            json.dump(data, f)
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_keepalive_file: {e}")


def load_keepalive_file() -> dict | None:
    """Load keepalive state from .cai_keepalive.json. Returns None if missing."""
    try:
        if os.path.exists(_KEEPALIVE_FILE):
            with open(_KEEPALIVE_FILE, "r", encoding="utf-8") as f:
                data = json.load(f)
            return data
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_keepalive_file: {e}")
    return None


# ---------------------------------------------------------------------------
# Custom services + per-service key/value tables (Supabase + MEM/disk fallback)
# ---------------------------------------------------------------------------
def _custom_services_mem() -> dict:
    """RAM/disk mirror for custom services, namespaced inside MEM."""
    svcs = MEM.get("custom_services")
    if not isinstance(svcs, dict):
        svcs = {}
        MEM["custom_services"] = svcs
    return svcs


def _service_tables_mem() -> dict:
    """RAM/disk mirror for per-service tables: {service_id: {key: value}}."""
    tabs = MEM.get("service_tables")
    if not isinstance(tabs, dict):
        tabs = {}
        MEM["service_tables"] = tabs
    return tabs


async def load_custom_services() -> list:
    """Return [{id, name, health_url, dashboard_url, color}]. MEM fallback."""
    mem_svcs = dict(_custom_services_mem())
    if not _ok():
        return list(mem_svcs.values())
    out = {}
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch(
                "SELECT id, name, health_url, dashboard_url, color "
                "FROM custom_services ORDER BY created_at")
            for r in rows:
                out[r["id"]] = {
                    "id": r["id"], "name": r["name"] or "",
                    "health_url": r["health_url"] or "",
                    "dashboard_url": r["dashboard_url"] or "",
                    "color": r["color"] or "#64748b",
                }
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_custom_services: {e}")
        return list(mem_svcs.values())
    for sid, svc in mem_svcs.items():
        out.setdefault(sid, svc)
    return list(out.values())


async def save_custom_service(svc: dict) -> bool:
    """Upsert one custom service. Always mirrors to MEM/disk as safety-net."""
    sid = (svc or {}).get("id")
    if not sid:
        return False
    entry = {
        "id": sid, "name": svc.get("name", ""),
        "health_url": svc.get("health_url", ""),
        "dashboard_url": svc.get("dashboard_url", ""),
        "color": svc.get("color", "#64748b"),
    }
    _custom_services_mem()[sid] = entry
    _dump_mem()
    if not _ok():
        return True
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO custom_services (id, name, health_url, dashboard_url, color)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (id) DO UPDATE SET
                name=$2, health_url=$3, dashboard_url=$4, color=$5
            """, sid, entry["name"], entry["health_url"],
                entry["dashboard_url"], entry["color"])
        return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_custom_service {sid}: {e} (fallback a disco ya persistió)")
        return True


async def delete_custom_service(sid: str) -> bool:
    """Delete a custom service and all its table rows."""
    _custom_services_mem().pop(sid, None)
    _service_tables_mem().pop(sid, None)
    _dump_mem()
    if not _ok():
        return True
    try:
        async with _pool.acquire() as c:
            await c.execute("DELETE FROM service_tables WHERE service_id=$1", sid)
            await c.execute("DELETE FROM custom_services WHERE id=$1", sid)
        return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_custom_service {sid}: {e}")
        return False


async def load_service_table(service_id: str) -> list:
    """Return [{key, value}] for a service. MEM fallback."""
    mem_tab = dict(_service_tables_mem().get(service_id) or {})
    if not _ok():
        return [{"key": k, "value": v} for k, v in sorted(mem_tab.items())]
    out = {}
    try:
        async with _pool.acquire() as c:
            rows = await c.fetch(
                "SELECT key, value FROM service_tables "
                "WHERE service_id=$1 ORDER BY key", service_id)
            for r in rows:
                out[r["key"]] = r["value"] or ""
    except Exception as e:  # noqa: BLE001
        print(f"[db] load_service_table {service_id}: {e}")
        return [{"key": k, "value": v} for k, v in sorted(mem_tab.items())]
    for k, v in mem_tab.items():
        out.setdefault(k, v)
    return [{"key": k, "value": v} for k, v in sorted(out.items())]


async def save_service_row(service_id: str, key: str, value: str) -> bool:
    """Upsert one key/value row. Always mirrors to MEM/disk as safety-net."""
    key = (key or "").strip()
    if not service_id or not key:
        return False
    tab = _service_tables_mem().setdefault(service_id, {})
    tab[key] = value or ""
    _dump_mem()
    if not _ok():
        return True
    try:
        async with _pool.acquire() as c:
            await c.execute("""
            INSERT INTO service_tables (service_id, key, value, updated_at)
            VALUES ($1, $2, $3, now())
            ON CONFLICT (service_id, key) DO UPDATE SET
                value=$3, updated_at=now()
            """, service_id, key, value or "")
        return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] save_service_row {service_id}/{key}: {e} (fallback a disco ya persistió)")
        return True


async def delete_service_row(service_id: str, key: str) -> bool:
    """Delete one key/value row."""
    tab = _service_tables_mem().get(service_id)
    if isinstance(tab, dict):
        tab.pop(key, None)
        _dump_mem()
    if not _ok():
        return True
    try:
        async with _pool.acquire() as c:
            await c.execute(
                "DELETE FROM service_tables WHERE service_id=$1 AND key=$2",
                service_id, key)
        return True
    except Exception as e:  # noqa: BLE001
        print(f"[db] delete_service_row {service_id}/{key}: {e}")
        return False
