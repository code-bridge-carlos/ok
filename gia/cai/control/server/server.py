"""
Servidor de control + panel web para el ecosistema OpenCode distribuido.

Flujo de tarea:
  1. El usuario envia una tarea. El servidor la manda a la PC "principal".
  2. La principal elabora un plan y devuelve la division del trabajo.
  3. El servidor despacha subtrabajos a las PCs segun el modo:
       - "percent": la principal asigna archivos por el % de cada PC.
       - "dynamic": cola de archivos; cada PC reserva el siguiente al terminar.
  4. Cada archivo lo modifica UNA sola PC (sin conflictos).
  5. Se acumulan estadisticas (palabras/tokens) por PC.

Requiere CONTROL_TOKEN en todas las conexiones.
"""

import asyncio
import base64
import hashlib
import hmac
import httpx
import json
import logging
import os
import re
import secrets
import time
import uuid
import asyncpg
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional

from fastapi import FastAPI, WebSocket, Request, WebSocketDisconnect, Form
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse, StreamingResponse
from dotenv import load_dotenv
load_dotenv()
from . import db
from .carlos_code_integration import carlos_code_available, carlos_code_plan, carlos_code_closeout, parse_plan_assignments, CARLOS_CODE_URL

logger = logging.getLogger("c1-panel")

# C3 base URL: env CARLOS_CODE_URL or default. The browser never talks to C3
# directly; all C3 traffic goes through the /api/c3/* proxy below (same-origin
# + panel session cookie).
C3_BASE_URL = os.environ.get("CARLOS_CODE_URL") or "https://carlos-code.onrender.com"
C3_TIMEOUT_S = float(os.environ.get("CARLOS_CODE_TIMEOUT", "30") or 30)

PORT = int(os.environ.get("PORT", "8000"))
CONTROL_TOKEN = os.environ.get("CONTROL_TOKEN", "")
# El secreto de sesion debe ser ESTABLE entre reinicios, si no cada deploy invalida
# todas las cookies y el panel pide login de nuevo (y /api/task devuelve 401).
# Preferencia: env SESSION_SECRET -> CONTROL_TOKEN (estable en Render) -> default fijo.
SESSION_SECRET = os.environ.get("SESSION_SECRET") or CONTROL_TOKEN or "cai-panel-stable-secret"
PANEL_USER = os.environ.get("PANEL_USER", "cmpf")
PANEL_PASSWORD = os.environ.get("PANEL_PASSWORD", "")
SESSION_MAX_AGE = 60 * 60 * 12  # 12 horas
HEARTBEAT_INTERVAL = 10
HEARTBEAT_TIMEOUT = 90
ENGRAM_URL = os.environ.get("ENGRAM_URL", "https://engram-mcp-q9ln.onrender.com")
# Track service uptime for /metrics endpoint
start_time = time.time()  # epoch when service started
# Keep-alive: cada 5 minutos C1 hace ping/pong con los servicios para conocer
# su estado y evitar que Render los duerma (spin-down free).
KEEPALIVE_INTERVAL_SEC = 300
KEEPALIVE_TIMEOUT_SEC = 8
KEEPALIVE_SERVICES = [
    {"id": "SVC-C1", "name": "C1 - Panel Web", "url": "https://bridgecarlos.onrender.com/health", "color": "#2563eb"},
    {"id": "SVC-C2", "name": "C2 - Gateway DeepSeek", "url": "https://carlos-gateway.onrender.com/health", "color": "#7c3aed"},
    {"id": "SVC-C3", "name": "C3 - Carlos Code", "url": "https://carlos-code.onrender.com/health", "color": "#f59e0b"},
    {"id": "SVC-C4", "name": "C4 - Engram MCP", "url": "https://engram-mcp-q9ln.onrender.com/health", "color": "#ec4899"},
    {"id": "SVC-C5", "name": "C5 - Context7+grep", "url": "https://context7-grep.onrender.com/health", "color": "#06b6d4"},
    {"id": "SVC-C6", "name": "C6 - Worker Code", "url": "https://worker-code.onrender.com/health", "color": "#10b981"},
    {"id": "SVC-DB", "name": "Supabase DB", "url": "", "color": "#059669", "db_check": True},
]
KEEPALIVE_STATE = {
    "enabled": False,
    "last_run": "",
    "services": {},
}

INDEX_HTML = Path(__file__).parent / "static" / "index.html"

app = FastAPI(title="Control Web — OpenCode Distribuido")

# Serve static assets (js/services.js, etc.) referenced by index.html.
from fastapi.staticfiles import StaticFiles
STATIC_DIR = Path(__file__).parent / "static"
if STATIC_DIR.exists():
    app.mount("/js", StaticFiles(directory=STATIC_DIR / "js"), name="js")
    app.mount("/static", StaticFiles(directory=STATIC_DIR), name="static")


# ─── Endpoint: /metrics ──────────────────────────────────────────────
@app.get("/metrics")
async def api_metrics():
    """Return service RAM, goroutines, and uptime for monitoring."""
    # NOTE: workers/tasks are module globals of this same module; do NOT
    # import them (a self-import like `from .server import ...` fails once
    # the second `app = FastAPI(...)` is removed and always risked a cycle).
    import resource as resource_module
    try:
        ru = resource_module.getrusage(resource_module.RUSAGE_SELF)
        ram_mb = ru.ru_maxrss / 1024  # KB to MB (Linux)
    except Exception:
        ram_mb = 0.0
    uptime_sec = time.time() - start_time
    return {
        "ram_mb": ram_mb,
        "ram_sys": 0.0,  # no easy way without psutil
        "goroutines": len(workers) + len(tasks),
        "uptime_sec": uptime_sec,
    }

# NOTE: single FastAPI instance — do NOT re-instantiate `app` here. A second
# `app = FastAPI(...)` used to discard every route registered above (incl.
# /metrics), so uvicorn served an app where C1 /metrics 404'd.

workers: Dict[str, Dict[str, Any]] = {}
tasks: Dict[str, Dict[str, Any]] = {}
panel_sockets: set[WebSocket] = set()
principal_id: Optional[str] = None
mode: str = "percent"  # "percent" | "dynamic"
current_task: Optional[str] = None  # tarea en ejecucion ahora
# El worker NO usa WebSocket (el proxy de Render corta conexiones persistentes a ~20s y
# mata la conexion al recibir frames del cliente). El server acola comandos en `inbox`
# y el worker los drena por HTTP long-polling (GET /api/worker-poll).
inbox: Dict[str, List[Dict[str, Any]]] = {}
WORKER_TIMEOUT = int(os.environ.get("WORKER_TIMEOUT", "60"))  # sin poll en N s -> caido

# Memoria de PCs: ultimo status conocido AUNQUE se desconecten (para el Historial
# del panel). Se actualiza en cada accion del worker y se marca connected=False
# al caer por timeout. Asi el historial muestra "ultimo status conocido".
worker_memory: Dict[str, Dict[str, Any]] = {}

# Debounce: batch Supabase writes for worker state changes (design:109)
_dirty_workers: set = set()


def _remember(wid: str) -> None:
    w = workers.get(wid)
    if not w:
        return
    worker_memory[wid] = {
        "id": wid, "name": w.get("name", wid), "perf": w.get("perf", 5),
        "weight": w.get("weight", 10), "brand": w.get("brand", ""),
        "model": w.get("model", ""), "os": w.get("os", ""),
        "status": w.get("status", "desconocido"), "connected": True,
        "last_seen": now_iso(), "task_id": w.get("task_id"),
        "stats": w.get("stats", {"tasks": 0, "words": 0, "tokens": 0}),
    }
    _dirty_workers.add(wid)


def _new_worker(wid: str, **fields) -> Dict[str, Any]:
    w = {
        "ws": None, "name": wid, "perf": 5, "weight": 10,
        "brand": "", "model": "", "os": "", "status": "disponible",
        "last_pong": now_iso(), "last_poll": now_iso(), "task_id": None,
        "busy_since": None, "idle_since": None,
        "task_queue": [], "stats": {"tasks": 0, "words": 0, "tokens": 0},
    }
    w.update(fields)
    return w


def _set_status(wid: str, st: str) -> None:
    """Cambia el estado de un worker y actualiza los timestamps de ocupacion/idle.

    - 'ocupado'   -> marca busy_since (inicio del subtrabajo actual)
    - 'disponible'-> marca idle_since (inicio del periodo libre)
    """
    w = workers.get(wid)
    if not w:
        return
    w["status"] = st
    t = time.time()
    if st == "ocupado":
        w["busy_since"] = t
        w["idle_since"] = None
    elif st == "disponible":
        w["idle_since"] = t
        w["busy_since"] = None


def _enqueue(wid: str, msg: Dict[str, Any]) -> None:
    """Encola un comando para el worker (lo drena por polling). No lanza excepciones."""
    inbox.setdefault(wid, []).append(msg)
paused: list = []  # tareas viejas en espera de reanudacion (FIFO)


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def token_ok(req_token: Optional[str]) -> bool:
    return True if not CONTROL_TOKEN else req_token == CONTROL_TOKEN


def _sign(payload: str) -> str:
    sig = hmac.new(SESSION_SECRET.encode(), payload.encode(), hashlib.sha256).digest()
    return base64.urlsafe_b64encode(sig).decode()


def make_session(user: str) -> str:
    exp = int(time.time()) + SESSION_MAX_AGE
    payload = f"{user}:{exp}"
    return base64.urlsafe_b64encode(payload.encode()).decode() + "." + _sign(payload)


def valid_session(cookie: Optional[str]) -> bool:
    if not cookie:
        return False
    try:
        b64, sig = cookie.split(".", 1)
        payload = base64.urlsafe_b64decode(b64).decode()
        user, exp = payload.rsplit(":", 1)
        if int(exp) < time.time():
            return False
        if not hmac.compare_digest(sig, _sign(payload)):
            return False
        return user == PANEL_USER
    except Exception:
        return False


def words_of(text: str) -> int:
    return len((text or "").split())


def tokens_of(text: str) -> int:
    # aproximacion: ~1.33 tokens por palabra
    return int(words_of(text) * 1.33) + 1


def worker_view(wid: str) -> Dict[str, Any]:
    w = workers[wid]
    return {
        "id": wid,
        "name": w.get("name", wid),
        "perf": w.get("perf", 5),
        "weight": w.get("weight", 10),
        "brand": w.get("brand", ""),
        "model": w.get("model", ""),
        "os": w.get("os", ""),
        "status": w.get("status", "desconocido"),
        "principal": wid == principal_id,
        "task_id": w.get("task_id"),
        "stats": w.get("stats", {"tasks": 0, "words": 0, "tokens": 0}),
        "last_pong": w.get("last_pong"),
    }


def broadcast_panel(payload: Dict[str, Any]) -> None:
    data = json.dumps(payload)
    for ws in list(panel_sockets):
        try:
            asyncio.create_task(ws.send_text(data))
        except Exception:
            panel_sockets.discard(ws)


def push_workers() -> None:
    broadcast_panel({"tipo": "workers", "workers": [worker_view(w) for w in workers]})


def push_task(task_id: str) -> None:
    broadcast_panel({"tipo": "task_update", "task": tasks.get(task_id)})


def _clean_plan_text(text: str) -> str:
    """Quita ANSI y el ruido de skills (p.ej. '[skill-registry] skipping refresh')."""
    text = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", text or "")
    text = re.sub(r"\x1b[()][AB0]", "", text or "")
    out = []
    for ln in (text or "").splitlines():
        s = ln.strip()
        if s.startswith("[skill-registry]") or s.startswith("[skill"):
            continue
        out.append(ln)
    return "\n".join(out)


def _extract_json_obj(text: str) -> Optional[Dict[str, Any]]:
    start = text.find("{")
    if start < 0:
        return None
    # prueba desde la primera '{' hasta cada '}' (de mayor a menor) -> tolera ruido previo
    for end in range(len(text), start, -1):
        if text[end - 1] == "}":
            frag = text[start:end]
            try:
                d = json.loads(frag)
                if isinstance(d, dict):
                    return d
            except Exception:
                pass
    return None


def parse_plan(text: str) -> Dict[str, Any]:
    """Extrae el JSON del plan de la salida de la principal (tolerante a ruido)."""
    text = _clean_plan_text(text)
    # 1) JSON dentro de fence ```json ... ```
    try:
        m = re.search(r"```(?:json)?\s*(\{.*?\})\s*```", text, re.DOTALL)
        if m:
            return json.loads(m.group(1))
    except Exception:
        pass
    # 2) primer objeto {...} valido (desde el final hacia atras para pillar el final)
    obj = _extract_json_obj(text)
    if obj is not None:
        return obj
    # 3) array suelto de archivos -> modo por defecto
    try:
        m = re.search(r"\[.*\]", text, re.DOTALL)
        if m:
            arr = json.loads(m.group(0))
            if isinstance(arr, list) and arr:
                return {"mode": mode, "files": [a for a in arr if isinstance(a, str)]}
    except Exception:
        pass
    return {}


