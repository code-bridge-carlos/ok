# Sistema de Control Web — Gentle AI / OpenCode distribuido

Controla varias PCs (cada una con OpenCode) desde un panel web. En el panel ves
marca/modelo/SO de cada equipo y eliges cuál es la **principal**.

## Topología
- **PC principal (donde corre el panel)**: `control/server/` (FastAPI + WebSocket + web).
  Lo hospedas donde sea alcanzable por las otras PCs (tu PC con el puerto abierto,
  o un VPS). No usa aplicaciones externas: el script maneja la conexión.
- **PCs a controlar**: `control/worker/` — el programa **`bridge`** (Node, multiplataforma
  Windows/Linux). Se escribe en la terminal como `opencode` y se enciende.

Las PCs controladas se conectan *hacia* el servidor (salida). El servidor no abre
puertos en ellas.

## Puestos en marcha

### 1) Servidor (PC principal / VPS)
```bash
cd control/server
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env   # fija CONTROL_TOKEN y PORT
python3 server.py
```
Panel en http://localhost:8000 (o la IP/puerto público que expongas).

### 2) En cada PC a controlar: instalar y configurar `bridge`
```bash
cd control/worker
npm install
npm link            # deja el comando `bridge` en el PATH (como opencode)
bridge config set serverUrl ws://<ip-o-host-del-servidor>:8000/ws
bridge config set token <mismo CONTROL_TOKEN>
bridge config set workerId $(hostname)   # opcional
bridge config set workerPerf 7           # 1-10, opcional
bridge                                 # arranca y se conecta
```
Al arrancar, `bridge` detecta sola la marca/modelo/SO y envía la señal de "activa".

### 3) Auto-arranque y OpenCode
```bash
bridge config set autoLaunchOpencode true   # al encender el bridge, tambien lanza OpenCode
bridge config set opencodeCmd "opencode serve"  # comando (defecto)
bridge autostart enable    # registra el arranque del SO (systemd --user / .desktop en Linux,
                           #   schtasks en Windows). Sin apps externas.
bridge autostart disable   # quitar
bridge help              # lista todos los comandos de bridge y para que sirven
```
Cada vez que el `bridge` se enciende (incluido el arranque del sistema), si
`autoLaunchOpencode=true` también ejecuta OpenCode hasta que el bridge se apague o
se desactive la opción. En las PCs que quieras en ese estado, dejas la opción activa.

## Protocolo (JSON sobre WebSocket)
Worker -> Servidor: `registro{id,name,perf,brand,model,os}`, `pong`, `estado`, `resultado`.
Servidor -> Worker: `registro_confirmado`, `ping`, `tarea`.
Servidor -> Panel (`/ws/panel`): `workers`, `tasks`, `task_update`, `principal_update`, `event`.

## REST
- `GET  /api/workers`  — lista workers + principal
- `GET  /api/tasks`    — lista de tareas
- `POST /api/task`     — `{target:"auto"|worker_id, prompt}` -> despacha a la principal si está libre
- `GET/POST /api/principal` — consulta / elige la PC principal `{id}`

## Control desde fuera de casa
Punta `serverUrl` de cada `bridge` a la **dirección pública** de tu servidor (VPS o tu
PC con el puerto reenviado). No necesitas Tailscale/Cloudflared ni apps externas: el
script se conecta directo por WebSocket. Para tráfico cifrado en ruta pública, pon el
servidor detrás de un proxy normal (nginx/Caddy) y usa `wss://...` (opcional).

## Reparto y preempcion
- Cada archivo del plan es un documento real (.py/.html/.css...); la principal analiza y
  devuelve un prompt especifico por archivo, y cada PC lo ejecuta de a uno (un archivo =
  una sola PC, sin conflictos).
- Modo porcentaje: **work-stealing** — las PCs libres se llevan el ULTIMO archivo de la
  cola de la PC mas cargada (la afectada recibe el aviso `recall`: "se te quita el ultimo
  archivo de tu cola"). Si una PC lleva >5 min con un archivo y hay otra libre, el servidor
  ordena `return_progress` (devolver el progreso parcial, hace commit si hay gitRepo) y
  reasigna el archivo a la PC libre. Umbral configurable con `STUCK_SECONDS` (def. 300).
- **Preempcion**: si envias una tarea nueva mientras otra corre, la vieja se PAUSA (se
  guardan los archivos que faltaban) y la nueva precede. Al terminar la nueva, la vieja se
  REANUDA ejecutando lo que faltaba. La principal, si estaba planeando, recibe `cancel` y
  al reanudar se le pide el plan de nuevo.

## Seguridad
- Todo requiere `CONTROL_TOKEN` (worker por `?token=`, panel por `?token=` en WS y
  `Authorization: Bearer` en REST).
- No expongas el puerto 8000 a internet sin token ni sin TLS.
