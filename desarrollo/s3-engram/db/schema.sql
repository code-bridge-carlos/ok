-- Migración idempotente para la tabla engram_observations de S3 (Engram).
-- Conforme a la spec S3 vigente (puntos 9, 22-24): id, título, contenido,
-- tipo, contexto, etiquetas (array), autor, created_at, updated_at, repo_path.
-- Ejecutar contra Supabase: psql "$DATABASE_URL" -f schema.sql

CREATE TABLE IF NOT EXISTS engram_observations (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  title       text NOT NULL,
  content     text NOT NULL,
  type        text NOT NULL DEFAULT 'manual',
  context     text,
  etiquetas   text[] NOT NULL DEFAULT '{}',
  autor       text NOT NULL DEFAULT 's2',
  project     text NOT NULL DEFAULT 'ok',
  scope       text NOT NULL DEFAULT 'project',
  topic_key   text,
  repo_path   text,
  search_tsv  tsvector,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Columnas nuevas para instalaciones que ya tenían la tabla (migración)
ALTER TABLE engram_observations ADD COLUMN IF NOT EXISTS context text;
ALTER TABLE engram_observations ADD COLUMN IF NOT EXISTS etiquetas text[] NOT NULL DEFAULT '{}';
ALTER TABLE engram_observations ADD COLUMN IF NOT EXISTS autor text NOT NULL DEFAULT 's2';
ALTER TABLE engram_observations ADD COLUMN IF NOT EXISTS repo_path text;

-- Índices exigidos por la spec (punto 23): tipo, etiquetas, created_at.
CREATE INDEX IF NOT EXISTS idx_engram_tipo      ON engram_observations (type);
CREATE INDEX IF NOT EXISTS idx_engram_etiquetas ON engram_observations USING GIN (etiquetas);
CREATE INDEX IF NOT EXISTS idx_engram_created   ON engram_observations (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_engram_search    ON engram_observations USING GIN (search_tsv);
CREATE INDEX IF NOT EXISTS idx_engram_project   ON engram_observations (project);
CREATE INDEX IF NOT EXISTS idx_engram_topic     ON engram_observations (topic_key);

-- Triggers: updated_at + búsqueda de texto completo (spec puntos 22-24).
CREATE OR REPLACE FUNCTION update_engram_obs() RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  NEW.search_tsv := to_tsvector('simple',
    NEW.title || ' ' || NEW.content ||
    ' ' || COALESCE(NEW.context, '') ||
    ' ' || COALESCE(array_to_string(NEW.etiquetas, ' '), ''));
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_engram_updated ON engram_observations;
CREATE TRIGGER trg_engram_updated
  BEFORE INSERT OR UPDATE ON engram_observations
  FOR EACH ROW EXECUTE FUNCTION update_engram_obs();