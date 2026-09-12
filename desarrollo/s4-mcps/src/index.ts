import { config } from "./config";
import { info } from "./logger";
import { isPreflight, preflightResponse, withCORS } from "./cors";

function buildServer() {
  return Bun.serve({
    port: config.port,
    async fetch(req) {
      if (isPreflight(req)) return preflightResponse(req.headers.get("origin"));
      const res = await dispatch(req);
      return withCORS(res, req.headers.get("origin"));
    },
  });
}

async function dispatch(req: Request): Promise<Response> {
  const url = new URL(req.url);
  const path = url.pathname;

      if (req.method === "GET" && path === "/health")
        return Response.json({ ok: true, servicio: "s4-mcps", estado: "up", ram_mb: Math.round(process.memoryUsage().rss / 1024 / 1024) });

      if (req.method === "GET" && path === "/estado")
        return Response.json({ servicio: "s4-mcps", engram: config.engramUrl });

      if (req.method === "GET" && path === "/mcp/list") {
        const engramUp = await fetch(`${config.engramUrl}/stats`, { signal: AbortSignal.timeout(3000) }).then(r => r.ok).catch(() => false);
        return Response.json({
          mcps: [
            { id: "engram", nombre: "Engram Memory", upstream: config.engramUrl, upstream_ok: engramUp },
          ],
        });
      }

      return Response.json({ ok: false, error: "ruta desconocida" }, { status: 404 });
}

const srv = buildServer();
info(`s4-mcps escuchando en http://localhost:${srv.port}`);