def _valid_file(f: Any) -> bool:
    """Solo acepta rutas de archivo plausibles; descarta placeholders y frases."""
    if not isinstance(f, str):
        return False
    s = f.strip()
    if not s or len(s) > 120:
        return False
    if s[0] in "([":  # placeholders como "(tarea-completa)"
        return False
    if " " in s:  # no frases
        return False
    if not re.search(r"[/.]", s) and not re.fullmatch(r"[\w\-]+", s):
        return False
    return True


def dispatch_plan(task_id: str) -> None:
    """Reparte el plan de la principal.
    Cada archivo es un documento real (.py/.html/.css...); la principal ya trajo
    un prompt especifico por archivo en 'items'. El servidor hace work-stealing
    en modo porcentaje y reassignment por timeout (>5 min) en ambos modos.
    Si el plan no trae JSON util (típico con OpenCode real), hace fallback."""
    t = tasks[task_id]
    plan = t.get("plan") or {}
    pmode = plan.get("mode") or t.get("mode_requested") or mode
    items = plan.get("items") or []
    if items:
        files = [it.get("file") for it in items if it.get("file")]
        prompt_map = {it["file"]: it.get("prompt", "") for it in items}
    else:
        files = list(plan.get("files", []) or [])
        # en modo percent los archivos viven en 'assignments', no en 'files'
        for _w, _wf in (plan.get("assignments") or {}).items():
            files.extend(_wf or [])
        prompt_map = {}
    # valida que sean archivos reales; descarta placeholders/frases del plan basura
    valid_files = [f for f in files if _valid_file(f)]
    if not valid_files:
        # plan inutil (timeout, ruido, o JSON roto): falla limpio en vez de crear basura
        t["status"] = "failed"
        t["error"] = ("El plan no produjo archivos validos (plan basura o timeout de la PC principal). "
                      "Reintenta la tarea o divide manualmente desde el panel.")
        t["finished_at"] = now_iso()
        asyncio.create_task(db.save_task(t))
        push_task(task_id)
        return
    files = valid_files
    t["items"] = prompt_map
    t["pqueues"] = {}
    t["active"] = {}
    t["recalling"] = {}

    if pmode == "dynamic":
        t["queue"] = list(files)
        t["reserved"] = []
        t["mode_used"] = "dynamic"
        _pump_dynamic(task_id)
    else:
        t["mode_used"] = "percent"
        # El reparto lo decide el USUARIO via t["weights"] (porcentajes por PC);
        # el planner solo aporta la lista de archivos. Se particiona por esos %.
        _distribute_weight(task_id, files)
        for wid in list(t["pqueues"].keys()):
            _dispatch_next(task_id, wid)

    if not t["subtasks"]:
        t["status"] = "done"
        t["finished_at"] = now_iso()
        asyncio.create_task(db.save_task(t))
        push_task(task_id)


STUCK_SECONDS = int(os.environ.get("STUCK_SECONDS", "300"))
# Preempcion por IDLE: una PC ocupada >IDLE_BUSY_S y otra libre >IDLE_FREE_S -> handoff.
# Si la ocupada no devuelve progreso en IDLE_RESP_S -> la libre se queda TODA la tarea desde 0.
IDLE_BUSY_S = int(os.environ.get("IDLE_BUSY_S", "300"))
IDLE_FREE_S = int(os.environ.get("IDLE_FREE_S", "300"))
IDLE_RESP_S = int(os.environ.get("IDLE_RESP_S", "300"))


def _distribute_weight(task_id: str, files: list) -> None:
    """Rellena t['pqueues'] segun los porcentajes del usuario (t['weights']) si
    existen, sino por el peso de cada PC. Reparte archivos enteros que suman
    len(files) (mayor resto)."""
    t = tasks[task_id]
    if not files:
        return
    subset = t.get("workers") or list(workers.keys())
    avail = [w for w in subset if w in workers]
    if not avail:
        return
    wmap = t.get("weights") or {}
    if wmap:
        weights = {w: float(wmap.get(w, 0) or 0) for w in avail}
    else:
        weights = {w: float(workers[w].get("weight", 10)) for w in avail}
    total = sum(weights.values())
    if total <= 0:
        weights = {w: 1.0 for w in avail}
        total = len(avail)
    n = len(files)
    raw = {w: weights[w] / total * n for w in avail}
    floor = {w: int(raw[w]) for w in avail}
    rema = n - sum(floor.values())
    # reparte el resto a los de mayor parte fraccionaria (determinista)
    for i, w in enumerate(sorted(avail, key=lambda w: raw[w] - floor[w], reverse=True)):
        if i < rema:
            floor[w] += 1
    i = 0
    for w in avail:
        cnt = floor[w]
        if cnt <= 0:
            continue
        t.setdefault("pqueues", {}).setdefault(w, []).extend(files[i:i + cnt])
        i += cnt


def _dispatch_next(task_id: str, wid: str) -> Optional[str]:
    """Envía el primer archivo de la cola de la PC (si hay y le toca el turno)."""
    t = tasks[task_id]
    w = workers.get(wid)
    if not w:
        return None
    # serializacion por PC: solo la tarea al frente de la cola de la PC puede
    # enviar subtrabajos a esa PC (una PC termina la tarea de un lider antes
    # de ejecutar la de otro lider).
    q = w.get("task_queue") or []
    if not q or q[0] != task_id:
        return None
    q2 = t.get("pqueues", {}).get(wid)
    if not q2:
        return None
    f = q2.pop(0)
    return _send_one(task_id, wid, f)


def _pump_pc(task_id: str, wid: str) -> None:
    """Arranca la siguiente subtarea de task_id en wid si le toca el turno."""
    w = workers.get(wid)
    if not w:
        return
    q = w.get("task_queue") or []
    if not q or q[0] != task_id:
        return
    if task_id not in tasks:
        return
    if wid in tasks[task_id].get("active", {}):
        return
    _dispatch_next(task_id, wid)


def _send_one(task_id: str, wid: str, file: str) -> str:
    """Crea un subtrabajo POR ARCHIVO y lo envía a la PC."""
    t = tasks[task_id]
    subtask_id = uuid.uuid4().hex[:8]
    fp = t.get("items", {}).get(file, "")
    t["subtasks"][subtask_id] = {
        "worker": wid, "files": [file], "status": "running", "output": None,
        "started_at": now_iso(), "file_prompt": fp,
    }
    t["active"][wid] = subtask_id
    _set_status(wid, "ocupado")
    workers[wid]["task_id"] = task_id
    _enqueue(wid, {
        "tipo": "tarea", "task_id": task_id, "subtask_id": subtask_id,
        "kind": "work", "prompt": t["prompt"], "files": [file],
        "file_prompt": fp, "plan_summary": t.get("plan_summary", ""),
    })
    push_workers()
    push_task(task_id)
    return subtask_id


def _pump_dynamic(task_id: str) -> None:
    """Modo dinamico: reserva el siguiente archivo para cada PC libre del conjunto."""
    t = tasks[task_id]
    queue = t.get("queue", [])
    subset = t.get("workers") or list(workers.keys())
    while queue:
        free = [w for w in subset if w in workers and w not in t.get("active", {})]
        if not free:
            break
        wid = free[0]
        f = queue.pop(0)
        t["reserved"].append(f)
        _send_one(task_id, wid, f)


def _free_workers(task_id: str) -> list:
    t = tasks[task_id]
    subset = t.get("workers") or list(workers.keys())
    busy = set(t.get("active", {}).keys())
    free = []
    for w in subset:
        if w not in workers:
            continue
        if w in busy:
            continue
        if t.get("mode_used") == "percent" and t.get("pqueues", {}).get(w):
            continue
        free.append(w)
    return free


def _rebalance(task_id: str) -> None:
    """Work-stealing en porcentaje + reassignment por timeout (>STUCK_SECONDS)."""
    t = tasks[task_id]
    if t["status"] != "running":
        return
    # 1) robo: PCs libres se llevan el ULTIMO archivo de la cola mas cargada
    if t.get("mode_used") == "percent":
        free = _free_workers(task_id)
        while free:
            subset_r = t.get("workers") or list(workers.keys())
            candidates = [w for w in subset_r if w in workers and len(t.get("pqueues", {}).get(w, [])) > 1]
            if not candidates:
                break
            busiest = max(candidates, key=lambda w: len(t["pqueues"][w]))
            f = t["pqueues"][busiest].pop()
            wid = free.pop(0)
            _send_one(task_id, wid, f)
            broadcast_panel({"tipo": "event", "msg": f"rebalance: {f} de {busiest} -> {wid}"})
            try:
                _enqueue(busiest,
                    {"tipo": "recall", "file": f,
                     "note": f"se te quita el ultimo archivo de tu cola: {f}"})
            except Exception:
                pass
    # 2) timeout: archivo >STUCK_SECONDS en una PC y hay otra libre -> devolver progreso
    free = _free_workers(task_id)
    if not free:
        return
    now = datetime.now(timezone.utc)
    for wid, st_id in list(t.get("active", {}).items()):
        st = t["subtasks"].get(st_id)
        if not st or st.get("status") != "running":
            continue
        started = st.get("started_at")
        if not started:
            continue
        age = (now - datetime.fromisoformat(started)).total_seconds()
        if age <= STUCK_SECONDS:
            continue
        f = st["files"][0]
        free_wid = free.pop(0) if free else None
        if not free_wid:
            break
        new_st = _send_one(task_id, free_wid, f)
        t["recalling"][st_id] = new_st
        try:
            _enqueue(wid,
                {"tipo": "return_progress", "task_id": task_id,
                 "subtask_id": st_id, "file": f})
        except Exception:
            pass
        broadcast_panel({"tipo": "event",
                         "msg": f"timeout {STUCK_SECONDS}s: {f} {wid} -> {free_wid} (devolver progreso)"})
        if not free:
            break


def _preempt_by_idle(task_id: str) -> None:
    """Preempcion por IDLE (spec del usuario).

    Si una PC lleva >IDLE_BUSY_S ocupada en un subtrabajo Y otra PC del subset
    lleva >IDLE_FREE_S libre, se le ordena a la ocupada devolver progreso y se
    hace handoff del archivo a la libre (la ocupada pausa). Si la ocupada no
    responde el return_progress en IDLE_RESP_S, la libre recibe TODA la tarea
    desde 0 y la ocupada queda marcada como no conforme (sus tareas futuras se
    reasignan y se le pide reiniciar opencode).
    """
    t = tasks[task_id]
    if t["status"] != "running":
        return
    now = time.time()
    subset = t.get("workers") or list(workers.keys())
    busy = [(w, workers[w]) for w in subset
            if w in workers and workers[w].get("status") == "ocupado"
            and workers[w].get("busy_since") and (now - workers[w]["busy_since"]) > IDLE_BUSY_S]
    free = [w for w in subset
            if w in workers and workers[w].get("status") == "disponible"
            and workers[w].get("idle_since") and (now - workers[w]["idle_since"]) > IDLE_FREE_S]
    if not busy or not free:
        return
    for wid, w in busy:
        if not free:
            break
        st_id = t.get("active", {}).get(wid)
        if not st_id:
            continue
        st = t["subtasks"].get(st_id)
        if not st or st.get("status") != "running":
            continue
        f = st["files"][0]
        free_wid = free.pop(0)
        # ¿ya se le pidio devolver progreso y no respondio en IDLE_RESP_S?
        pending = t.get("handoff_pending", {}).get(st_id)
        if pending and (now - pending["at"]) > IDLE_RESP_S:
            # No hubo respuesta -> asignar TODA la tarea a la libre desde 0.
            to_wid = pending.get("to") or free_wid
            _restart_task_from_zero(task_id, from_wid=wid, to_wid=to_wid)
            # Marcar la PC no conforme y pedirle reiniciar opencode.
            w["noncompliant"] = True
            try:
                _enqueue(wid, {"tipo": "restart", "task_id": task_id,
                               "reason": "no respondio handoff en " + str(IDLE_RESP_S) + "s"})
            except Exception:
                pass
            broadcast_panel({"tipo": "event",
                             "msg": f"PC {wid} NO respondio handoff: tarea completa -> {free_wid} (reiniciar opencode {wid})"})
            continue
        # Handoff normal: se ordena a la ocupada devolver progreso; la libre ESPERA
        # libre hasta que la ocupada efectivamente devuelva (o hasta IDLE_RESP_S).
        t["recalling"][st_id] = free_wid
        try:
            _enqueue(wid,
                {"tipo": "return_progress", "task_id": task_id,
                 "subtask_id": st_id, "file": f})
        except Exception:
            pass
        t.setdefault("handoff_pending", {})[st_id] = {"at": now, "to": free_wid}
        broadcast_panel({"tipo": "event",
                         "msg": f"preempcion IDLE: {wid} ocupada {int(now - w['busy_since'])}s -> orden devolver progreso a {free_wid}"})


