import { isPreflight, preflightResponse, withCORS } from "./cors";
import { info } from "./logger";

async function dispatch(req: Request): Promise<Response> {
  const path = new URL(req.url).pathname;
  if (path === "/health") return Response.json({ ok: true, servicio: "s5-reservado", estado: "up", ram_mb: Math.round(process.memoryUsage().rss / 1024 / 1024) });
  if (path === "/estado") return Response.json({ servicio: "s5-reservado", estado: "reservado", notas: "servicio placeholder para expansion futura" });
  return Response.json({ ok: false, error: "ruta desconocida" }, { status: 404 });
}

function buildServer() {
  return Bun.serve({
    port: Number(Bun.env.PORT ?? 9005),
    async fetch(req) {
      if (isPreflight(req)) return preflightResponse(req.headers.get("origin"));
      const res = await dispatch(req);
      return withCORS(res, req.headers.get("origin"));
    },
  });
}

const srv = buildServer();
info(`s5-reservado escuchando en http://localhost:${srv.port}`);