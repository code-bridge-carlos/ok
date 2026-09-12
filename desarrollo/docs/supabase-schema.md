# Schema Supabase — Sistema Multiagente

> Fecha: 2026-09-10
> Script: `s1-panel/db/schema.sql`
> Motor: PostgreSQL (Supabase, PG15+)

---

## Tablas (8)

| # | Tabla | Propósito | Escritor principal | Lector principal |
|---|-------|-----------|--------------------|------------------|
| 1 | `tasks` | Tarea global (plan → ejecución → fin) | s2 | s1, s2 |
| 2 | `subtask_queue` | Cola de subtareas (nunca en RAM) | s1, workers | s1, s2, workers |
| 3 | `services_status` | Estado de los 10 servicios (keep-alive) | s1 | s1 (dashboard) |
| 4 | `tareas_panel` | Tareas del botón "Tarea" de s1 | s1 | s1 |
| 5 | `engram_observations` | Memoria persistente de s3 (Engram) | s3 | s3, s2 |
| 6 | `proxy_pool` | Pool de proxies (proxmint/free-proxy-list) | s1 | s1 |
| 7 | `proxy_assignments` | Asignación proxy → servicio | s1 | s1, servicios |
| 8 | `tui_access_tokens` | Tokens temporales para TUIs | s1 | s1, servicios |

---

## 1. `tasks`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| prompt | text NOT NULL | Tarea original del usuario |
| estado | text | `planificando` \| `ejecutando` \| `completada` \| `fallida` |
| plan_json | jsonb | Plan generado por s2 (para revisión en TUI) |
| created_at | timestamptz | Default now() |
| finished_at | timestamptz | Null hasta completar |

**Índices:** `idx_tasks_estado`, `idx_tasks_created_at (desc)`

**Flujo:** s1 crea la task en este momento del envío → s2 la lee, genera plan (escribe `plan_json`), divide en subtareas (inserta en `subtask_queue`) → al terminar todas, s2 marca `completada` + `finished_at`.

---

## 2. `subtask_queue`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| task_id | uuid FK → tasks.id | Cascade delete |
| worker_id | text | `w6`..`w10`; null = sin asignar |
| file_path | text | Archivo(s) objetivo de la subtarea |
| prompt | text NOT NULL | Prompt específico (contexto completo preparado por s2) |
| contexto | text | Contexto adicional |
| estado | text | `pendiente` \| `asignada` \| `en_progreso` \| `completada` \| `fallida` \| `reasignada` |
| resultado | jsonb | Salida: `{exit, files, summary, output}` |
| created_at | timestamptz | Default now() |
| updated_at | timestamptz | Trigger `set_updated_at()` |

**Índices:** `idx_subtask_estado`, `idx_subtask_worker` (parcial, worker not null), `idx_subtask_task`, `idx_subtask_created`

**Flujo:** s2 inserta subtareas (pendiente) → s1 asigna a worker libre (`asignada` + worker_id) → worker marca `en_progreso` → termina: `completada`/`fallida` + `resultado` → s2 evalúa → si falla, s1 puede reasignar (`reasignada` → `pendiente`).

---

## 3. `services_status`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id_servicio | text PK | `s1`..`s5`, `w6`..`w10` (check) |
| ram_mb | integer | Consumo de RAM reportado |
| estado | text | `up` \| `down` \| `starting` |
| url_tui | text | URL de la TUI del servicio |
| latencia_ms | integer | Latencia del último ping keep-alive |
| updated_at | timestamptz | Default now() |

**Seed:** 10 filas insertadas con `estado = 'down'` al crear la tabla.

**Flujo:** s1 hace GET `/health` a cada servicio cada 10 min (si keep-alive ON) y actualiza fila. Dashboard de s1 lee esta tabla (nunca RAM).

---

## 4. `tareas_panel`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| prompt | text NOT NULL | Tarea escrita en el botón "Tarea" |
| creada_at | timestamptz | Default now() |
| enviada_a_s2 | boolean | False hasta pulsar "Enviar tarea" |
| subtask_count | integer | Nº de subtareas generadas por s2 |

**Índice:** `idx_tareas_panel_creada (desc)`

**Regla:** una vez `enviada_a_s2 = true`, s1 NO permite cancelar/corregir (la UI lo bloquea; control posterior solo en TUI de s2).

