"""
Context7 + grep_app — Herramientas de conocimiento para Carlos Code.

Servicio unificado que provee:
  - Context7: documentación de librerías y frameworks
  - grep_app: búsqueda de código en repositorios de GitHub

Uso:
  python server.py
  # o
  uvicorn server:app --host 0.0.0.0 --port 8004

Endpoints:
  GET  /context7/docs       — buscar documentación de una librería
  GET  /context7/resolve    — resolver nombre de paquete a ID
  GET  /grep_app/search     — buscar código en GitHub
  GET  /search              — búsqueda unificada
  GET  /tools               — listar herramientas MCP (hub F6)
  POST /git/{action}        — git hub: status, list, read, commit, update, delete, commits
  POST /render/{action}     — render: list_services, get_service, list_deploys, trigger_deploy, get_logs
  GET  /health              — health check
  GET  /metrics             — RAM usage and uptime
"""

import base64
import os
import json
import time
import logging
import httpx
from typing import Any, Dict, Optional
from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Query, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

# ── Config ──────────────────────────────────────────────────────────────────

GITHUB_TOKEN = os.getenv("GITHUB_TOKEN", "")
CONTEXT7_API = os.getenv("CONTEXT7_API", "https://api.context7.com")
GREP_APP_API = os.getenv("GREP_APP_API", "https://grep.app/api")
GITHUB_API = "https://api.github.com"
RENDER_API = os.getenv("RENDER_API", "https://api.render.com/v1")


def _github_token() -> str:
    # Read at request time so tests/ops can set the env after import.
    return os.getenv("GITHUB_TOKEN", GITHUB_TOKEN)


def _repo_owner() -> str:
    return os.getenv("REPO_OWNER", "1000carlospena-prog")


def _repo_name() -> str:
    return os.getenv("REPO_NAME", "cai")


def _render_api_key() -> str:
    return os.getenv("RENDER_API_KEY", "")


def _render_owner_id() -> str:
    return os.getenv("RENDER_OWNER_ID", "")

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("context7-grep")


@asynccontextmanager
async def lifespan(app: FastAPI):
    log.info("Context7 + grep_app arrancado")
    yield
    log.info("Context7 + grep_app apagado")


app = FastAPI(title="Context7 + grep_app", lifespan=lifespan)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)


# ── Health ──────────────────────────────────────────────────────────────────

