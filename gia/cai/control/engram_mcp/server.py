"""
Engram MCP — Memoria persistente para Carlos Code.

Servicio ligero de memoria que guarda decisiones, planes, convenciones
y descubrimientos entre sesiones. Usa SQLite como cache/fallback y
Supabase (asyncpg) como primary store.

Uso:
  python server.py
  # o
  uvicorn server:app --host 0.0.0.0 --port 8003

Endpoints:
  POST /save       — guardar una observación (dual-write: Supabase + sqlite)
  POST /import     — importar observaciones idempotente (upsert/dedup)
  GET  /search     — buscar observaciones
  GET  /get/{id}   — obtener observación completa
  GET  /recent     — observaciones recientes
  DELETE /delete/{id} — eliminar observación
  GET  /health     — health check
  GET  /stats      — estadísticas
"""

import os
import json
import time
import sqlite3
import logging
import asyncio
import re
import socket
from typing import Optional
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI, HTTPException, Query
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from pydantic import BaseModel

_start_time = time.time()

# ── Config ──────────────────────────────────────────────────────────────────

DB_PATH = os.getenv("ENGRAM_DB_PATH", "engram.db")
ADMIN_TOKEN = os.getenv("ADMIN_TOKEN", "")

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("engram-mcp")

_db: Optional[sqlite3.Connection] = None
_supabase_pool = None
_supabase_ready = False


# ── Supabase pool (asyncpg, proven params from db.py) ──────────────────────

def _ipv4(host: str) -> str:
    """Fuerza IPv4: asyncpg a veces intenta IPv6 y la red responde
    'Network is unreachable' sin reintentar IPv4."""
    try:
        return socket.getaddrinfo(host, None, socket.AF_INET)[0][4][0]
    except Exception:
        return host


async def _init_supabase():
    """Conecta a Supabase via DATABASE_URL con params probados."""
    global _supabase_pool, _supabase_ready
    url = os.environ.get("DATABASE_URL")
    if not url:
        log.info("[engram] DATABASE_URL no seteado: modo solo-sqlite")
        return
    m = re.match(r"^postgres(?:ql)?://([^:@/]+):(.*)@([^:/]+):(\d+)/(.+)$", url)
    if not m:
        log.warning("[engram] DATABASE_URL mal formado: modo solo-sqlite")
        return
    try:
        _supabase_pool = await asyncio.wait_for(
            asyncpg.create_pool(
                host=_ipv4(m.group(3)),
                port=int(m.group(4)),
                user=m.group(1),
                password=m.group(2),
                database=m.group(5),
                ssl="require",
                min_size=1,
                max_size=3,
                statement_cache_size=0,
            ),
            timeout=15,
        )
        # Create table
        async with _supabase_pool.acquire() as conn:
            await conn.execute("""
                CREATE TABLE IF NOT EXISTS engram_observations (
                    id TEXT PRIMARY KEY,
                    title TEXT,
                    type TEXT,
                    topic TEXT,
                    content TEXT,
                    tags TEXT,
                    created_at TEXT,
                    updated_at TEXT
                )
            """)
        _supabase_ready = True
        log.info("[engram] Supabase conectado + tabla engram_observations lista")
    except Exception as e:
        log.warning(f"[engram] Supabase init falló: {e} — modo solo-sqlite")
        _supabase_pool = None
        _supabase_ready = False


# ── Boot load: Supabase → sqlite when empty ────────────────────────────────

