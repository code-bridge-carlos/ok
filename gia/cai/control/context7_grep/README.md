# Context7 + grep_app — MCP hub (F6)

Unified knowledge + repo + Render hub. REST FastAPI service.

## Run

```bash
python server.py
# or
uvicorn server:app --host 0.0.0.0 --port 8004
```

## Endpoints

| Method | Path | Description |
| ------ | ---- | ----------- |
| GET | `/context7/resolve?query=` | Resolve package name to Context7 ID |
| GET | `/context7/docs?library=&query=&tokens=` | Fetch library docs |
| GET | `/grep_app/search?q=&lang=&repo=&page=&per_page=` | Search code via grep.app |
| GET | `/search?q=&source=` | Unified search (`context7`, `grep_app`, `all`) |
| GET | `/tools` | MCP tool catalog `{tools: [{name, description, parameters}]}` (parsed by C3 `MCPClient.GetTools` via `GET {url}/tools`) |
| POST | `/git/{action}` | Git actions: `status`, `list`, `read`, `commit`, `update`, `delete`, `commits`. JSON body: `path`, `content`, `message`, `sha`, `limit` |
| POST | `/render/{action}` | Render actions: `list_services`, `get_service`, `list_deploys`, `trigger_deploy`, `get_logs`. JSON body: `serviceId`, `limit`, `clearCache`, log filters |
| GET | `/health` | Health check |
| GET | `/metrics` | RAM usage and uptime |

## Env vars

| Var | Required | Description |
| --- | -------- | ----------- |
| `PORT` | no (default `8004`) | Listen port |
| `GITHUB_TOKEN` | for `/git/*` | GitHub token, sent as `Bearer`, never logged or returned |
| `REPO_OWNER` | no (default `1000carlospena-prog`) | GitHub repo owner (same default as C3) |
| `REPO_NAME` | no (default `cai`) | GitHub repo name (same default as C3) |
| `RENDER_API_KEY` | for `/render/*` | Render token, sent as `Authorization: Bearer`, never logged or returned |
| `RENDER_OWNER_ID` | no | Optional owner filter for `list_services` |
| `CONTEXT7_API` | no | Override Context7 base URL |
| `GREP_APP_API` | no | Override grep.app base URL |
| `RENDER_API` | no (default `https://api.render.com/v1`) | Override Render base URL |

## Examples

```bash
curl localhost:8004/tools
curl -X POST localhost:8004/git/status -H 'Content-Type: application/json' -d '{}'
curl -X POST localhost:8004/git/read -H 'Content-Type: application/json' -d '{"path":"README.md"}'
curl -X POST localhost:8004/render/list_services -H 'Content-Type: application/json' -d '{"limit":"20"}'
```

Errors are `{"error": ...}` and never include tokens. Each upstream call uses a per-request HTTP client with a 30s timeout.
