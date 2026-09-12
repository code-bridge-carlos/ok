# worker-code (C6)

Worker superligero del ecosistema Carlos. Es un agente de implementación mínimo
("inspirado en OpenCode" pero recortado): **solo implementa** las tareas que el
panel C1 envía vía bridge. No planifica (eso es Carlos Code C3), no navega, no
coordina otras PCs.

## Qué hace

1. Se registra como worker en el panel C1 (`POST /api/worker-action`, tipo=registro).
2. Drena la cola por HTTP long-polling (`GET /api/worker-poll`, `<=15s` por el cap
   del proxy de Render).
3. Cuando C1 le envía una subtarea de implementación (`tipo: tarea`, `kind: work`
   con un archivo + prompt), la implementa **él mismo**:
   - Lee el archivo objetivo en `PROJECT_PATH`.
   - Pide al Gateway C2 (DeepSeek) el contenido final concreto del archivo.
   - Lo aplica con una escritura atómica (confinado a `PROJECT_PATH`).
   - Opcionalmente hace `git commit`/`push`.
4. Reporta `estado`, `progress` y `resultado` al panel.

## Herramientas (recortadas a lo mínimo)

- `safePath` — resuelve rutas SOLO dentro de `PROJECT_PATH` (seguridad).
- `read` + `write` — leer/escribir el archivo objetivo (a través del agente).
- `git commit`/`push` — opcional, si `GIT_COMMIT`/`GIT_PUSH` están activos.

NO hay navegador, NO hay red hacia otras PCs, NO hay planificación.

## Variables de entorno

| Variable          | Descripción                                          | Default |
|-------------------|------------------------------------------------------|---------|
| `PANEL_URL`       | Base HTTP del panel C1                               | *(obligatorio)* |
| `WORKER_TOKEN`    | `CONTROL_TOKEN` compartido con el servidor           | *(obligatorio)* |
| `WORKER_ID`       | ID único del worker                                  | hostname |
| `WORKER_NAME`     | Nombre para mostrar en el panel                      | hostname |
| `WORKER_PERF`     | Rendimiento 1-100 (reparto %)                        | 5       |
| `WORKER_WEIGHT`   | Peso 0-100 (reparto %)                               | 10      |
| `PROJECT_PATH`    | Checkout del repo donde implementar                  | *(obligatorio)* |
| `GATEWAY_URL`     | URL del Gateway C2                                   | `https://carlos-gateway.onrender.com` |
| `GIT_REPO`        | Repo local para commit (default = PROJECT_PATH)      | PROJECT_PATH |
| `GIT_COMMIT`      | `true`/`false` commitear tras implementar            | false   |
| `GIT_PUSH`        | `true`/`false` push tras commit                      | false   |
| `POLL_INTERVAL`   | Segundos entre polls (2-15)                          | 10      |

## Build y deploy

```bash
cd control/worker_code
CGO_ENABLED=0 go build -o worker-code-linux .
```

El `Dockerfile` genera el binario y lo corre en `alpine:3.19` con `git`. Se
despliega en el workspace C6.

> Nota de plataforma: en Render el plan free solo admite **web services**, así
> que worker-code corre como web service aunque es un proceso de fondo. Expone
> un `/health` mínimo en `PORT` (Render inyecta 10000) que responde 200 para
> satisfacer el health check, mientras el loop de polling hace el trabajo real
> (llamadas salientes, sin puerto de entrada para clientes).

## Repo del worker

El storage de Render es efímero: al arrancar, `entrypoint.sh` clona
`REPO_URL` (puede llevar credenciales) en `PROJECT_PATH` si está vacío. Los
cambios que worker-code implementa se **committean y pushean** (si
`GIT_COMMIT`/`GIT_PUSH` están activos) para persistir; con auto-deploy `no` en
el servicio el push no dispara un redeploy de sí mismo.