async def _boot_load_from_supabase():
    """Si sqlite está vacío, carga observaciones desde Supabase."""
    global _db
    if not _supabase_ready or not _db:
        return
    count = _db.execute("SELECT COUNT(*) FROM observations").fetchone()[0]
    if count > 0:
        log.info(f"[engram] sqlite tiene {count} obs — skip boot-load")
        return
    try:
        async with _supabase_pool.acquire() as conn:
            rows = await conn.fetch("SELECT * FROM engram_observations ORDER BY id")
        if not rows:
            log.info("[engram] Supabase vacía — nada que cargar")
            return
        for row in rows:
            _db.execute(
                """INSERT OR IGNORE INTO observations
                   (id, title, type, topic, content, tags, created_at, updated_at)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                (int(row["id"]) if row["id"].isdigit() else row["id"],
                 row["title"], row["type"], row["topic"], row["content"],
                 row["tags"], row["created_at"], row["updated_at"]),
            )
        _db.commit()
        log.info(f"[engram] Boot-loaded {len(rows)} obs from Supabase → sqlite")
    except Exception as e:
        log.warning(f"[engram] Boot-load falló: {e}")


# ── SQLite init ─────────────────────────────────────────────────────────────

def _init_db():
    """Inicializa la base de datos SQLite."""
    global _db
    _db = sqlite3.connect(DB_PATH, check_same_thread=False)
    _db.row_factory = sqlite3.Row
    _db.execute("""
        CREATE TABLE IF NOT EXISTS observations (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            title TEXT NOT NULL,
            type TEXT DEFAULT 'manual',
            topic TEXT DEFAULT '',
            content TEXT NOT NULL,
            tags TEXT DEFAULT '[]',
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )
    """)
    _db.execute("""
        CREATE INDEX IF NOT EXISTS idx_topic ON observations(topic)
    """)
    _db.execute("""
        CREATE INDEX IF NOT EXISTS idx_type ON observations(type)
    """)
    _db.commit()
    log.info(f"[engram] SQLite init: {DB_PATH}")


# ── Dual-write helpers ─────────────────────────────────────────────────────

async def _supabase_save(obs_id: str, title: str, otype: str, topic: str,
                         content: str, tags: str, created: str, updated: str):
    """Upsert to Supabase engram_observations. Fire-and-forget."""
    if not _supabase_ready or not _supabase_pool:
        return
    try:
        async with _supabase_pool.acquire() as conn:
            await conn.execute("""
                INSERT INTO engram_observations (id, title, type, topic, content, tags, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                ON CONFLICT (id) DO UPDATE SET
                    title = EXCLUDED.title, type = EXCLUDED.type, topic = EXCLUDED.topic,
                    content = EXCLUDED.content, tags = EXCLUDED.tags, updated_at = EXCLUDED.updated_at
            """, str(obs_id), title, otype, topic, content, tags, created, updated)
    except Exception as e:
        log.warning(f"[engram] Supabase save falló (fail-open): {e}")


async def _supabase_delete(obs_id: str):
    """Delete from Supabase. Fire-and-forget."""
    if not _supabase_ready or not _supabase_pool:
        return
    try:
        async with _supabase_pool.acquire() as conn:
            await conn.execute("DELETE FROM engram_observations WHERE id = $1", str(obs_id))
    except Exception as e:
        log.warning(f"[engram] Supabase delete falló (fail-open): {e}")


# ── Lifespan ────────────────────────────────────────────────────────────────

@asynccontextmanager
async def lifespan(app: FastAPI):
    _init_db()
    await _init_supabase()
    await _boot_load_from_supabase()
    # Seed from bundled observations.json if sqlite is still empty
    await _seed_from_bundle()
    yield
    if _db:
        _db.close()
    if _supabase_pool:
        await _supabase_pool.close()


app = FastAPI(title="Engram MCP", lifespan=lifespan)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)


# ── Modelos ─────────────────────────────────────────────────────────────────

class SaveRequest(BaseModel):
    title: str
    type: str = "manual"
    topic: str = ""
    content: str
    tags: list[str] = []


class UpdateRequest(BaseModel):
    title: Optional[str] = None
    type: Optional[str] = None
    topic: Optional[str] = None
    content: Optional[str] = None
    tags: Optional[list[str]] = None


class ImportRequest(BaseModel):
    observations: list[dict]


# ── Health ──────────────────────────────────────────────────────────────────

@app.get("/health")
async def health():
    count = _db.execute("SELECT COUNT(*) FROM observations").fetchone()[0] if _db else 0
    return {
        "status": "ok",
        "db_path": DB_PATH,
        "observations": count,
        "supabase": _supabase_ready,
    }


@app.get("/metrics")
async def metrics():
    """Return RAM usage and uptime for this service."""
    import resource
    usage = resource.getrusage(resource.RUSAGE_SELF)
    return {
        "ram_mb": round(usage.ru_maxrss / 1024, 1),
        "cpu_user": round(usage.ru_utime, 2),
        "cpu_system": round(usage.ru_stime, 2),
        "uptime_sec": round(time.time() - _start_time, 0),
    }


# ── Save (dual-write: Supabase primary + sqlite cache) ─────────────────────

@app.post("/save")
async def save_observation(req: SaveRequest):
    """Guarda una observación nueva. Dual-write: Supabase primary + sqlite cache."""
    now = time.strftime("%Y-%m-%d %H:%M:%S")
    cursor = _db.execute(
        "INSERT INTO observations (title, type, topic, content, tags, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
        (req.title, req.type, req.topic, req.content, json.dumps(req.tags), now, now),
    )
    _db.commit()
    obs_id = cursor.lastrowid
    log.info(f"[engram] sqlite save: #{obs_id} — {req.title}")

    # Fire-and-forget to Supabase
    await _supabase_save(
        str(obs_id), req.title, req.type, req.topic,
        req.content, json.dumps(req.tags), now, now,
    )

    return {"id": obs_id, "status": "ok"}


# ── Import (idempotent: upsert by id, dedup by normalized title+content) ───

@app.post("/import")
async def import_observations(req: ImportRequest):
    """Importa observaciones de forma idempotente.
    Upsert por id cuando está presente, dedup por normalized title+content."""
    imported = 0
    skipped = 0
    for obs in req.observations:
        obs_id = str(obs.get("id", ""))
        title = obs.get("title", "")
        otype = obs.get("type", "manual")
        topic = obs.get("topic", "")
        content = obs.get("content", "")
        tags = json.dumps(obs.get("tags", [])) if isinstance(obs.get("tags"), list) else obs.get("tags", "[]")
        created = obs.get("created_at") or obs.get("created", time.strftime("%Y-%m-%d %H:%M:%S"))
        updated = obs.get("updated_at", created)

        if obs_id:
            # Upsert by id
            existing = _db.execute("SELECT id FROM observations WHERE id = ?", (obs_id,)).fetchone()
            if existing:
                # Update existing
                _db.execute(
                    "UPDATE observations SET title=?, type=?, topic=?, content=?, tags=?, updated_at=? WHERE id=?",
                    (title, otype, topic, content, tags, updated, obs_id),
                )
                skipped += 1
            else:
                # Insert with specific id
                _db.execute(
                    "INSERT INTO observations (id, title, type, topic, content, tags, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                    (obs_id, title, otype, topic, content, tags, created, updated),
                )
                imported += 1
        else:
            # Dedup by normalized title+content
            norm = (title.strip().lower() + "|" + content.strip().lower())
            existing = _db.execute("SELECT id FROM observations").fetchall()
            dup = False
            for row in existing:
                r = _db.execute("SELECT title, content FROM observations WHERE id=?", (row["id"],)).fetchone()
                if r:
                    check = (r["title"].strip().lower() + "|" + r["content"].strip().lower())
                    if check == norm:
                        dup = True
                        break
            if dup:
                skipped += 1
            else:
                cursor = _db.execute(
                    "INSERT INTO observations (title, type, topic, content, tags, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
                    (title, otype, topic, content, tags, created, updated),
                )
                obs_id = str(cursor.lastrowid)
                imported += 1

        # Dual-write to Supabase (fire-and-forget)
        if obs_id:
            await _supabase_save(obs_id, title, otype, topic, content, tags, created, updated)

    _db.commit()
    log.info(f"[engram] Import: {imported} imported, {skipped} skipped (dedup)")
    return {"imported": imported, "skipped": skipped, "status": "ok"}


# ── Seed from bundled observations.json ─────────────────────────────────────

async def _seed_from_bundle():
    """One-time import of bundled observations.json if sqlite is still empty."""
    if not _db:
        return
    count = _db.execute("SELECT COUNT(*) FROM observations").fetchone()[0]
    if count > 0:
        return
    bundle_path = Path(__file__).parent / "observations.json"
    if not bundle_path.exists():
        bundle_path = Path("/app/observations.json")
    if not bundle_path.exists():
        log.info("[engram] No bundled observations.json found")
        return
    try:
        data = json.loads(bundle_path.read_text(encoding="utf-8"))
        obs_list = data.get("observations", [])
        if not obs_list:
            return
        req = ImportRequest(observations=obs_list)
        result = await import_observations(req)
        log.info(f"[engram] Seed from bundle: {result}")
    except Exception as e:
        log.warning(f"[engram] Seed from bundle failed: {e}")


# ── Search ──────────────────────────────────────────────────────────────────

@app.get("/search")
async def search(
    q: str = Query("", description="Texto de búsqueda"),
    type: Optional[str] = Query(None, description="Filtrar por tipo"),
    topic: Optional[str] = Query(None, description="Filtrar por topic"),
    limit: int = Query(20, ge=1, le=100),
):
    """Busca observaciones por texto, tipo o topic."""
    query = "SELECT * FROM observations WHERE 1=1"
    params = []

    if q:
        query += " AND (title LIKE ? OR content LIKE ?)"
        params.extend([f"%{q}%", f"%{q}%"])

    if type:
        query += " AND type = ?"
        params.append(type)

    if topic:
        query += " AND topic = ?"
        params.append(topic)

    query += " ORDER BY created_at DESC LIMIT ?"
    params.append(limit)

    rows = _db.execute(query, params).fetchall()
    return {
        "count": len(rows),
        "observations": [dict(r) for r in rows],
    }


# ── Get ─────────────────────────────────────────────────────────────────────

@app.get("/get/{obs_id}")
async def get_observation(obs_id: int):
    """Obtiene una observación por ID."""
    row = _db.execute("SELECT * FROM observations WHERE id = ?", (obs_id,)).fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="Observación no encontrada")
    return dict(row)


# ── Recent ──────────────────────────────────────────────────────────────────

@app.get("/recent")
async def recent(limit: int = Query(10, ge=1, le=50)):
    """Obtiene las observaciones más recientes."""
    rows = _db.execute(
        "SELECT * FROM observations ORDER BY created_at DESC LIMIT ?", (limit,)
    ).fetchall()
    return {"observations": [dict(r) for r in rows]}


# ── Update ──────────────────────────────────────────────────────────────────

@app.put("/update/{obs_id}")
async def update_observation(obs_id: int, req: UpdateRequest):
    """Actualiza una observación existente."""
    row = _db.execute("SELECT * FROM observations WHERE id = ?", (obs_id,)).fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="Observación no encontrada")

    updates = []
    params = []
    for field in ["title", "type", "topic", "content"]:
        val = getattr(req, field)
        if val is not None:
            updates.append(f"{field} = ?")
            params.append(val)
    if req.tags is not None:
        updates.append("tags = ?")
        params.append(json.dumps(req.tags))

    if updates:
        updates.append("updated_at = ?")
        params.append(time.strftime("%Y-%m-%d %H:%M:%S"))
        params.append(obs_id)
        _db.execute(f"UPDATE observations SET {', '.join(updates)} WHERE id = ?", params)
        _db.commit()
        log.info(f"[engram] sqlite update: #{obs_id}")

        # Fire-and-forget to Supabase
        row = _db.execute("SELECT * FROM observations WHERE id = ?", (obs_id,)).fetchone()
        if row:
            await _supabase_save(
                str(obs_id), row["title"], row["type"], row["topic"],
                row["content"], row["tags"], row["created_at"], row["updated_at"],
            )

    return {"status": "ok"}


# ── Delete ──────────────────────────────────────────────────────────────────

@app.delete("/delete/{obs_id}")
async def delete_observation(obs_id: int):
    """Elimina una observación."""
    row = _db.execute("SELECT * FROM observations WHERE id = ?", (obs_id,)).fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="Observación no encontrada")

    _db.execute("DELETE FROM observations WHERE id = ?", (obs_id,))
    _db.commit()
    log.info(f"[engram] sqlite delete: #{obs_id}")

    # Fire-and-forget to Supabase
    await _supabase_delete(str(obs_id))

    return {"status": "ok"}


# ── Stats ───────────────────────────────────────────────────────────────────

@app.get("/stats")
async def stats():
    """Estadísticas de la memoria."""
    total = _db.execute("SELECT COUNT(*) FROM observations").fetchone()[0]
    by_type = _db.execute(
        "SELECT type, COUNT(*) as count FROM observations GROUP BY type"
    ).fetchall()
    by_topic = _db.execute(
        "SELECT topic, COUNT(*) as count FROM observations WHERE topic != '' GROUP BY topic"
    ).fetchall()

    return {
        "total": total,
        "by_type": {r["type"]: r["count"] for r in by_type},
        "by_topic": {r["topic"]: r["count"] for r in by_topic},
    }


# ── Main ────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    import uvicorn

    port = int(os.getenv("PORT", "8003"))
    uvicorn.run(app, host="0.0.0.0", port=port)
