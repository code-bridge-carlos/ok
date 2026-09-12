-- Esquema opcional de s4 (MCPs bajo demanda). La tabla mcp_logs guarda el
-- registro de ejecuciones de herramientas MCP (spec S4, punto 19: id, mcp,
-- herramienta, parametros, resultado, estado, created_at). Se aplica solo si
-- DATABASE_URL está disponible; el servicio funciona igual sin ella.

CREATE TABLE IF NOT EXISTS mcp_logs (
    id          bigserial PRIMARY KEY,
    mcp         text NOT NULL,
    herramienta text NOT NULL,
    parametros  jsonb,
    resultado   jsonb,
    estado      text NOT NULL,           -- 'ok' | 'error'
    error       text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mcp_logs_mcp_created
    ON mcp_logs (mcp, created_at DESC);