def _restart_task_from_zero(task_id: str, from_wid: str, to_wid: str) -> None:
    """Reasigna TODA la tarea a to_wid desde cero (sin progreso de from_wid)."""
    t = tasks[task_id]
    # Cancelar lo que venia haciendo from_wid.
    st_id = t.get("active", {}).pop(from_wid, None)
    if st_id:
        t["subtasks"].pop(st_id, None)
    # Devolver todos los archivos pendientes de from_wid a la tarea.
    pq = t.setdefault("pqueues", {}).setdefault(from_wid, [])
    # La tarea completa (todos sus items) vuelve a repartirse, esta vez solo a to_wid.
    files = list(t.get("items", {}).keys())
    t["workers"] = [to_wid]
    t["pqueues"] = {to_wid: list(files)}
    t["active"] = {}
    t["recalling"] = {}
    t["handoff_pending"] = {}
    t["subtasks"] = {}
    t["done"] = 0
    t["mode_used"] = t.get("mode_used", "percent")
    # Poner a to_wid al frente de su cola de tareas.
    wq = workers.setdefault(to_wid, _new_worker(to_wid)).setdefault("task_queue", [])
    while task_id in wq:
        wq.remove(task_id)
    wq.insert(0, task_id)
    _set_status(to_wid, "disponible")
    _pump_pc(task_id, to_wid)
    broadcast_panel({"tipo": "event", "msg": f"tarea {task_id} reasignada entera a {to_wid} desde 0"})


def _on_subtask_done(task_id: str, subtask_id: str, output: str, status: str = "done") -> None:
    t = tasks[task_id]
    st = t["subtasks"].get(subtask_id)
    if not st:
        return
    # si este subtrabajo tenia un handoff pendiente, se resuelve ahora
    hp = t.get("handoff_pending", {}).pop(subtask_id, None)
    if status == "returned":
        # la PC devolvio el progreso parcial
        t["subtasks"].pop(subtask_id, None)
        t["active"].pop(st["worker"], None)
        if hp:
            # la ocupada devolvio progreso -> la libre (hp["to"]) se queda el archivo
            target = hp.get("to")
            if target and target in workers:
                _send_one(task_id, target, st["files"][0])
                broadcast_panel({"tipo": "event",
                                 "msg": f"handoff completado: {st['files'][0]} -> {target}"})
                push_workers()
                push_task(task_id)
                return
        # sin handoff: la PC sigue con sus archivos restantes
        if t.get("mode_used") == "percent" and t.get("pqueues", {}).get(st["worker"]):
            _dispatch_next(task_id, st["worker"])
        push_workers()
        push_task(task_id)
        return
    st["status"] = "done"
    st["output"] = output
    st["finished_at"] = now_iso()
    wid = st["worker"]
    w = workers.get(wid)
    if w:
        s = w.setdefault("stats", {"tasks": 0, "words": 0, "tokens": 0})
        s["tasks"] += 1
        s["words"] += words_of(output)
        s["tokens"] += tokens_of(output)
    t["active"].pop(wid, None)
    # ¿quedan archivos de ESTA tarea para esta PC?
    if w and t.get("pqueues", {}).get(wid):
        _dispatch_next(task_id, wid)   # sigue con la siguiente subtarea de esta tarea
        # _send_one (si envio) ya marca la PC ocupada; si no envio, queda libre
        if wid not in t.get("active", {}):
            _set_status(wid, "disponible")
            w["task_id"] = None
    else:
        # esta tarea termino su trabajo en esta PC -> sacarla de la cola de la PC
        if w:
            q = w.setdefault("task_queue", [])
            while task_id in q:
                q.remove(task_id)
            if q:
                _pump_pc(q[0], wid)   # arrancar la siguiente tarea en cola en esta PC
            else:
                _set_status(wid, "disponible")
                w["task_id"] = None
    # ¿la tarea completo todas sus subtareas?
    if t["subtasks"] and all(s["status"] == "done" for s in t["subtasks"].values()):
        t["status"] = "done"
        t["finished_at"] = now_iso()
        for wid2 in list(t.get("workers", []) or []):
            ww = workers.get(wid2)
            if ww:
                q2 = ww.setdefault("task_queue", [])
                while task_id in q2:
                    q2.remove(task_id)
        _maybe_next()
        # Fire-and-forget: log task completion to Engram (spec:58-72).
        asyncio.create_task(_engram_save(
            "Task " + task_id + " completed",
            t.get("prompt", "")[:2000],
            ["worker"],
        ))
        # Cierre distribuido: los reportes de los workers van a C3 para que
        # consolide (snapshot Engram+EMGRAN, commit del closeout, deploy
        # opcional). Best-effort, nunca bloquea el panel.
        try:
            _closeout_outputs = [
                str(s.get("output", ""))[:2000]
                for s in (t.get("subtasks") or {}).values()
                if s.get("output")
            ][:20]
            asyncio.create_task(carlos_code_closeout(
                task_id,
                t.get("prompt", ""),
                _closeout_outputs,
            ))
        except Exception:
            pass
    push_workers()
    asyncio.create_task(db.save_task(t))
    push_task(task_id)


# ---------------------------------------------------------------------------
# Cola de tareas + preempcion: una tarea nueva interrumpe las viejas; al
# terminar la nueva, se reanudan las viejas ejecutando lo que faltaba.
# ---------------------------------------------------------------------------
def _maybe_next() -> None:
    global current_task
    if current_task and tasks.get(current_task, {}).get("status") in ("done", "failed"):
        current_task = None
    if current_task is None and paused:
        nxt = paused.pop(0)
        current_task = nxt
        _resume_task(nxt)


def _pause_task(task_id: str) -> None:
    """Pausa una tarea en curso: aborta PCs activas y guarda archivos restantes."""
    t = tasks[task_id]
    remaining = []
    for st in list(t["subtasks"].values()):
        if st["status"] != "done":
            remaining.extend(st["files"])
            st["status"] = "paused"
    for wid, st_id in list(t.get("active", {}).items()):
        st = t["subtasks"].get(st_id, {})
        f = (st.get("files") or [None])[0]
        try:
            _enqueue(wid,
                {"tipo": "return_progress", "task_id": task_id,
                 "subtask_id": st_id, "file": f})
        except Exception:
            pass
        w = workers.get(wid)
        if w:
            _set_status(wid, "disponible")
            w["task_id"] = None
    # si la principal seguia planeando, cancelarla
    tp = t.get("principal") or principal_id
    if tp and tp in workers and workers[tp].get("task_id") == task_id:
        try:
            _enqueue(tp,
                {"tipo": "cancel", "task_id": task_id})
        except Exception:
            pass
        _set_status(tp, "disponible")
        workers[tp]["task_id"] = None
    t["remaining"] = remaining
    t["active"] = {}
    t["pqueues"] = {}
    t["status"] = "paused"
    asyncio.create_task(db.save_task(t))
    push_task(task_id)


def _resume_task(task_id: str) -> None:
    """Reanuda una tarea pausada ejecutando los archivos que faltaban."""
    t = tasks[task_id]
    # si nunca llego a planearse, re-pedir el plan a la principal de esta tarea
    tp = t.get("principal") or principal_id
    if t.get("plan") is None and tp and tp in workers:
        t["status"] = "planning"
        _set_status(tp, "ocupado")
        workers[tp]["task_id"] = task_id
        wsummary = [{"id": w, "weight": workers[w].get("weight", 10)} for w in workers]
        _enqueue(tp, {
            "tipo": "tarea", "task_id": task_id, "kind": "plan",
            "prompt": t["prompt"], "workers": wsummary, "mode": mode,
        })
        asyncio.create_task(db.save_task(t))
        push_task(task_id)
        return
    remaining = t.get("remaining") or []
    if not remaining:
        t["status"] = "done"
        t["finished_at"] = now_iso()
        _maybe_next()
        asyncio.create_task(db.save_task(t))
        push_task(task_id)
        return
    t["status"] = "running"
    t["pqueues"] = {}
    t["active"] = {}
    t["recalling"] = {}
    pmode = t.get("mode_used") or (t.get("plan") or {}).get("mode", mode)
    if pmode == "dynamic":
        t["queue"] = list(remaining)
        t["reserved"] = []
        _pump_dynamic(task_id)
    else:
        _distribute_weight(task_id, remaining)
        for wid in list(t["pqueues"].keys()):
            _dispatch_next(task_id, wid)
    asyncio.create_task(db.save_task(t))
    push_task(task_id)