@app.get("/health")
async def health():
    return {
        "status": "ok",
        "context7_api": CONTEXT7_API,
        "grep_app_api": GREP_APP_API,
        "github_token_set": bool(GITHUB_TOKEN),
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


_start_time = time.time()


# ── Context7: Documentación ────────────────────────────────────────────────

@app.get("/context7/resolve")
async def context7_resolve(
    query: str = Query(..., description="Nombre de la librería a resolver"),
):
    """Resuelve un nombre de paquete a un ID de Context7."""
    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            resp = await client.get(
                f"{CONTEXT7_API}/resolve",
                params={"query": query},
            )
            if resp.status_code == 200:
                return resp.json()
            else:
                return {"error": f"Context7 error: {resp.status_code}", "fallback": True}
    except Exception as e:
        log.warning(f"Context7 resolve falló: {e}")
        return {"error": str(e), "fallback": True}


@app.get("/context7/docs")
async def context7_docs(
    library: str = Query(..., description="ID de la librería (ej: /vercel/next.js)"),
    query: str = Query("", description="Topic específico a buscar"),
    tokens: int = Query(10000, ge=1000, le=100000),
):
    """Obtiene documentación de una librería."""
    try:
        async with httpx.AsyncClient(timeout=30.0) as client:
            resp = await client.get(
                f"{CONTEXT7_API}/docs",
                params={"library": library, "query": query, "tokens": tokens},
            )
            if resp.status_code == 200:
                data = resp.json()
                return {
                    "library": library,
                    "query": query,
                    "docs": data.get("docs", data),
                    "source": "context7",
                }
            else:
                return {
                    "error": f"Context7 error: {resp.status_code}",
                    "fallback": True,
                    "library": library,
                }
    except Exception as e:
        log.warning(f"Context7 docs falló: {e}")
        return {"error": str(e), "fallback": True, "library": library}


# ── grep_app: Búsqueda en GitHub ───────────────────────────────────────────

@app.get("/grep_app/search")
async def grep_app_search(
    q: str = Query(..., description="Texto a buscar"),
    lang: Optional[str] = Query(None, description="Filtrar por lenguaje"),
    repo: Optional[str] = Query(None, description="Filtrar por repositorio"),
    page: int = Query(1, ge=1),
    per_page: int = Query(20, ge=1, le=100),
):
    """Busca código en repositorios de GitHub vía grep.app."""
    headers = {}
    if GITHUB_TOKEN:
        headers["Authorization"] = f"token {GITHUB_TOKEN}"

    params = {"q": q, "page": page, "per_page": per_page}
    if lang:
        params["lang"] = lang
    if repo:
        params["repo"] = repo

    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            resp = await client.get(
                f"{GREP_APP_API}/search",
                headers=headers,
                params=params,
            )
            if resp.status_code == 200:
                data = resp.json()
                return {
                    "query": q,
                    "results": data.get("results", []),
                    "total": data.get("total", 0),
                    "source": "grep_app",
                }
            else:
                return {
                    "error": f"grep_app error: {resp.status_code}",
                    "fallback": True,
                    "query": q,
                }
    except Exception as e:
        log.warning(f"grep_app search falló: {e}")
        return {"error": str(e), "fallback": True, "query": q}


# ── Búsqueda unificada ─────────────────────────────────────────────────────

@app.get("/search")
async def unified_search(
    q: str = Query(..., description="Texto a buscar"),
    source: str = Query("all", description=" Fuente: context7, grep_app, o all"),
):
    """Búsqueda unificada en todas las fuentes de conocimiento."""
    results = {}

    if source in ("context7", "all"):
        try:
            async with httpx.AsyncClient(timeout=15.0) as client:
                resp = await client.get(
                    f"{CONTEXT7_API}/resolve",
                    params={"query": q},
                )
                if resp.status_code == 200:
                    results["context7"] = resp.json()
                else:
                    results["context7"] = {"error": resp.status_code}
        except Exception as e:
            results["context7"] = {"error": str(e)}

    if source in ("grep_app", "all"):
        try:
            async with httpx.AsyncClient(timeout=15.0) as client:
                resp = await client.get(
                    f"{GREP_APP_API}/search",
                    params={"q": q, "per_page": 5},
                )
                if resp.status_code == 200:
                    data = resp.json()
                    results["grep_app"] = {
                        "results": data.get("results", [])[:5],
                        "total": data.get("total", 0),
                    }
                else:
                    results["grep_app"] = {"error": resp.status_code}
        except Exception as e:
            results["grep_app"] = {"error": str(e)}

    return {"query": q, "sources": results}


# ── MCP hub (F6): tool catalog ────────────────────────────────────────────

def _tool(name: str, description: str, properties: Dict[str, Any],
          required: Optional[list] = None) -> Dict[str, Any]:
    schema: Dict[str, Any] = {"type": "object", "properties": properties}
    if required:
        schema["required"] = required
    return {"name": name, "description": description, "parameters": schema}


def _str_prop(description: str) -> Dict[str, Any]:
    return {"type": "string", "description": description}


@app.get("/tools")
async def list_tools():
    """Return the MCP tool catalog (parsed by C3 MCPClient.GetTools)."""
    tools = [
        _tool("context7_resolve", "Resolve a package name to a Context7 library ID",
              {"query": _str_prop("Library name to resolve")}, ["query"]),
        _tool("context7_docs", "Fetch documentation for a library",
              {"library": _str_prop("Library ID (e.g. /vercel/next.js)"),
               "query": _str_prop("Specific topic to search"),
               "tokens": {"type": "integer", "description": "Max tokens (1000-100000)"}},
              ["library"]),
        _tool("grep_search", "Search code in GitHub repos via grep.app",
              {"q": _str_prop("Text to search"),
               "lang": _str_prop("Filter by language"),
               "repo": _str_prop("Filter by repository"),
               "page": {"type": "integer", "description": "Page number"},
               "per_page": {"type": "integer", "description": "Results per page"}},
              ["q"]),
        _tool("unified_search", "Unified search across all knowledge sources",
              {"q": _str_prop("Text to search"),
               "source": _str_prop("Source: context7, grep_app, or all")}, ["q"]),
        _tool("git_status", "Get GitHub repo info (name, branch, visibility)",
              {}),
        _tool("git_list", "List files in a directory on GitHub",
              {"path": _str_prop("Directory path (empty = root)")}),
        _tool("git_read", "Read file contents from GitHub",
              {"path": _str_prop("File path to read")}, ["path"]),
        _tool("git_commit", "Create or update a file on GitHub (auto-commits)",
              {"path": _str_prop("File path to create/update"),
               "content": _str_prop("File content"),
               "message": _str_prop("Commit message")}, ["path", "content"]),
        _tool("git_delete", "Delete a file on GitHub",
              {"path": _str_prop("File path to delete"),
               "message": _str_prop("Commit message"),
               "sha": _str_prop("Current file SHA (from git_read)")},
              ["path", "message", "sha"]),
        _tool("git_commits", "List recent commits on GitHub",
              {"limit": _str_prop("Number of commits to return (default: 10)")}),
        _tool("render_list_services", "List Render services",
              {"limit": _str_prop("Max services to return (default: 20)")}),
        _tool("render_get_service", "Get a Render service by ID",
              {"serviceId": _str_prop("Render service ID")}, ["serviceId"]),
        _tool("render_list_deploys", "List deploys for a Render service",
              {"serviceId": _str_prop("Render service ID"),
               "limit": _str_prop("Max deploys to return (default: 10)")},
              ["serviceId"]),
        _tool("render_trigger_deploy", "Trigger a new deploy for a Render service",
              {"serviceId": _str_prop("Render service ID"),
               "clearCache": _str_prop("Set to 'clear' to clear build cache")},
              ["serviceId"]),
        _tool("render_get_logs", "Get logs for a Render service",
              {"serviceId": _str_prop("Render service ID"),
               "limit": _str_prop("Max log lines (default: 100)"),
               "direction": _str_prop("backward (newest first) or forward"),
               "text": _str_prop("Filter logs by text"),
               "level": _str_prop("Filter logs by severity level")},
              ["serviceId"]),
    ]
    return {"tools": tools}


# ── GitHub helpers (ported from C3 carlos_code gh* — no token in output) ────

async def _gh_api(method: str, path: str, body: Optional[Dict[str, Any]] = None):
    token = _github_token()
    if not token:
        return None, 0, "GITHUB_TOKEN not configured"
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    try:
        async with httpx.AsyncClient(timeout=30.0) as client:
            resp = await client.request(method, f"{GITHUB_API}{path}",
                                        headers=headers, json=body)
            try:
                data = resp.json()
            except Exception:
                data = {"raw": resp.text}
            return data, resp.status_code, None
    except Exception as e:
        log.warning("GitHub API %s %s failed: %s", method, path, type(e).__name__)
        return None, 0, "GitHub request failed"


def _gh_repo_path(path: str) -> str:
    owner, repo = _repo_owner(), _repo_name()
    suffix = path.strip("/") if path else ""
    base = f"/repos/{owner}/{repo}/contents"
    return f"{base}/{suffix}" if suffix else base


async def _gh_repo_info() -> Dict[str, Any]:
    data, status, err = await _gh_api(
        "GET", f"/repos/{_repo_owner()}/{_repo_name()}")
    if err:
        return {"error": err}
    if status != 200:
        return {"error": "GitHub error", "status": status}
    return {
        "name": data.get("name"),
        "full_name": data.get("full_name"),
        "default_branch": data.get("default_branch"),
        "private": data.get("private"),
        "updated_at": data.get("updated_at"),
    }


async def _gh_list_files(path: str) -> Dict[str, Any]:
    data, status, err = await _gh_api("GET", _gh_repo_path(path or ""))
    if err:
        return {"error": err}
    if status != 200:
        return {"error": "GitHub error", "status": status}
    entries = data if isinstance(data, list) else [data]
    files = [{"name": f.get("name"), "path": f.get("path"),
              "type": f.get("type"), "size": f.get("size"),
              "sha": f.get("sha")} for f in entries]
    return {"files": files, "count": len(files)}


async def _gh_read_file(path: str) -> Dict[str, Any]:
    if not path:
        return {"error": "path is required"}
    data, status, err = await _gh_api("GET", _gh_repo_path(path))
    if err:
        return {"error": err}
    if status != 200:
        return {"error": "GitHub error", "status": status}
    raw = (data.get("content") or "").strip()
    try:
        content = base64.b64decode(raw).decode("utf-8", errors="replace")
    except Exception:
        content = ""
    return {"name": data.get("name"), "path": data.get("path"),
            "content": content, "sha": data.get("sha"), "size": data.get("size")}


async def _gh_commit_file(path: str, content: str, message: str) -> Dict[str, Any]:
    if not path:
        return {"error": "path is required", "success": False}
    if message == "":
        message = f"Update {path}"
    api_path = _gh_repo_path(path)
    sha = ""
    existing, status, _ = await _gh_api("GET", api_path)
    if status == 200 and isinstance(existing, dict) and existing.get("sha"):
        sha = existing["sha"]
    body: Dict[str, Any] = {
        "message": message,
        "content": base64.b64encode(content.encode("utf-8")).decode("ascii"),
    }
    if sha:
        body["sha"] = sha
    data, put_status, err = await _gh_api("PUT", api_path, body)
    if err:
        return {"error": err, "success": False}
    if put_status not in (200, 201):
        return {"error": "GitHub error", "status": put_status, "success": False}
    commit = (data or {}).get("commit", {})
    file_info = (data or {}).get("content", {})
    return {"success": True, "commit_sha": commit.get("sha"),
            "file_sha": file_info.get("sha"), "path": path}


async def _gh_delete_file(path: str, message: str, sha: str) -> Dict[str, Any]:
    if not path:
        return {"error": "path is required", "success": False}
    if not sha:
        return {"error": "sha is required for delete", "success": False}
    if message == "":
        message = f"Delete {path}"
    _, status, err = await _gh_api(
        "DELETE", _gh_repo_path(path), {"message": message, "sha": sha})
    if err:
        return {"error": err, "success": False}
    if status != 200:
        return {"error": "GitHub error", "status": status, "success": False}
    return {"success": True, "path": path}


async def _gh_list_commits(limit: str) -> Dict[str, Any]:
    per_page = (limit or "10").strip() or "10"
    data, status, err = await _gh_api(
        "GET", f"/repos/{_repo_owner()}/{_repo_name()}/commits?per_page={per_page}")
    if err:
        return {"error": err}
    if status != 200:
        return {"error": "GitHub error", "status": status}
    commits = []
    for c in data or []:
        sha = c.get("sha", "")
        info = c.get("commit", {})
        author = info.get("author", {})
        commits.append({"sha": sha[:8], "message": info.get("message"),
                        "author": author.get("name"), "date": author.get("date")})
    return {"commits": commits, "count": len(commits)}


@app.post("/git/{action}")
async def git_action(action: str, request: Request):
    """Git hub actions ported from C3. Body: JSON with path/content/message/sha/limit."""
    try:
        args = await request.json()
    except Exception:
        args = {}
    if not isinstance(args, dict):
        args = {}
    if _github_token() == "":
        return {"error": "GITHUB_TOKEN not configured"}

    if action == "status":
        return await _gh_repo_info()
    if action == "list":
        return await _gh_list_files(str(args.get("path", "") or ""))
    if action == "read":
        return await _gh_read_file(str(args.get("path", "") or ""))
    if action in ("commit", "update"):
        return await _gh_commit_file(
            str(args.get("path", "") or ""),
            str(args.get("content", "") or ""),
            str(args.get("message", "") or ""))
    if action == "delete":
        return await _gh_delete_file(
            str(args.get("path", "") or ""),
            str(args.get("message", "") or ""),
            str(args.get("sha", "") or ""))
    if action == "commits":
        return await _gh_list_commits(str(args.get("limit", "10") or "10"))
    return {"error": f"unknown action: {action}"}


# ── Render helpers (https://api.render.com/v1, Bearer RENDER_API_KEY) ───────

async def _render_api(method: str, path: str,
                      params: Optional[Dict[str, Any]] = None,
                      body: Optional[Dict[str, Any]] = None):
    key = _render_api_key()
    if not key:
        return None, 0, "RENDER_API_KEY not configured"
    headers = {"Authorization": f"Bearer {key}", "Accept": "application/json"}
    try:
        async with httpx.AsyncClient(timeout=30.0) as client:
            resp = await client.request(method, f"{RENDER_API}{path}",
                                        headers=headers, params=params, json=body)
            try:
                data = resp.json()
            except Exception:
                data = {"raw": resp.text}
            return data, resp.status_code, None
    except Exception:
        log.warning("Render API %s %s failed", method, path)
        return None, 0, "Render request failed"


@app.post("/render/{action}")
async def render_action(action: str, request: Request):
    """Render actions. Body: JSON with serviceId/limit/clearCache/log filters."""
    try:
        args = await request.json()
    except Exception:
        args = {}
    if not isinstance(args, dict):
        args = {}
    if _render_api_key() == "":
        return {"error": "RENDER_API_KEY not configured"}
    owner_id = _render_owner_id()

    if action == "list_services":
        params: Dict[str, Any] = {"limit": str(args.get("limit", "20") or "20")}
        if owner_id:
            params["ownerId"] = owner_id
        data, status, err = await _render_api("GET", "/services", params=params)
        if err:
            return {"error": err}
        if status != 200:
            return {"error": "Render error", "status": status}
        return {"services": data, "count": len(data) if isinstance(data, list) else 0}

    if action == "get_service":
        service_id = str(args.get("serviceId", "") or "")
        if not service_id:
            return {"error": "serviceId is required"}
        data, status, err = await _render_api("GET", f"/services/{service_id}")
        if err:
            return {"error": err}
        if status != 200:
            return {"error": "Render error", "status": status}
        return data

    if action == "list_deploys":
        service_id = str(args.get("serviceId", "") or "")
        if not service_id:
            return {"error": "serviceId is required"}
        params = {"limit": str(args.get("limit", "10") or "10")}
        data, status, err = await _render_api(
            "GET", f"/services/{service_id}/deploys", params=params)
        if err:
            return {"error": err}
        if status != 200:
            return {"error": "Render error", "status": status}
        return {"deploys": data, "count": len(data) if isinstance(data, list) else 0}

    if action == "trigger_deploy":
        service_id = str(args.get("serviceId", "") or "")
        if not service_id:
            return {"error": "serviceId is required"}
        body: Dict[str, Any] = {}
        if str(args.get("clearCache", "") or "") == "clear":
            body["clearCache"] = "clear"
        data, status, err = await _render_api(
            "POST", f"/services/{service_id}/deploys", body=body or None)
        if err:
            return {"error": err}
        if status not in (200, 201, 202):
            return {"error": "Render error", "status": status}
        return data

    if action == "get_logs":
        service_id = str(args.get("serviceId", "") or "")
        if not service_id:
            return {"error": "serviceId is required"}
        params = {"limit": str(args.get("limit", "100") or "100")}
        for key in ("direction", "startTime", "endTime", "text", "level", "type", "instance"):
            if args.get(key):
                params[key] = str(args[key])
        data, status, err = await _render_api(
            "GET", f"/services/{service_id}/logs", params=params)
        if err:
            return {"error": err}
        if status != 200:
            return {"error": "Render error", "status": status}
        return {"logs": data, "serviceId": service_id}

    return {"error": f"unknown action: {action}"}


# ── Main ────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    import uvicorn

    port = int(os.getenv("PORT", "8004"))
    uvicorn.run(app, host="0.0.0.0", port=port)