---

## 5. `engram_observations`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| title | text NOT NULL | Título corto y buscable |
| content | text NOT NULL | Formato What/Why/Where/Learned |
| type | text | `decision` \| `bugfix` \| `discovery` \| `pattern` \| `config` \| `manual`... |
| project | text | Proyecto al que pertenece |
| scope | text | `project` \| `personal` |
| topic_key | text | Clave estable para upserts de temas evolutivos |
| search_tsv | tsvector GENERATED | FTS: `to_tsvector('simple', title || ' ' || content)` |
| created_at / updated_at | timestamptz | updated_at con trigger |

**Índices:** `idx_engram_project`, `idx_engram_topic`, `idx_engram_created`, GIN `idx_engram_search`

**Uso:** s3 (Engram) escribe aquí; s2 consulta su memoria real. Búsqueda con `search_tsv @@ plainto_tsquery('simple', $1)`.

---

## 6. `proxy_pool`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| proxy_url | text UNIQUE | Ej: `http://1.2.3.4:8080` |
| status | text | `libre` \| `asignado` \| `fallido` |
| last_checked | timestamptz | Última validación |
| source | text | `proxmint/free-proxy-list` |

**Índice:** `idx_proxy_pool_status`

**Flujo:** "Refrescar"/cron horario → s1 limpia y reinserta 100 proxies → marca `libre`. Al asignar a un servicio, ese proxy pasa a `asignado`.

---

## 7. `proxy_assignments`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| servicio | text FK → services_status.id_servicio | Cascade delete |
| proxy_url | text NOT NULL | Proxy asignado |
| asignado_at | timestamptz | Default now() |
| origen | text | `auto` (cron horario) \| `manual` (botón ⋮) |
| activo | boolean | False al reasignar/reemplazar |

**Índices:** `idx_proxy_assign_servicio` (parcial, activo), `idx_proxy_assign_url`

**Regla:** solo una asignación activa por servicio (índice parcial + lógica de s1: al asignar, marcar anteriores `activo=false`). Los 90 proxies no asignados quedan "libres" para uso manual.

---

## 8. `tui_access_tokens`

| Campo | Tipo | Descripción |
|-------|------|-------------|
| id | uuid PK | Autogenerado |
| token_hash | text UNIQUE | SHA-256 del token (NUNCA token en crudo) |
| servicio | text FK → services_status.id_servicio | Cascade delete |
| usuario | text | `admin` |
| expira_at | timestamptz NOT NULL | Corta duración (ej: 60s) |
| usado | boolean | Un solo uso |
| created_at | timestamptz | Default now() |

**Índices:** `idx_tui_tokens_expira`, `idx_tui_tokens_usado` (parcial, not usado)

**Regla de seguridad:** s1 emite token → guarda SOLO hash → redirige a `https://tui-servicio/?token=XXX`. El servicio destino valida: hash(token) existe, no expirado, no usado → marca `usado=true` → permite acceso. Acceso directo sin token: denegado.

---

## Funciones y Triggers

```sql
create or replace function public.set_updated_at()
returns trigger language plpgsql ...
-- Se dispara en: subtask_queue.updated_at, engram_observations.updated_at
```

---

## Consideraciones de despliegue en Supabase

1. Ejecutar `schema.sql` en SQL Editor (rol owner) — crea tablas, índices, triggers, seed.
2. **Keys:** servicios backend usan **service_role key** (configurada al final, Fase 14). Nunca exponer en frontend.
3. **RLS:** por defecto OFF (acceso service_role). Si se activa, políticas SOLO para service_role.
4. **Escrituras masivas:** NO loguear en tiempo real (los workers solo actualizan su subtarea + estado; el `resultado` no debe contener logs streaming).
5. **Suscripción Realtime** (opcional, Fase 12): habilitar solo en `subtask_queue` y `tasks` si se usa Realtime en vez de long-polling.
6. **Retención:** `tui_access_tokens` consumidos pueden purgarse (cron de s1 o job de Supabase).

---

## Verificación

Queries de prueba (insert/select/update/delete sobre cada tabla) ejecutadas contra Postgres local vía Docker durante la fase; replicar en Supabase al configurar las cuentas (Fase 14).