# ---------------------------------------------------------------------------
# REST
# ---------------------------------------------------------------------------
@app.get("/", response_class=HTMLResponse)
async def index(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return RedirectResponse("/login")
    return HTMLResponse(INDEX_HTML.read_text(encoding="utf-8"))


LOGIN_HTML = """<!DOCTYPE html>
<html lang="es"><head><meta charset="utf-8"><title>Login — Control Web</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:360px;margin:4rem auto;padding:0 1rem}
input,button{font:inherit;padding:.5rem .6rem;margin:.3rem 0;width:100%;box-sizing:border-box;border:1px solid #8884;border-radius:6px}
button{background:#26a;color:#fff;border-color:#26a;cursor:pointer}.err{color:#c33}</style></head>
<body><h2>Control Web — acceso</h2>
<form method="post" action="/login">
<label>Usuario<input name="user" autofocus></label>
<label>Contraseña<input name="password" type="password"></label>
<button type="submit">Entrar</button>
</form></body></html>"""

LOGIN_HTML_ERR = LOGIN_HTML.replace("<form method=", '<p class="err">Usuario o contraseña incorrectos.</p><form method=')


@app.get("/login", response_class=HTMLResponse)
async def login_get(request: Request):
    if valid_session(request.cookies.get("panel_session")):
        return RedirectResponse("/")
    return HTMLResponse(LOGIN_HTML)


@app.post("/login")
async def login_post(request: Request, user: str = Form(...), password: str = Form(...)):
    if user == PANEL_USER and PANEL_PASSWORD and secrets.compare_digest(password, PANEL_PASSWORD):
        resp = RedirectResponse("/", status_code=303)
        resp.set_cookie("panel_session", make_session(user),
                        httponly=True, samesite="lax", max_age=SESSION_MAX_AGE, path="/")
        return resp
    return HTMLResponse(LOGIN_HTML_ERR, status_code=401)


@app.get("/logout")
async def logout():
    resp = RedirectResponse("/login", status_code=303)
    resp.delete_cookie("panel_session", path="/")
    return resp


@app.get("/health")
async def health():
    return {"status": "ok"}


@app.get("/api/state")
async def api_state(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    # Verificar Carlos Code en background (no bloquear)
    cc_available = await carlos_code_available()
    return {"mode": mode, "principal": principal_id,
            "workers": [worker_view(w) for w in workers],
            "worker_memory": list(worker_memory.values()),
            "tasks": list(tasks.values()),
            "db_ok": db._ok(),
            "db_error": db.get_last_error(),
            "db_stage": db.get_stage(),
            "has_db_url": db.has_db_url(),
            "carlos_code": {
                "available": cc_available,
                "url": CARLOS_CODE_URL,
            }}


@app.get("/api/keepalive")
async def api_keepalive(request: Request):
    """Estado del keep-alive: toggle on/off + último heartbeat por servicio."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return {
        "enabled": KEEPALIVE_STATE["enabled"],
        "interval_sec": KEEPALIVE_INTERVAL_SEC,
        "last_run": KEEPALIVE_STATE["last_run"],
        "services": KEEPALIVE_STATE["services"],
    }


@app.post("/api/keepalive")
async def api_keepalive_set(request: Request):
    """Enciende/apaga el keep-alive. Body: {"enabled": true|false}."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
        enabled = bool(body.get("enabled"))
    except Exception:  # noqa: BLE001
        return JSONResponse({"error": "body inválido"}, status_code=400)
    KEEPALIVE_STATE["enabled"] = enabled
    if not enabled:
        KEEPALIVE_STATE["last_run"] = ""
    asyncio.create_task(db.save_keepalive(
        KEEPALIVE_STATE["enabled"],
        KEEPALIVE_STATE["last_run"],
        KEEPALIVE_STATE["services"],
    ))
    # Also write .cai_keepalive.json as file fallback (design:48-54)
    db.save_keepalive_file(
        KEEPALIVE_STATE["enabled"],
        KEEPALIVE_STATE["last_run"],
        KEEPALIVE_STATE["services"],
    )
    asyncio.create_task(db.log_event(
        f"Keepalive {'activado' if enabled else 'desactivado'} ({KEEPALIVE_INTERVAL_SEC}s)"
    ))
    return {"ok": True, "enabled": KEEPALIVE_STATE["enabled"]}


@app.get("/api/workers")
async def api_workers(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return {"workers": [worker_view(w) for w in workers], "principal": principal_id}


@app.get("/api/tasks")
async def api_tasks(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return {"tasks": list(tasks.values())}


@app.get("/api/sessions")
async def api_sessions(request: Request):
    """List active task sessions with their current state."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return {
        "sessions": [
            {
                "task_id": t_id,
                "prompt": t.get("prompt", "")[:100],
                "status": t.get("status", ""),
                "principal": t.get("principal", ""),
                "workers": t.get("workers", []),
                "mode": t.get("mode_used", t.get("mode", "percent")),
                "created_at": t.get("created_at", ""),
            }
            for t_id, t in tasks.items()
        ]
    }


# ----------------------------- cancelar / reintentar / delegar / eliminar -----------------------------
def _abort_task(task_id: str) -> None:
    """Aborta todo lo que esta corriendo de una tarea (planner y subtareas activas)."""
    t = tasks.get(task_id)
    if not t:
        return
    for wid, sid in list(t.get("active", {}).items()):
        _enqueue(wid, {"tipo": "return_progress", "task_id": task_id})
        _enqueue(wid, {"tipo": "cancel", "task_id": task_id, "subtask_id": sid,
                       "file": (t["subtasks"].get(sid, {}) or {}).get("files", [""])[0]})
        st = t["subtasks"].get(sid)
        if st:
            st["status"] = "returned"
    if t.get("principal") and t.get("status") == "planning":
        _enqueue(t["principal"], {"tipo": "return_progress", "task_id": task_id})
    t["active"] = {}
    t["status"] = "canceled"
    t["finished_at"] = now_iso()


def _retry_task(task_id: str) -> None:
    t = tasks.get(task_id)
    if not t:
        return
    _abort_task(task_id)
    t["status"] = "planning"
    t["subtasks"] = {}; t["active"] = {}; t["plan"] = None; t["plan_summary"] = ""
    t["queue"] = []; t["reserved"] = []; t["items"] = {}; t["pqueues"] = {}; t["recalling"] = {}
    t["finished_at"] = None
    chosen = t.get("principal") or (t.get("workers") or [None])[0]
    subset = t.get("workers") or ([chosen] if chosen else [])
    if chosen and chosen in workers and subset:
        wsummary = [{"id": w, "weight": workers[w].get("weight", 10),
                     "status": workers[w].get("status", "desconocido"), "index": i + 1}
                    for i, w in enumerate(subset)]
        status_snapshot = {w: {"status": workers[w].get("status", "desconocido"),
                               "name": workers[w].get("name", w), "index": i + 1}
                           for i, w in enumerate(subset)}
        _enqueue(chosen, {"tipo": "tarea", "task_id": task_id, "kind": "plan",
                          "prompt": t["prompt"], "workers": wsummary,
                          "mode": t.get("mode_used") or mode, "status_snapshot": status_snapshot})
        _set_status(chosen, "ocupado"); workers[chosen]["task_id"] = task_id
    asyncio.create_task(db.save_task(t))
    push_task(task_id)


def _retry_subtask(task_id: str, subtask_id: str) -> None:
    t = tasks.get(task_id)
    s = t["subtasks"].get(subtask_id) if t else None
    if not s:
        return
    wid = s["worker"]
    if t.get("active", {}).get(wid) == subtask_id:
        _enqueue(wid, {"tipo": "return_progress", "task_id": task_id})
        t["active"].pop(wid, None)
    s["status"] = "running"; s["output"] = None; s["started_at"] = now_iso()
    fp = s.get("file_prompt", "")
    file = (s.get("files") or [""])[0]
    _enqueue(wid, {"tipo": "tarea", "task_id": task_id, "subtask_id": subtask_id,
                   "kind": "work", "prompt": t["prompt"], "files": [file],
                   "file_prompt": fp, "plan_summary": t.get("plan_summary", "")})
    t["active"][wid] = subtask_id
    _set_status(wid, "ocupado"); workers[wid]["task_id"] = task_id
    asyncio.create_task(db.save_task(t))
    push_task(task_id); push_workers()


def _cancel_subtask(task_id: str, subtask_id: str) -> None:
    t = tasks.get(task_id)
    s = t["subtasks"].get(subtask_id) if t else None
    if not s:
        return
    wid = s["worker"]
    if t.get("active", {}).get(wid) == subtask_id:
        _enqueue(wid, {"tipo": "return_progress", "task_id": task_id})
        t["active"].pop(wid, None)
        workers[wid].pop("task_id", None)
        _set_status(wid, "disponible")
    s["status"] = "returned"
    asyncio.create_task(db.save_task(t))
    push_task(task_id); push_workers()


def _delegate_subtask(task_id: str, subtask_id: str, to_wid: str) -> bool:
    t = tasks.get(task_id)
    s = t["subtasks"].get(subtask_id) if t else None
    if not s or to_wid not in workers:
        return False
    from_wid = s["worker"]
    if from_wid != to_wid and t.get("active", {}).get(from_wid) == subtask_id:
        _enqueue(from_wid, {"tipo": "return_progress", "task_id": task_id})
        t["active"].pop(from_wid, None)
    s["worker"] = to_wid
    s["status"] = "running"; s["output"] = None; s["started_at"] = now_iso()
    file = (s.get("files") or [""])[0]; fp = s.get("file_prompt", "")
    _enqueue(to_wid, {"tipo": "tarea", "task_id": task_id, "subtask_id": subtask_id,
                      "kind": "work", "prompt": t["prompt"], "files": [file],
                      "file_prompt": fp, "plan_summary": t.get("plan_summary", "")})
    t["active"][to_wid] = subtask_id
    _set_status(to_wid, "ocupado"); workers[to_wid]["task_id"] = task_id
    if from_wid in workers:
        workers[from_wid].pop("task_id", None); _set_status(from_wid, "disponible")
    asyncio.create_task(db.save_task(t))
    push_task(task_id); push_workers()
    return True


@app.post("/api/task/{task_id}/cancel")
async def api_task_cancel(task_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    _abort_task(task_id)
    asyncio.create_task(db.save_task(tasks[task_id]))
    push_workers()
    return {"ok": True, "status": tasks[task_id]["status"]}


@app.post("/api/task/{task_id}/retry")
async def api_task_retry(task_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if task_id not in tasks:
        return JSONResponse({"error": "tarea no encontrada"}, status_code=404)
    _retry_task(task_id)
    asyncio.create_task(db.save_task(tasks[task_id]))
    return {"ok": True, "status": tasks[task_id]["status"]}


@app.post("/api/task/{task_id}/subtask/{subtask_id}/retry")
async def api_subtask_retry(task_id: str, subtask_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if task_id not in tasks or subtask_id not in tasks[task_id]["subtasks"]:
        return JSONResponse({"error": "subtarea no encontrada"}, status_code=404)
    _retry_subtask(task_id, subtask_id)
    asyncio.create_task(db.save_task(tasks[task_id]))
    return {"ok": True}


@app.post("/api/task/{task_id}/subtask/{subtask_id}/cancel")
async def api_subtask_cancel(task_id: str, subtask_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if task_id not in tasks or subtask_id not in tasks[task_id]["subtasks"]:
        return JSONResponse({"error": "subtarea no encontrada"}, status_code=404)
    _cancel_subtask(task_id, subtask_id)
    asyncio.create_task(db.save_task(tasks[task_id]))
    return {"ok": True}


@app.post("/api/task/{task_id}/subtask/{subtask_id}/delegate")
async def api_subtask_delegate(task_id: str, subtask_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    body = await request.json()
    to_wid = body.get("to_wid")
    if task_id not in tasks or subtask_id not in tasks[task_id]["subtasks"]:
        return JSONResponse({"error": "subtarea no encontrada"}, status_code=404)
    if not _delegate_subtask(task_id, subtask_id, to_wid):
        return JSONResponse({"error": "PC destino no conectada"}, status_code=404)
    asyncio.create_task(db.save_task(tasks[task_id]))
    return {"ok": True, "to_wid": to_wid}


@app.post("/api/task/{task_id}/delete")
async def api_task_delete(task_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    tasks.pop(task_id, None)
    asyncio.create_task(db.delete_task(task_id))
    broadcast_panel({"tipo": "tasks_deleted", "task_id": task_id})
    return {"ok": True}


@app.post("/api/tasks/clear")
async def api_tasks_clear(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    tasks.clear()
    asyncio.create_task(db.clear_tasks())
    broadcast_panel({"tipo": "tasks_cleared"})
    return {"ok": True}


@app.get("/api/task/{task_id}/conversation")
async def api_task_conversation(task_id: str, request: Request):
    """Devuelve la conversacion completa de una tarea: prompts y respuestas
    de TODAS las PCs en orden cronologico, con el nombre de cada PC."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    t = tasks.get(task_id)
    if not t:
        return JSONResponse({"error": "tarea no encontrada"}, status_code=404)

    conversation = []

    # 1) Prompt original del usuario
    conversation.append({
        "role": "user",
        "pc": "Usuario",
        "content": t.get("prompt", ""),
        "timestamp": t.get("created_at", ""),
        "kind": "prompt",
    })

    # 2) Respuesta de la lider (planificacion)
    if t.get("plan_summary") or t.get("plan_draft"):
        leader_id = t.get("principal", "")
        leader_name = workers.get(leader_id, {}).get("name", leader_id) if leader_id else "Líder"
        conversation.append({
            "role": "assistant",
            "pc": leader_name,
            "pc_id": leader_id,
            "content": t.get("plan_draft") or t.get("plan_summary", ""),
            "timestamp": t.get("created_at", ""),
            "kind": "plan",
        })

    # 3) Subtareas: cada worker que proceso un archivo
    for sid, st in (t.get("subtasks") or {}).items():
        worker_id = st.get("worker", "")
        worker_name = workers.get(worker_id, {}).get("name", worker_id) if worker_id else worker_id
        files = st.get("files", [])
        file_str = ", ".join(files) if files else "—"

        # Prompt enviado al worker (el contexto de la tarea)
        worker_prompt = f"Archivo: {file_str}"
        if st.get("file_prompt"):
            worker_prompt += f"\n{st['file_prompt']}"

        conversation.append({
            "role": "system",
            "pc": worker_name,
            "pc_id": worker_id,
            "content": worker_prompt,
            "timestamp": st.get("started_at", ""),
            "kind": "work-prompt",
            "subtask_id": sid,
            "files": files,
        })

        # Output/respuesta del worker
        if st.get("output"):
            conversation.append({
                "role": "assistant",
                "pc": worker_name,
                "pc_id": worker_id,
                "content": st["output"],
                "timestamp": st.get("started_at", ""),
                "kind": "work-result",
                "subtask_id": sid,
                "status": st.get("status", "done"),
                "files": files,
            })

    # Ordenar por timestamp
    conversation.sort(key=lambda x: x.get("timestamp", ""))

    return {"task_id": task_id, "status": t.get("status", ""), "conversation": conversation}


@app.get("/api/subtask/{task_id}/{subtask_id}/conversation")
async def api_subtask_conversation(task_id: str, subtask_id: str, request: Request):
    """Devuelve la conversacion de una subtarea especifica."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    t = tasks.get(task_id)
    if not t:
        return JSONResponse({"error": "tarea no encontrada"}, status_code=404)
    st = t.get("subtasks", {}).get(subtask_id)
    if not st:
        return JSONResponse({"error": "subtarea no encontrada"}, status_code=404)

    worker_id = st.get("worker", "")
    worker_name = workers.get(worker_id, {}).get("name", worker_id) if worker_id else worker_id
    files = st.get("files", [])
    file_str = ", ".join(files) if files else "—"

    conversation = []

    # Contexto de la tarea global
    conversation.append({
        "role": "user",
        "pc": "Usuario",
        "content": t.get("prompt", ""),
        "timestamp": t.get("created_at", ""),
        "kind": "prompt",
    })

    # Prompt de la lider (resumen del plan)
    if t.get("plan_summary"):
        leader_id = t.get("principal", "")
        leader_name = workers.get(leader_id, {}).get("name", leader_id) if leader_id else "Líder"
        conversation.append({
            "role": "assistant",
            "pc": leader_name,
            "pc_id": leader_id,
            "content": t.get("plan_summary", ""),
            "timestamp": t.get("created_at", ""),
            "kind": "plan",
        })

    # Instruccion para esta subtarea
    worker_prompt = f"Archivo asignado: {file_str}"
    if st.get("file_prompt"):
        worker_prompt += f"\n{st['file_prompt']}"
    conversation.append({
        "role": "system",
        "pc": worker_name,
        "pc_id": worker_id,
        "content": worker_prompt,
        "timestamp": st.get("started_at", ""),
        "kind": "work-prompt",
        "subtask_id": subtask_id,
        "files": files,
    })

    # Respuesta del worker
    if st.get("output"):
        conversation.append({
            "role": "assistant",
            "pc": worker_name,
            "pc_id": worker_id,
            "content": st["output"],
            "timestamp": st.get("started_at", ""),
            "kind": "work-result",
            "subtask_id": subtask_id,
            "status": st.get("status", "done"),
            "files": files,
        })

    conversation.sort(key=lambda x: x.get("timestamp", ""))

    return {"task_id": task_id, "subtask_id": subtask_id, "status": t.get("status", ""), "conversation": conversation}


@app.post("/api/worker/{worker_id}/kick")
async def api_worker_kick(worker_id: str, request: Request):
    """Desconecta un worker manualmente (lo elimina del registro en memoria)."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if worker_id not in workers:
        return JSONResponse({"error": "worker no encontrado"}, status_code=404)
    # marcar en memoria antes de borrar
    if worker_id in worker_memory:
        worker_memory[worker_id]["connected"] = False
        worker_memory[worker_id]["status"] = "down"
        worker_memory[worker_id]["last_seen"] = now_iso()
    inbox.pop(worker_id, None)
    del workers[worker_id]
    push_workers()
    broadcast_panel({"tipo": "event", "msg": f"Worker {worker_id} desconectado manualmente"})
    asyncio.create_task(db.log_event(f"Worker {worker_id} desconectado manualmente"))
    return {"ok": True}


@app.post("/api/reassign")
async def api_reassign(request: Request):
    """Retira un archivo pendiente de una PC y lo asigna a otra (work-stealing).

    Cuerpo: {task_id, from_wid, file, to_wid?}. Si to_wid se omite, se elige la
    siguiente PC en la prioridad del conjunto (t['workers']).
    """
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    body = await request.json()
    task_id = body.get("task_id")
    from_wid = body.get("from_wid")
    file = body.get("file")
    to_wid = body.get("to_wid")
    t = tasks.get(task_id)
    if not t:
        return JSONResponse({"error": "tarea no existe"}, status_code=404)
    pq = t.get("pqueues", {})
    active_st = t.get("active", {}).get(from_wid)
    is_active = (active_st and t["subtasks"].get(active_st, {}).get("files")
                 and t["subtasks"][active_st]["files"][0] == file)

    # destino por defecto:
    #  - archivo PENDIENTE -> siguiente PC en la prioridad del conjunto
    #  - subtarea ACTIVA cancelada -> PC de NUMERO MENOR (anterior en el orden)
    def _default_target():
        order = t.get("workers") or []
        try:
            idx = order.index(from_wid)
        except ValueError:
            idx = -1
        if is_active:
            cand = [w for w in order[:idx] if w in workers and w != from_wid]  # anteriores
            return cand[-1] if cand else None
        cand = [w for w in order[idx + 1:] if w in workers and w != from_wid]  # siguientes
        return cand[0] if cand else None

    if not to_wid:
        to_wid = _default_target()
    if not to_wid or to_wid not in workers:
        return JSONResponse({"error": "no hay PC destino disponible"}, status_code=400)

    # ---- subtarea ACTIVA: cancelar en from_wid y reasignar el archivo ----
    if is_active:
        try:
            _enqueue(from_wid,
                {"tipo": "cancel", "task_id": task_id, "subtask_id": active_st, "file": file})
        except Exception:
            pass
        t["active"].pop(from_wid, None)
        t["subtasks"].pop(active_st, None)
        # si la PC origen no tiene mas archivos pendientes ni activos en esta tarea,
        # queda disponible (salta a su siguiente tarea/archivo via _pump_pc)
        if not t.get("pqueues", {}).get(from_wid) and from_wid not in t["active"]:
            _set_status(from_wid, "disponible")
            workers[from_wid]["task_id"] = None
        if to_wid not in t.get("workers", []):
            t.setdefault("workers", []).append(to_wid)
        pq.setdefault(to_wid, []).append(file)
        wq = workers[to_wid].setdefault("task_queue", [])
        if task_id not in wq:
            wq.insert(0, task_id)
        _pump_pc(task_id, to_wid)    # la destino arranca este archivo
        _pump_pc(task_id, from_wid)  # la origen salta a su siguiente tarea/archivo
        broadcast_panel({"tipo": "event",
                         "msg": f"cancelado {file} en {from_wid} -> reasignado a {to_wid} (nº menor)"})
        push_workers()
        push_task(task_id)
        return {"ok": True, "to_wid": to_wid, "file": file, "canceled": True}

    # ---- archivo PENDIENTE: mover de cola ----
    if from_wid not in pq or file not in pq[from_wid]:
        return JSONResponse({"error": "archivo no pendiente en esa PC"}, status_code=400)
    pq[from_wid].remove(file)
    pq.setdefault(to_wid, []).append(file)
    if to_wid not in t.get("workers", []):
        t.setdefault("workers", []).append(to_wid)
    wq = workers[to_wid].setdefault("task_queue", [])
    if task_id not in wq:
        wq.insert(0, task_id)
    _pump_pc(task_id, to_wid)
    broadcast_panel({"tipo": "event", "msg": f"reasignado {file}: {from_wid} -> {to_wid}"})
    push_workers()
    push_task(task_id)
    return {"ok": True, "to_wid": to_wid, "file": file}


@app.post("/api/refresh-status")
async def api_refresh_status(request: Request):
    """Pide status a TODAS las PCs conectadas y limpia las que ya no responden.
    El panel lo usa para saber cuales estan activas realmente."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    now = datetime.now(timezone.utc)
    n = 0
    cleaned = 0
    for wid in list(workers.keys()):
        # Verificar si el worker ha respondido recientemente
        last = workers[wid].get("last_poll")
        age = 0.0
        if last:
            try:
                age = (now - datetime.fromisoformat(last)).total_seconds()
            except Exception:
                age = 0.0
        # Si no responde en WORKER_TIMEOUT, limpiarlo
        if age > WORKER_TIMEOUT:
            # Marcar en memoria antes de borrar
            if wid in worker_memory:
                worker_memory[wid]["connected"] = False
                worker_memory[wid]["status"] = "down"
                worker_memory[wid]["last_seen"] = now_iso()
            inbox.pop(wid, None)
            del workers[wid]
            cleaned += 1
            continue
        try:
            _enqueue(wid, {"tipo": "status"})
            n += 1
        except Exception:
            pass
    if cleaned:
        push_workers()
        broadcast_panel({"tipo": "event", "msg": f"refresh: {cleaned} PCs limpiadas, status solicitado a {n}"})
    else:
        broadcast_panel({"tipo": "event", "msg": f"solicitando status a {n} PCs"})
    return {"ok": True, "count": n, "cleaned": cleaned}


@app.get("/api/principal")
async def api_principal_get(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return {"principal": principal_id}


@app.post("/api/principal")
async def api_principal_set(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    body = await request.json()
    wid = body.get("id")
    if wid and wid not in workers:
        return JSONResponse({"error": "worker no conectado"}, status_code=404)
    global principal_id
    principal_id = wid or None
    push_workers()
    broadcast_panel({"tipo": "principal_update", "principal": principal_id})
    return {"principal": principal_id}


@app.post("/api/mode")
async def api_mode_set(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    body = await request.json()
    global mode
    mode = body.get("mode", mode)
    if mode not in ("percent", "dynamic"):
        return JSONResponse({"error": "modo invalido"}, status_code=400)
    broadcast_panel({"tipo": "mode_update", "mode": mode})
    return {"mode": mode}


# ---------------------------------------------------------------------------
# History CRUD: gateway_history + carlos_code_history
# ---------------------------------------------------------------------------
@app.get("/api/gateway-history")
async def api_gateway_history(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        limit = int(request.query_params.get("limit", "50"))
    except (TypeError, ValueError):
        limit = 50
    rows = await db.load_gateway_history(limit)
    # Serialize datetime objects for JSON
    for r in rows:
        if hasattr(r.get("ts"), "isoformat"):
            r["ts"] = r["ts"].isoformat()
    return {"rows": rows}


@app.delete("/api/gateway-history/{row_id}")
async def api_gateway_history_delete_one(row_id: int, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    ok = await db.delete_gateway_history(row_id)
    return {"ok": ok}


@app.delete("/api/gateway-history")
async def api_gateway_history_delete_all(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    ok = await db.delete_gateway_history_all()
    return {"ok": ok}


@app.post("/api/gateway-history")
async def api_gateway_history_save(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    entry = await request.json()
    ok = await db.save_gateway_history(entry)
    return {"ok": ok}


@app.post("/api/carlos-history")
async def api_carlos_history_save(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    entry = await request.json()
    ok = await db.save_carlos_history(entry)
    return {"ok": ok}


@app.get("/api/carlos-history")
async def api_carlos_history(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        limit = int(request.query_params.get("limit", "50"))
    except (TypeError, ValueError):
        limit = 50
    rows = await db.load_carlos_history(limit)
    for r in rows:
        if hasattr(r.get("ts"), "isoformat"):
            r["ts"] = r["ts"].isoformat()
    return {"rows": rows}


@app.delete("/api/carlos-history")
async def api_carlos_history_delete_all(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    ok = await db.delete_carlos_history_all()
    return {"ok": ok}


# ---------------------------------------------------------------------------
# C1 -> C3 proxy (same-origin for the browser, panel session auth).
# The browser only talks to /api/c3/*; C1 relays to C3 (CARLOS_CODE_URL).
# ---------------------------------------------------------------------------
def _c3_url(path: str) -> str:
    return C3_BASE_URL.rstrip("/") + path


async def _c3_relay_json(method: str, path: str, body: Optional[Dict[str, Any]] = None,
                         timeout: Optional[float] = None):
    """Relay a JSON request to C3 and return its payload with its status."""
    t = timeout or C3_TIMEOUT_S
    try:
        async with httpx.AsyncClient(timeout=t) as client:
            resp = await client.request(method, _c3_url(path), json=body)
    except httpx.TimeoutException:
        return JSONResponse({"error": "C3 timeout"}, status_code=502)
    except Exception as e:  # noqa: BLE001 - fail-open with 502, never leak stack
        return JSONResponse({"error": "C3 unreachable: " + str(e)[:120]}, status_code=502)
    try:
        data = resp.json()
    except Exception:
        data = {"raw": (resp.text or "")[:2000]}
    if isinstance(data, dict):
        return JSONResponse(data, status_code=resp.status_code)
    return JSONResponse({"data": data}, status_code=resp.status_code)


@app.post("/api/c3/stream")
async def api_c3_stream(request: Request):
    """Relay C3 SSE chat stream to the browser. Body: {prompt, section_id?, request_id?, model?}."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "body inválido"}, status_code=400)
    prompt = (body.get("prompt") or "").strip()
    if not prompt:
        return JSONResponse({"error": "prompt vacío"}, status_code=400)
    payload = {
        "prompt": prompt,
        "section_id": body.get("section_id") or "",
        "request_id": body.get("request_id") or ("req-" + uuid.uuid4().hex[:12]),
        "model": body.get("model") or "deepseek-chat",
    }
    if payload["model"] not in ("deepseek-chat", "deepseek-reasoner"):
        payload["model"] = "deepseek-chat"

    async def _gen():
        try:
            timeout = httpx.Timeout(120.0, connect=10.0)
            async with httpx.AsyncClient(timeout=timeout) as client:
                async with client.stream("POST", _c3_url("/chat/stream"), json=payload) as resp:
                    async for chunk in resp.aiter_bytes():
                        if chunk:
                            yield chunk
        except Exception as e:  # noqa: BLE001 - terminate SSE with a done+error event
            yield ("event: done\ndata: " + json.dumps({"error": str(e)[:200],
                     "request_id": payload["request_id"]}) + "\n\n").encode()

    return StreamingResponse(_gen(), media_type="text/event-stream",
                             headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})


@app.post("/api/c3/cancel")
async def api_c3_cancel(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "body inválido"}, status_code=400)
    if not (body.get("request_id") or "").strip():
        return JSONResponse({"error": "request_id requerido"}, status_code=400)
    return await _c3_relay_json("POST", "/chat/cancel", {"request_id": body["request_id"].strip()})


@app.get("/api/c3/sections")
async def api_c3_sections(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return await _c3_relay_json("GET", "/sections")


@app.post("/api/c3/sections/select")
async def api_c3_sections_select(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        body = {}
    return await _c3_relay_json("POST", "/sections/select", body or {})


@app.post("/api/c3/sections/clear")
async def api_c3_sections_clear(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        body = {}
    return await _c3_relay_json("POST", "/sections/clear", body or {})


@app.post("/api/c3/sections/compact")
async def api_c3_sections_compact(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        body = {}
    return await _c3_relay_json("POST", "/sections/compact", body or {}, timeout=90.0)


@app.get("/api/c3/models")
async def api_c3_models(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return await _c3_relay_json("GET", "/models")


@app.get("/api/c3/tools")
async def api_c3_tools(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    return await _c3_relay_json("GET", "/tools")


@app.post("/api/c3/subplans")
async def api_c3_subplans(request: Request):
    """Proxy to C3 POST /subplans: split a plan into sub-prompts (timeout 60s)."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "body inválido"}, status_code=400)
    if not (body.get("prompt") or "").strip():
        return JSONResponse({"error": "prompt vacío"}, status_code=400)
    return await _c3_relay_json("POST", "/subplans", body or {}, timeout=60.0)


# ---------------------------------------------------------------------------
# C1 -> worker-code console proxy (same-origin for the browser, panel session
# auth). The browser only talks to /api/worker/*; C1 relays to the worker's
# own HTTP server. Worker URL resolves from the workers dict (optional "url"
# field) or WORKER_CODE_URL fallback; 404 when the worker is offline.
# ---------------------------------------------------------------------------
WORKER_CONSOLE_BASE = os.environ.get("WORKER_CODE_URL", "") or "https://worker-code.onrender.com"


def _worker_console_base(wid: str) -> Optional[str]:
    w = workers.get(wid)
    if not w:
        return None
    base = (w.get("url") or WORKER_CONSOLE_BASE or "").strip()
    if not base:
        return None
    return base.rstrip("/")


@app.get("/api/worker/{wid}/models")
async def api_worker_models(wid: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    base = _worker_console_base(wid)
    if not base:
        return JSONResponse({"error": "worker offline"}, status_code=404)
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(base + "/models")
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": "worker unreachable: " + str(e)[:120]}, status_code=502)
    try:
        data = resp.json()
    except Exception:
        data = {"raw": (resp.text or "")[:2000]}
    if isinstance(data, list):
        return JSONResponse({"models": data}, status_code=resp.status_code)
    if isinstance(data, dict):
        return JSONResponse(data, status_code=resp.status_code)
    return JSONResponse({"data": data}, status_code=resp.status_code)


@app.post("/api/worker/{wid}/console/clear")
async def api_worker_console_clear(wid: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    base = _worker_console_base(wid)
    if not base:
        return JSONResponse({"error": "worker offline"}, status_code=404)
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.post(base + "/console/clear")
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": "worker unreachable: " + str(e)[:120]}, status_code=502)
    try:
        data = resp.json()
    except Exception:
        data = {"raw": (resp.text or "")[:2000]}
    if isinstance(data, dict):
        return JSONResponse(data, status_code=resp.status_code)
    return JSONResponse({"data": data}, status_code=resp.status_code)


@app.get("/api/worker/{wid}/console/stream")
async def api_worker_console_stream(wid: str, request: Request):
    """Relay worker /console/stream SSE to the browser (session auth)."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    base = _worker_console_base(wid)
    if not base:
        return JSONResponse({"error": "worker offline"}, status_code=404)
    url = base + "/console/stream"

    async def _gen():
        try:
            timeout = httpx.Timeout(120.0, connect=10.0)
            async with httpx.AsyncClient(timeout=timeout) as client:
                async with client.stream("GET", url) as resp:
                    async for chunk in resp.aiter_bytes():
                        if chunk:
                            yield chunk
        except Exception as e:  # noqa: BLE001 - terminate SSE with a done+error event
            yield ("event: done\ndata: " + json.dumps({"error": str(e)[:200]}) + "\n\n").encode()

    return StreamingResponse(_gen(), media_type="text/event-stream",
                             headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})


@app.post("/api/worker/{wid}/reconnect")
async def api_worker_reconnect(wid: str, request: Request):
    """Ask a worker to re-register: enqueue {"tipo": "status"} in its inbox."""
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if wid not in workers and wid not in worker_memory:
        return JSONResponse({"error": "worker no encontrado"}, status_code=404)
    _enqueue(wid, {"tipo": "status"})
    return {"ok": True, "id": wid}


@app.post("/api/worker-weight")
async def api_weight_set(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    body = await request.json()
    wid = body.get("id")
    weight = int(body.get("weight", 10))
    if wid not in workers:
        return JSONResponse({"error": "worker no conectado"}, status_code=404)
    workers[wid]["weight"] = weight
    push_workers()
    return {"id": wid, "weight": weight}


@app.post("/api/task")
async def api_task(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    body = await request.json()
    prompt = (body.get("prompt") or "").strip()
    if not prompt:
        return JSONResponse({"error": "prompt vacio"}, status_code=400)
    # ---- eleccion de lider + conjunto de PCs trabajadoras ----
    principal_req = body.get("principal") or body.get("target") or "auto"
    subset = body.get("workers") or []
    if isinstance(subset, str):
        subset = [subset]
    if subset:
        subset = [w for w in subset if w in workers]
        # una PC no conforme (no respondio handoff) se excluye del conjunto nuevo
        subset = [w for w in subset if not workers[w].get("noncompliant")]
    if principal_req not in ("auto",) and principal_req not in workers:
        return JSONResponse({"error": "lider no conectado: " + str(principal_req)}, status_code=409)
    if subset:
        # el lider tambien trabaja: debe estar dentro del conjunto
        if principal_req not in ("auto",) and principal_req not in subset:
            subset.append(principal_req)
        if not subset:
            return JSONResponse({"error": "sin PCs en el conjunto"}, status_code=409)
        chosen = principal_req if principal_req != "auto" else subset[0]
    else:
        # sin conjunto explicito: comportamiento anterior (una sola PC)
        if principal_req not in ("auto",) and principal_req in workers:
            chosen = principal_req
            subset = [principal_req]
        elif principal_id and principal_id in workers:
            chosen = principal_id
            subset = [principal_id]
        else:
            libres = [w for w in workers if workers[w].get("status") == "disponible"
                      and not workers[w].get("noncompliant")]
            pool = libres or list(workers)
            if not pool:
                return JSONResponse({"error": "no hay PCs conectadas"}, status_code=409)
            chosen = max(pool, key=lambda w: workers[w].get("perf", 0))
            subset = [chosen]

    # la lider es la numero 1; el resto conserva su orden como numeros 2,3,...
    subset = [chosen] + [w for w in subset if w != chosen]

    task_id = uuid.uuid4().hex[:12]
    # C3 sub-prompts approved via /start + /ready (in-memory only: the DB
    # mirror in db.save_task persists core fields like prompt/status/plan).
    raw_subs = body.get("subplans") or []
    subplans = [s.strip() for s in raw_subs if isinstance(s, str) and s.strip()][:8]
    tasks[task_id] = {
        "id": task_id, "prompt": prompt, "status": "planning",
        "principal": chosen, "workers": list(subset),
        "plan": None, "plan_summary": "", "subtasks": {}, "queue": [], "reserved": [],
        "items": {}, "pqueues": {}, "active": {}, "recalling": {}, "handoff_pending": {},
        "remaining": [],
        "subplans": subplans,
        # NOTE: extra field from view-send section selector (Bug 2). Stored for
        # traceability only; the planner path ignores it (no behavior change).
        "section_id": body.get("section_id") or body.get("cc_section_id") or "",
        "mode_requested": (body.get("mode") or mode),
        "weights": body.get("weights") or {},
        "mode_used": None, "created_at": now_iso(), "finished_at": None,
    }
    asyncio.create_task(db.save_task(tasks[task_id]))

    # ── Intentar Carlos Code primero ────────────────────────────────────
    if await carlos_code_available():
        try:
            # Obtener archivos existentes del proyecto
            existing = []
            project_path = body.get("project_path")
            if project_path:
                try:
                    from pathlib import Path
                    p = Path(project_path)
                    if p.exists():
                        existing = [str(f.relative_to(p)) for f in p.rglob("*") if f.is_file()][:50]
                except Exception:
                    pass

            plan = await carlos_code_plan(
                prompt=prompt,
                workers=subset,
                existing_files=existing,
                project_path=project_path,
                section_id=body.get("section_id") or "",
            )

            if plan:
                # Carlos Code devolvió un plan válido — usarlo directamente
                assignments = parse_plan_assignments(plan, subset)
                tasks[task_id]["status"] = "running"
                tasks[task_id]["plan"] = plan
                tasks[task_id]["plan_summary"] = plan.get("note", "")
                tasks[task_id]["mode_used"] = plan.get("mode", "percent")

                # Despachar directamente a workers (sin pasar por el leader)
                asyncio.create_task(db.save_task(tasks[task_id]))

                # Encolar la tarea en cada PC del conjunto
                for wid in subset:
                    workers[wid].setdefault("task_queue", []).append(task_id)

                # Despachar archivos a cada worker (mismo contrato tipo:"tarea"
                # del flujo normal: crea subtask, setea active/estado y encola
                # kind:"work" con files — todo worker lo entiende).
                for wid, files in assignments.items():
                    if wid in workers:
                        for fname in files:
                            _send_one(task_id, wid, fname)

                # Carlos Code is planning — no worker should be marked as occupied;
                # the plan is distributed via Carlos Code's assignments, not a single leader.
                push_workers()
                push_task(task_id)
                broadcast_panel({"tipo": "event", "msg": f"Carlos Code planificó: {plan.get('note', '')[:80]}"})
                return {
                    "task_id": task_id, "principal": None, "workers": list(subset),
                    "status": "running", "source": "carlos-code",
                    "plan": plan,
                }
        except Exception as e:
            logger.warning(f"Carlos Code falló, usando leader: {e}")

    # ── Fallback: leader PC planifica (flujo original) ──────────────────
    # encolar la tarea en cada PC del conjunto (una PC ejecuta una tarea a la vez)
    for wid in subset:
        workers[wid].setdefault("task_queue", []).append(task_id)

    # concurrencia: NO se pausa la tarea anterior; cada PC serializa sus tareas
    _set_status(chosen, "ocupado")
    workers[chosen]["task_id"] = task_id
    # snapshot de status de TODAS las PCs conectadas, para que la lider sepa
    # cuales estan activas al recibir el prompt (spec del usuario).
    status_snapshot = {
        w: {"status": workers[w].get("status", "desconocido"),
            "name": workers[w].get("name", w),
            "index": i + 1}
        for i, w in enumerate(subset)
    }
    wsummary = [{"id": w, "weight": workers[w].get("weight", 10),
                 "status": workers[w].get("status", "desconocido"),
                 "index": i + 1} for i, w in enumerate(subset)]
    _enqueue(chosen, {
        "tipo": "tarea", "task_id": task_id, "kind": "plan", "prompt": prompt,
        "workers": wsummary, "mode": (body.get("mode") or mode), "status_snapshot": status_snapshot,
    })
    push_workers()
    push_task(task_id)
    return {"task_id": task_id, "principal": chosen, "workers": list(subset), "status": "planning"}


# ---------------------------------------------------------------------------
# Accion del worker por HTTP POST (el cliente NO envia frames WS: el proxy de
# Render mata la conexion al recibirlos). Sube registro/resultado/pong/estado.
# ---------------------------------------------------------------------------
@app.post("/api/worker-action")
async def api_worker_action(request: Request):
    tok = request.query_params.get("token", "")
    if not token_ok(tok):
        return JSONResponse({"error": "token invalido"}, status_code=401)
    wid = request.query_params.get("id")
    if not wid:
        return JSONResponse({"error": "falta id"}, status_code=400)
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "json invalido"}, status_code=400)
    tipo = body.get("tipo")
    # asegurar worker (el cliente lo crea con 'registro')
    if wid not in workers:
        if tipo == "registro":
            workers[wid] = _new_worker(wid,
                name=body.get("name", wid),
                perf=int(body.get("perf", 5) or 5),
                weight=int(body.get("weight", 10) or 10),
                brand=body.get("brand", ""), model=body.get("model", ""), os=body.get("os", ""),
            )
            push_workers()
            broadcast_panel({"tipo": "event", "msg": f"Worker {wid} conectado"})
            asyncio.create_task(db.log_event(f"Worker {wid} conectado"))
        else:
            return JSONResponse({"error": "worker no conectado"}, status_code=404)
    workers[wid]["last_poll"] = now_iso()
    _remember(wid)
    if tipo == "resultado":
        await _handle_resultado(body)
    elif tipo == "progress":
        await _handle_progress(body)
    elif tipo == "pong":
        workers[wid]["last_pong"] = now_iso()
    elif tipo == "estado":
        _set_status(wid, body.get("estado", "disponible"))
        push_workers()
    elif tipo == "registro":
        w = workers[wid]
        # una PC que se reconecta (opencode reiniciado) deja de ser no conforme
        w["noncompliant"] = False
        if body.get("name"): w["name"] = body["name"]
        if body.get("perf") is not None: w["perf"] = int(body["perf"])
        if body.get("weight") is not None: w["weight"] = int(body["weight"])
        if body.get("brand"): w["brand"] = body["brand"]
        if body.get("model"): w["model"] = body["model"]
        if body.get("os"): w["os"] = body["os"]
        push_workers()
    else:
        return JSONResponse({"error": "tipo desconocido: " + str(tipo)}, status_code=400)
    return JSONResponse({"ok": True, "id": wid})


@app.get("/api/worker-poll")
async def api_worker_poll(request: Request):
    """Long-polling: el worker drena los comandos acumulados en inbox.

    Cada request es corto (<15s) para sobrevivir al cap de ~20s del proxy de Render.
    """
    tok = request.query_params.get("token", "")
    if not token_ok(tok):
        return JSONResponse({"error": "token invalido"}, status_code=401)
    wid = request.query_params.get("id")
    if not wid:
        return JSONResponse({"error": "falta id"}, status_code=400)
    if wid not in workers:
        workers[wid] = _new_worker(wid)
    workers[wid]["last_poll"] = now_iso()
    try:
        timeout = min(float(request.query_params.get("timeout", "15") or 15), 15)
    except Exception:
        timeout = 15
    deadline = time.time() + timeout
    while time.time() < deadline:
        msgs = inbox.pop(wid, [])
        if msgs:
            return JSONResponse({"messages": msgs})
        await asyncio.sleep(0.3)
    return JSONResponse({"messages": inbox.pop(wid, [])})


@app.get("/api/resources")
async def api_resources(request: Request):
    """Return RAM (used/total/%), uptime for each service + DB size."""
    # Render free tier = 512 MB per service
    TOTAL_RAM_MB = 512.0
    result: Dict[str, Any] = {}
    # C1 own resources: measured directly (no HTTP to self).
    try:
        import resource as resource_module
        try:
            ru = resource_module.getrusage(resource_module.RUSAGE_SELF)
            c1_ram = ru.ru_maxrss / 1024  # KB to MB (Linux)
        except Exception:
            c1_ram = 0.0
        result["C1"] = {
            "ram_mb": round(c1_ram, 1),
            "ram_total_mb": TOTAL_RAM_MB,
            "ram_pct": round(c1_ram / TOTAL_RAM_MB * 100, 1),
            "uptime_sec": time.time() - start_time,
        }
    except Exception as e:
        result["C1"] = {"error": str(e)[:50]}
    # Service URLs for /metrics (empty envs fall back to Render defaults).
    svc_urls = {
        "C2": (os.getenv("GATEWAY_URL") or "https://carlos-gateway.onrender.com") + "/metrics",
        "C3": (os.getenv("CARLOS_CODE_URL") or "https://carlos-code.onrender.com") + "/metrics",
        "C4": (os.getenv("ENGRAM_URL") or "https://engram-mcp-q9ln.onrender.com") + "/metrics",
        "C5": (os.getenv("CONTEXT7_URL") or "https://context7-grep.onrender.com") + "/metrics",
        "C6": "https://worker-code.onrender.com/metrics",
    }
    async with httpx.AsyncClient(timeout=5) as client:
        for name, url in svc_urls.items():
            try:
                resp = await client.get(url)
                if resp.status_code == 200:
                    d = resp.json()
                    ram = d.get("ram_mb", 0)
                    result[name] = {
                        "ram_mb": round(ram, 1),
                        "ram_total_mb": TOTAL_RAM_MB,
                        "ram_pct": round(ram / TOTAL_RAM_MB * 100, 1),
                        "uptime_sec": d.get("uptime_sec"),
                    }
                else:
                    result[name] = {"error": f"HTTP {resp.status_code}", "offline": True}
            except Exception as e:
                result[name] = {"error": str(e)[:50], "offline": True}
    # DB size from Supabase
    db_url = os.getenv("DATABASE_URL", "")
    if db_url:
        try:
            conn = await asyncpg.connect(db_url, timeout=10, statement_cache_size=0)
            try:
                row = await conn.fetchrow(
                    "SELECT pg_database_size(current_database())::bigint as size_bytes"
                )
                result["DB"] = {
                    "size_bytes": row["size_bytes"],
                    "size_human": f"{row['size_bytes'] / 1024 / 1024:.1f} MB",
                }
            finally:
                await conn.close()
        except Exception as e:
            result["DB"] = {"error": str(e)[:80]}
    return JSONResponse(result)


# ---------------------------------------------------------------------------
# Services registry + per-service key/value tables
# ---------------------------------------------------------------------------
# Built-in services (same properties as the Services view). Custom services
# added by the user live in Supabase (custom_services) and merge here.
BUILTIN_SERVICES = [
    {"id": "SVC-C1", "name": "C1 - Panel Web",
     "health_url": "https://bridgecarlos.onrender.com/health",
     "dashboard_url": "https://dashboard.render.com/web/srv-da8k36ajnfac73elnneg",
     "color": "#2563eb"},
    {"id": "SVC-C2", "name": "C2 - Gateway DeepSeek",
     "health_url": "https://carlos-gateway.onrender.com/health",
     "dashboard_url": "https://dashboard.render.com/web/srv-dab7o8cs728c739rt4k0",
     "color": "#7c3aed"},
    {"id": "SVC-C3", "name": "C3 - Carlos Code",
     "health_url": "https://carlos-code.onrender.com/health",
     "dashboard_url": "https://dashboard.render.com/web/srv-daddt02fngtc738vml80",
     "color": "#f59e0b"},
    {"id": "SVC-C4", "name": "C4 - Engram MCP",
     "health_url": "https://engram-mcp-q9ln.onrender.com/health",
     "dashboard_url": "https://dashboard.render.com/web/srv-da9uja142hec7390flu0",
     "color": "#ec4899"},
    {"id": "SVC-C5", "name": "C5 - Context7+grep",
     "health_url": "https://context7-grep.onrender.com/health",
     "dashboard_url": "https://dashboard.render.com/web/srv-dademueq1p3s73dcd4rg",
     "color": "#06b6d4"},
    {"id": "SVC-C6", "name": "C6 - Worker Code",
     "health_url": "https://worker-code.onrender.com/health",
     "dashboard_url": "https://dashboard.render.com/web/srv-dademueq1p3s73dcd4rg",
     "color": "#10b981"},
    {"id": "SVC-DB", "name": "Supabase DB",
     "health_url": "",
     "dashboard_url": "https://supabase.com/dashboard/project/qqaocynurdhtmmxktzcz",
     "color": "#059669"},
]
BUILTIN_SERVICE_IDS = {s["id"] for s in BUILTIN_SERVICES}


def _service_known(service_id: str, customs: list) -> bool:
    if service_id in BUILTIN_SERVICE_IDS:
        return True
    return any(c.get("id") == service_id for c in customs)


def _slug_service_id(name: str) -> str:
    slug = re.sub(r"[^a-z0-9]+", "-", (name or "").lower()).strip("-")
    return "SVC-" + (slug[:24] or "custom")


@app.get("/api/services")
async def api_services_list(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    customs = await db.load_custom_services()
    return {"builtin": BUILTIN_SERVICES, "custom": customs}


@app.post("/api/services")
async def api_services_add(request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "body inválido"}, status_code=400)
    name = (body.get("name") or "").strip()
    health_url = (body.get("health_url") or "").strip()
    if not name or not health_url:
        return JSONResponse({"error": "name y health_url son obligatorios"}, status_code=400)
    sid = (body.get("id") or _slug_service_id(name)).strip().upper()
    if sid in BUILTIN_SERVICE_IDS:
        return JSONResponse({"error": "id reservado por un servicio built-in"}, status_code=409)
    svc = {
        "id": sid, "name": name, "health_url": health_url,
        "dashboard_url": (body.get("dashboard_url") or "").strip(),
        "color": (body.get("color") or "#64748b").strip() or "#64748b",
    }
    await db.save_custom_service(svc)
    return {"ok": True, "service": svc}


@app.delete("/api/services/{service_id}")
async def api_services_delete(service_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if service_id in BUILTIN_SERVICE_IDS:
        return JSONResponse({"error": "los servicios built-in no se eliminan"}, status_code=409)
    ok = await db.delete_custom_service(service_id)
    return {"ok": ok}


@app.get("/api/services/{service_id}/table")
async def api_service_table_load(service_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    customs = await db.load_custom_services()
    if not _service_known(service_id, customs):
        return JSONResponse({"error": "servicio desconocido"}, status_code=404)
    rows = await db.load_service_table(service_id)
    return {"service_id": service_id, "rows": rows}


@app.post("/api/services/{service_id}/table")
async def api_service_table_save(service_id: str, request: Request):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    try:
        body = await request.json()
    except Exception:
        return JSONResponse({"error": "body inválido"}, status_code=400)
    customs = await db.load_custom_services()
    if not _service_known(service_id, customs):
        return JSONResponse({"error": "servicio desconocido"}, status_code=404)
    key = (body.get("key") or "").strip()
    if not key:
        return JSONResponse({"error": "key es obligatoria"}, status_code=400)
    ok = await db.save_service_row(service_id, key, body.get("value") or "")
    return {"ok": ok}


@app.delete("/api/services/{service_id}/table")
async def api_service_table_delete(service_id: str, request: Request, key: str = ""):
    if not valid_session(request.cookies.get("panel_session")):
        return JSONResponse({"error": "no autenticado"}, status_code=401)
    if not key:
        key = request.query_params.get("key", "")
    if not key:
        return JSONResponse({"error": "key es obligatoria"}, status_code=400)
    ok = await db.delete_service_row(service_id, key)
    return {"ok": ok}


# ---------------------------------------------------------------------------
# WebSocket: WORKERS
# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# WORKERS: ya NO usan WebSocket (el proxy de Render corta conexiones persistentes
# a ~20s y mata la conexion al recibir frames del cliente). El worker se da de alta
# con POST /api/worker-action (tipo=registro), drena comandos con GET /api/worker-poll
# (long-polling <=15s) y sube resultado/estado con POST /api/worker-action.
# ---------------------------------------------------------------------------


async def _handle_resultado(msg: Dict[str, Any]) -> None:
    task_id = msg.get("task_id")
    kind = msg.get("kind")
    t = tasks.get(task_id)
    if not t:
        return
    # ignorar resultados de tareas pausadas / ya cerradas (preempcion)
    if t["status"] not in ("planning", "running"):
        return
    if kind == "plan":
        out = msg.get("output", "")
        t["plan"] = parse_plan(out)
        t["plan_summary"] = (t["plan"] or {}).get("note", "") or out[:200]
        # Detectar si OpenCode ya implemento en la misma corrida (modo --pure
        # hace plan + implementacion de golpe). Senales: Edit/Write en el output
        # o el plan no tiene archivos asignables (todo hecho de una).
        has_edits = any(kw in out for kw in ["Edit ", "Write ", "edit ", "write ", "Index: "])
        plan_files = (t.get("plan") or {}).get("assignments") or (t.get("plan") or {}).get("files") or []
        if has_edits and not plan_files:
            # Plan + implementacion en una sola corrida: marcar como done
            t["status"] = "done"
            t["finished_at"] = now_iso()
            asyncio.create_task(db.save_task(t))
            # Fire-and-forget: log task completion to Engram (spec:58-72).
            asyncio.create_task(_engram_save(
                "Task " + task_id + " completed",
                t.get("prompt", "")[:2000],
                ["leader"],
            ))
            push_task(task_id)
            push_workers()
        else:
            t["status"] = "running"
            dispatch_plan(task_id)
            asyncio.create_task(db.save_task(t))
            push_task(task_id)
    else:  # work
        subtask_id = msg.get("subtask_id")
        _on_subtask_done(task_id, subtask_id, msg.get("output", ""), msg.get("status", "done"))


async def _handle_progress(msg: Dict[str, Any]) -> None:
    """Actualiza el output de la subtarea EN VIVO (sin finalizarla)."""
    task_id = msg.get("task_id")
    t = tasks.get(task_id)
    if not t:
        return
    if t["status"] not in ("planning", "running"):
        return
    kind = msg.get("kind")
    out = msg.get("output", "") or ""
    if kind == "plan":
        # borrador de plan en vivo (el panel lo muestra mientras la lider piensa)
        t["plan_summary"] = out[-600:] or t.get("plan_summary", "")
        t["plan_draft"] = out
    else:
        subtask_id = msg.get("subtask_id")
        st = t["subtasks"].get(subtask_id)
        if not st:
            return
        st["output"] = out
        if st["status"] not in ("done", "failed", "returned"):
            st["status"] = "running"
            t["active"][st["worker"]] = subtask_id
    push_task(task_id)


# ---------------------------------------------------------------------------
# Fire-and-forget: log task completion to Engram MCP (spec:58-72).
# Failures are logged only — never break task flow.
# ---------------------------------------------------------------------------
async def _engram_save(title: str, content: str, tags: list) -> None:
    """POST /save to Engram MCP fire-and-forget. 5 s timeout, log-only on failure."""
    if not ENGRAM_URL:
        return
    body = {
        "title": title,
        "type": "task",
        "topic": "tasks/" + title,
        "content": content,
        "tags": tags,
    }
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            await client.post(ENGRAM_URL + "/save", json=body)
    except Exception:  # noqa: BLE001
        pass  # fail-open: log only, never break task flow


# ---------------------------------------------------------------------------
# WebSocket: PANEL
# ---------------------------------------------------------------------------
@app.websocket("/ws/panel")
async def ws_panel(websocket: WebSocket):
    await websocket.accept()
    if not valid_session(websocket.cookies.get("panel_session")):
        await websocket.send_text(json.dumps({"tipo": "error", "msg": "no autenticado"}))
        await websocket.close()
        return
    panel_sockets.add(websocket)
    try:
        await websocket.send_text(json.dumps({"tipo": "workers", "workers": [worker_view(w) for w in workers]}))
        await websocket.send_text(json.dumps({"tipo": "tasks", "tasks": list(tasks.values())}))
        await websocket.send_text(json.dumps({"tipo": "principal_update", "principal": principal_id}))
        await websocket.send_text(json.dumps({"tipo": "mode_update", "mode": mode}))
        while True:
            await websocket.receive_text()
    except WebSocketDisconnect:
        pass
    finally:
        panel_sockets.discard(websocket)


# ---------------------------------------------------------------------------
# Heartbeat
# ---------------------------------------------------------------------------
@app.on_event("startup")
async def _start_heartbeat():
    global principal_id, current_task, mode
    await db.init_db()
    try:
        tasks.update(await db.load_tasks())
    except Exception:  # noqa: BLE001
        pass
    # --- C1 boot recovery: load workers + memory + panel_state from Supabase ---
    if db._ok():
        try:
            restored_workers = await db.load_workers_from_db()
            for wid, snap in restored_workers.items():
                if wid not in workers:
                    # Create a skeleton worker entry; live WS reconnects via heartbeat
                    workers[wid] = _new_worker(wid,
                        name=snap.get("name", wid),
                        perf=int(snap.get("perf", 5)),
                        weight=int(snap.get("weight", 10)),
                        brand=snap.get("brand", ""),
                        model=snap.get("model", ""),
                        os=snap.get("os", ""),
                        status=snap.get("status", "disponible"),
                    )
                    # Restore stats if available
                    if snap.get("stats"):
                        workers[wid]["stats"] = snap["stats"]
                    # Restore worker_memory entry
                    worker_memory[wid] = snap
            if restored_workers:
                print(f"[boot] restored {len(restored_workers)} workers from Supabase")
        except Exception as e:  # noqa: BLE001
            print(f"[boot] worker restore failed: {e}")
        try:
            ps = await db.load_panel_state()
            if ps.get("principal_id"):
                principal_id = ps["principal_id"]
            if ps.get("mode"):
                mode = ps["mode"]
            if ps.get("paused"):
                paused.clear()
                paused.extend(ps["paused"])
            if ps.get("current_task"):
                current_task = ps["current_task"]
        except Exception as e:  # noqa: BLE001
            print(f"[boot] panel_state restore failed: {e}")
    # --- Keepalive: DB first, .cai_keepalive.json file second (design:48-54) ---
    try:
        k = await db.load_keepalive()
        KEEPALIVE_STATE["enabled"] = k.get("enabled", False)
        KEEPALIVE_STATE["last_run"] = k.get("last_run", "")
        KEEPALIVE_STATE["services"] = k.get("services", {})
    except Exception:  # noqa: BLE001
        pass
    # File fallback if DB didn't have anything
    if not KEEPALIVE_STATE["enabled"] and not KEEPALIVE_STATE["last_run"]:
        try:
            fk = db.load_keepalive_file()
            if fk:
                KEEPALIVE_STATE["enabled"] = fk.get("enabled", False)
                KEEPALIVE_STATE["last_run"] = fk.get("last_run", "")
                KEEPALIVE_STATE["services"] = fk.get("services", {})
        except Exception:  # noqa: BLE001
            pass
    asyncio.create_task(_heartbeat_loop())
    asyncio.create_task(_rebalance_loop())
    asyncio.create_task(_keepalive_loop())
    asyncio.create_task(_debounce_flush_loop())
    asyncio.create_task(_history_cleanup_loop())


async def _recover_tasks():
    """Re-planifica una sola vez tareas atascadas >120s en planning cuya lider ya reconecto."""
    now = datetime.now(timezone.utc)
    for tid, t in list(tasks.items()):
        if t.get("status") != "planning" or t.get("_recovered_plan"):
            continue
        p = t.get("principal")
        if p not in workers:
            continue
        try:
            created = datetime.fromisoformat(t.get("created_at"))
            if created.tzinfo is None:
                created = created.replace(tzinfo=timezone.utc)
            if (now - created).total_seconds() < 120:
                continue
        except Exception:
            continue
        wsummary = [{"id": w, "weight": workers[w].get("weight", 10)} for w in t.get("workers", [])]
        _enqueue(p, {"tipo": "tarea", "task_id": tid, "kind": "plan", "prompt": t["prompt"],
                     "workers": wsummary, "mode": t.get("mode_requested") or mode})
        t["_recovered_plan"] = True
        push_task(tid)
        asyncio.create_task(db.log_event(f"Recuperacion: tarea {tid[:6]} pegada en planning, re-planificando"))


async def _heartbeat_loop():
    """Heartbeat: limpia workers caidos y recupera tareas stuck."""
    while True:
        await asyncio.sleep(HEARTBEAT_INTERVAL)
        now = datetime.now(timezone.utc)
        # recuperar tareas stuck en planning
        await _recover_tasks()
        # limpiar workers inactivos
        for wid, w in list(workers.items()):
            last = w.get("last_poll")
            age = 0.0
            if last:
                try:
                    age = (now - datetime.fromisoformat(last)).total_seconds()
                except Exception:
                    age = 0.0
            if age <= WORKER_TIMEOUT:
                continue
            # worker caido por inactividad de poll
            t = w.get("task_id")
            if t and t in tasks:
                for st in tasks[t]["subtasks"].values():
                    if st["worker"] == wid and st["status"] == "running":
                        st["status"] = "failed"
                if tasks[t]["status"] in ("planning", "running"):
                    tasks[t]["status"] = "failed"
                    tasks[t]["finished_at"] = now_iso()
                    asyncio.create_task(db.save_task(tasks[t]))
                    push_task(t)
            if principal_id == wid:
                principal_id = None
                broadcast_panel({"tipo": "principal_update", "principal": None})
            inbox.pop(wid, None)
            if wid in worker_memory:
                worker_memory[wid]["connected"] = False
                worker_memory[wid]["status"] = "down"
                worker_memory[wid]["last_seen"] = now_iso()
            del workers[wid]
            _dirty_workers.add(wid)
            push_workers()
            broadcast_panel({"tipo": "event", "msg": f"Worker {wid} caido (timeout poll)"})
            asyncio.create_task(db.log_event(f"Worker {wid} caido (timeout poll)"))


async def _keepalive_loop() -> None:
    """Cada 5 min (si el toggle está ON) hace ping/pong de estado con cada
    servicio listado: C1 envía GET al health del servicio y el servicio
    responde su estado. SVC-DB usa SELECT 1 en el pool. Mantiene los
    servicios despiertos (evita spin-down de Render free) y registra disponibilidad."""
    while True:
        await asyncio.sleep(KEEPALIVE_INTERVAL_SEC)
        if not KEEPALIVE_STATE["enabled"]:
            continue
        KEEPALIVE_STATE["last_run"] = now_iso()
        for svc in KEEPALIVE_SERVICES:
            t0 = time.time()
            status = "error"
            if svc.get("db_check"):
                # SVC-DB: SELECT 1 on asyncpg pool
                try:
                    if db._ok():
                        async with db._pool.acquire() as c:
                            await c.fetchval("SELECT 1")
                        status = "ok"
                except Exception:  # noqa: BLE001
                    status = "error"
            else:
                # HTTP health check for SVC-C1..C6
                try:
                    async with httpx.AsyncClient(timeout=KEEPALIVE_TIMEOUT_SEC) as client:
                        r = await client.get(svc["url"])
                        if r.status_code < 500:
                            status = "ok"
                except Exception:  # noqa: BLE001 (timeout, dns, conexion)
                    status = "error"
            latency_ms = round((time.time() - t0) * 1000)
            KEEPALIVE_STATE["services"][svc["id"]] = {
                "name": svc["name"],
                "url": svc["url"],
                "status": status,
                "latency_ms": latency_ms,
                "checked_at": now_iso(),
            }
        asyncio.create_task(db.save_keepalive(
            KEEPALIVE_STATE["enabled"],
            KEEPALIVE_STATE["last_run"],
            KEEPALIVE_STATE["services"],
        ))
        # Also persist to .cai_keepalive.json file fallback
        db.save_keepalive_file(
            KEEPALIVE_STATE["enabled"],
            KEEPALIVE_STATE["last_run"],
            KEEPALIVE_STATE["services"],
        )


async def _rebalance_loop() -> None:
    """Cada 10s rebalancea las tareas en ejecucion (robo de cola + timeout)."""
    while True:
        await asyncio.sleep(10)
        for tid, t in list(tasks.items()):
            if t.get("status") == "running":
                try:
                    _rebalance(tid)
                    _preempt_by_idle(tid)
                except Exception:
                    pass


# ---------------------------------------------------------------------------
# Debounce flush: batch worker state writes to Supabase every 5s (design:109)
# ---------------------------------------------------------------------------
_DEBOUNCE_INTERVAL = 5  # seconds


async def _debounce_flush_loop() -> None:
    """Periodic flush of dirty workers to Supabase. Fail-open: skip on error."""
    while True:
        await asyncio.sleep(_DEBOUNCE_INTERVAL)
        if not _dirty_workers:
            continue
        # Snapshot and clear dirty set
        batch = list(_dirty_workers)
        _dirty_workers.clear()
        # Flush each dirty worker
        for wid in batch:
            w = workers.get(wid)
            if w:
                # Build full snapshot for worker_memory table
                snap = {
                    "id": wid, "name": w.get("name", wid), "perf": w.get("perf", 5),
                    "weight": w.get("weight", 10), "brand": w.get("brand", ""),
                    "model": w.get("model", ""), "os": w.get("os", ""),
                    "status": w.get("status", "disponible"),
                    "connected": True, "last_seen": now_iso(),
                    "task_id": w.get("task_id"),
                    "stats": w.get("stats", {"tasks": 0, "words": 0, "tokens": 0}),
                }
                try:
                    await db.save_worker_full(wid, snap)
                except Exception:  # noqa: BLE001
                    pass  # fail-open
            elif wid in worker_memory:
                # Worker disconnected but memory entry exists — persist it
                try:
                    await db.save_worker_full(wid, worker_memory[wid])
                except Exception:  # noqa: BLE001
                    pass
        # Persist panel state snapshot
        try:
            await db.save_panel_state({
                "principal_id": principal_id,
                "mode": mode,
                "paused": list(paused),
                "current_task": current_task,
            })
        except Exception:  # noqa: BLE001
            pass


# ---------------------------------------------------------------------------
# History retention cleanup: delete rows older than 30 days (hourly)
# ---------------------------------------------------------------------------
async def _history_cleanup_loop() -> None:
    """Hourly cleanup of gateway_history and carlos_code_history (30-day retention)."""
    while True:
        await asyncio.sleep(3600)  # every hour
        try:
            await db.cleanup_old_history()
        except Exception:  # noqa: BLE001
            pass


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=PORT)
