import { config } from "./config";
import { info, warn } from "./logger";
import { isPreflight, preflightResponse, withCORS } from "./cors";

async function engramStats() {
  const r = await fetch(`${config.engramUrl}/stats`, { signal: AbortSignal.timeout(5000) });
  if (!r.ok) return null;
  return await r.json() as { total_sessions: number; total_observations: number; total_prompts: number; projects: string[] };
}

async function engramObservations(limit = 20) {
  const r = await fetch(`${config.engramUrl}/observations`, { signal: AbortSignal.timeout(5000) });
  if (!r.ok) return [];
  const all = await r.json() as Array<{ id: string; title: string; type: string; created_at: string }>;
  return all.slice(-limit).reverse();
}

function engramAlive(): Promise<boolean> {
  return fetch(`${config.engramUrl}/health`, { signal: AbortSignal.timeout(3000) })
    .then(r => r.ok).catch(() => false);
}

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

      if (req.method === "GET" && path === "/health") {
        const alive = await engramAlive();
        return Response.json({
          ok: alive, servicio: "s3-engram",
          estado: alive ? "up" : "down",
          engram_upstream: alive ? "connected" : "disconnected",
          ram_mb: Math.round(process.memoryUsage().rss / 1024 / 1024),
        });
      }

      if (req.method === "GET" && path === "/estado") {
        const stats = await engramStats().catch(() => null);
        return Response.json({
          servicio: "s3-engram", engram_upstream: config.engramUrl,
          stats: stats ?? "upstream no disponible",
        });
      }

      // Proxy stats from engram upstream
      if (req.method === "GET" && path === "/stats") {
        const r = await fetch(`${config.engramUrl}/stats`, { signal: AbortSignal.timeout(5000) }).catch(() => null);
        if (r && r.ok) return new Response(r.body, { headers: { "Content-Type": "application/json" } });
        return Response.json({ error: "upstream no disponible" }, { status: 502 });
      }

      // Proxy observations from engram upstream
      if (req.method === "GET" && path === "/observations") {
        const r = await fetch(`${config.engramUrl}/observations`, { signal: AbortSignal.timeout(8000) }).catch(() => null);
        if (r && r.ok) return new Response(r.body, { headers: { "Content-Type": "application/json" } });
        return Response.json({ error: "upstream no disponible" }, { status: 502 });
      }

      // s3 es infraestructura (memoria Engram): solo status JSON, sin TUI.
      // La TUI (terminal del agente) la usan s2 y w6-w10, que son los que programan.
      return Response.json({ ok: false, error: "ruta desconocida en s3" }, { status: 404 });
}

const srv = buildServer();
info(`s3-engram escuchando en http://localhost:${srv.port} · upstream: ${config.engramUrl}`);
