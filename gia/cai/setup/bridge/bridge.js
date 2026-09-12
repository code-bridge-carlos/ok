#!/usr/bin/env node
/**
 * bridge — programa de puente para controlar PCs con OpenCode desde el panel web.
 *
 * Multiplataforma (Windows/Linux). Se usa como un comando en la terminal:
 *
 *   bridge                 -> arranca y se conecta al servidor (como `opencode`)
 *   bridge start           -> igual que arriba
 *   bridge config get      -> muestra la configuracion guardada
 *   bridge config set KEY VALOR
 *                          -> guarda: serverUrl, token, workerId, workerName,
 *                             workerPerf, workerBrand, workerModel,
 *                             autoLaunchOpencode (true|false), opencodeCmd
 *   bridge autostart enable|disable
 *                          -> registra/quita el arranque automatico del SO
 *   bridge status          -> config + estado
 *
 * Al arrancar, si autoLaunchOpencode=true, lanza OpenCode (opencodeCmd) y lo
 * mantiene mientras el bridge vive. Todo se integra en este script: no usa
 * aplicaciones externas para la conectividad.
 */

const WebSocket = require("ws");
const { spawn, execSync, execFileSync } = require("child_process");
const readline = require("readline");
const fs = require("fs");
const os = require("os");
const path = require("path");

const CONFIG_DIR = path.join(os.homedir(), ".bridge");
const CONFIG_FILE = path.join(CONFIG_DIR, "config.json");
const DEFAULTS = {
  serverUrl: "",
  token: "",
  workerId: os.hostname(),
  workerName: os.hostname(),
  workerPerf: 5,
  workerBrand: "",
  workerModel: "",
  autoLaunchOpencode: false,
  opencodeCmd: "opencode serve",
  gitRepo: "",
  gitPush: false,
  engramUrl: "https://engram-mcp-q9ln.onrender.com",
};

// ---------- config ----------
function loadConfig() {
  let cfg = { ...DEFAULTS };
  try {
    const raw = JSON.parse(fs.readFileSync(CONFIG_FILE, "utf8"));
    cfg = { ...cfg, ...raw };
  } catch {}
  // las variables de entorno tienen prioridad (util para pruebas)
  if (process.env.SERVER_URL) cfg.serverUrl = process.env.SERVER_URL;
  if (process.env.WORKER_TOKEN) cfg.token = process.env.WORKER_TOKEN;
  if (process.env.WORKER_ID) cfg.workerId = process.env.WORKER_ID;
  if (process.env.WORKER_NAME) cfg.workerName = process.env.WORKER_NAME;
  if (process.env.WORKER_PERF) cfg.workerPerf = parseInt(process.env.WORKER_PERF, 10);
  if (process.env.WORKER_BRAND) cfg.workerBrand = process.env.WORKER_BRAND;
  if (process.env.WORKER_MODEL) cfg.workerModel = process.env.WORKER_MODEL;
  if (process.env.OPENCODE_CMD) cfg.opencodeCmd = process.env.OPENCODE_CMD;
  if (process.env.AUTO_LAUNCH_OPENCODE) cfg.autoLaunchOpencode = process.env.AUTO_LAUNCH_OPENCODE === "true";
  if (process.env.ENGRAM_URL) cfg.engramUrl = process.env.ENGRAM_URL;
  return cfg;
}

function saveConfig(cfg) {
  fs.mkdirSync(CONFIG_DIR, { recursive: true });
  fs.writeFileSync(CONFIG_FILE, JSON.stringify(cfg, null, 2));
  console.log("[bridge] config guardada en", CONFIG_FILE);
}

// ---------- hardware ----------
function detectHardware(cfg) {
  let brand = cfg.workerBrand;
  let model = cfg.workerModel;
  try {
    if (process.platform === "win32") {
      const out = execSync(
        'powershell -NoProfile -Command "(Get-CimInstance Win32_ComputerSystem | ForEach-Object { ($_.Manufacturer || \'\').Trim() + \'|\' + ($_.Model || \'\').Trim() })"',
        { encoding: "utf8" }
      ).trim();
      const [b, m] = out.split("|").map((s) => s.trim());
      brand = brand || b || "";
      model = model || m || "";
    } else {
      const read = (p) => { try { return fs.readFileSync(p, "utf8").trim(); } catch { return ""; } };
      brand = brand || read("/sys/class/dmi/id/sys_vendor") || read("/sys/class/dmi/id/board_vendor");
      model = model || read("/sys/class/dmi/id/product_name") || read("/sys/class/dmi/id/product_version");
      if (!brand && !model) {
        const h = execSync("hostnamectl 2>/dev/null | grep -iE 'Hardware|Model|Vendor' || true", { encoding: "utf8" }).trim();
        model = model || h.split("\n")[0] || "";
      }
    }
  } catch (e) {
    console.error("[bridge] no se pudo detectar hardware:", e.message);
  }
  return { brand, model, os: `${os.type()} ${os.release()}` };
}

// ---------- auto-arranque del SO (sin apps externas) ----------
// Usa spawnSync con `timeout` para no colgar si no hay sesion/systemd.
function sys(cmd, args) {
  try { return spawnSync("timeout", ["5", cmd, ...args], { stdio: "ignore" }).status; }
  catch { return -1; }
}

function registerAutostart() {
  const script = `"${process.execPath}" "${__filename}" start`;
  try {
    if (process.platform === "win32") {
      spawnSync("schtasks", ["/create", "/tn", "Bridge", "/tr", script, "/sc", "onlogon", "/f"], { stdio: "ignore" });
      console.log("[bridge] autostart registrado en Windows (tarea al iniciar sesion)");
    } else if (process.platform === "linux") {
      // siempre escribimos el unit de systemd --user
      const unitDir = path.join(os.homedir(), ".config", "systemd", "user");
      fs.mkdirSync(unitDir, { recursive: true });
      fs.writeFileSync(path.join(unitDir, "bridge.service"), `[Unit]
Description=Bridge — puente OpenCode
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=${script}
Restart=on-failure

[Install]
WantedBy=default.target
`);
      // intentamos habilitarlo (con timeout por si no hay sesion)
      const r = sys("systemctl", ["--user", "daemon-reload"]);
      const e = sys("systemctl", ["--user", "enable", "bridge.service"]);
      // respaldo para sesiones graficas sin systemd --user funcional
      const ad = path.join(os.homedir(), ".config", "autostart");
      fs.mkdirSync(ad, { recursive: true });
      fs.writeFileSync(path.join(ad, "bridge.desktop"),
        `[Desktop Entry]\nType=Application\nName=Bridge\nExec=${script}\nX-GNOME-Autostart-enabled=true\n`);
      console.log("[bridge] autostart registrado" + (e === 0 ? " (systemd --user)" : " (escritorio ~/.config/autostart)"));
    } else {
      console.log("[bridge] plataforma no soportada para autostart automatico; hazlo manual");
    }
  } catch (err) {
    console.error("[bridge] no se pudo registrar autostart:", err.message);
  }
}

function unregisterAutostart() {
  try {
    if (process.platform === "win32") {
      spawnSync("schtasks", ["/delete", "/tn", "Bridge", "/f"], { stdio: "ignore" });
    } else if (process.platform === "linux") {
      sys("systemctl", ["--user", "disable", "bridge.service"]);
      fs.rmSync(path.join(os.homedir(), ".config", "systemd", "user", "bridge.service"), { force: true });
      fs.rmSync(path.join(os.homedir(), ".config", "autostart", "bridge.desktop"), { force: true });
    }
    console.log("[bridge] autostart quitado");
  } catch (err) {
    console.error("[bridge] no se pudo quitar autostart:", err.message);
  }
}

// ---------- conexion ----------
function startBridge(cfg) {
  if (!cfg.serverUrl) {
    console.error("Falta serverUrl. Usa: bridge config set serverUrl ws://host:8000/ws");
    process.exit(1);
  }
  const hw = detectHardware(cfg);
  // El worker NO usa WebSocket: el proxy de Render corta conexiones persistentes a ~20s
  // y mata la conexion al recibir frames del cliente. Usamos HTTP: el server acola los
  // comandos y el worker los drena por long-polling; el worker sube acciones por POST.
  const httpBase = cfg.serverUrl.replace(/^wss:/, "https:").replace(/^ws:/, "http:").replace(/\/ws.*$/, "");

  let opencodeChild = null;
  if (cfg.autoLaunchOpencode) {
    const [bin, ...args] = cfg.opencodeCmd.split(/\s+/);
    console.log("[bridge] auto-lanzando OpenCode:", cfg.opencodeCmd);
    opencodeChild = spawn(bin, args, { stdio: "ignore", detached: false, env: process.env });
    opencodeChild.on("exit", (code) => console.log("[bridge] OpenCode termino (exit " + code + ")"));
  }

  let current = null;  // tarea en curso (para abortar por return_progress/cancel)
  let chain = Promise.resolve();  // serializa tareas (una a la vez)
  let backoff = 3000;
  const MAX_BACKOFF = 30000;
  const TIMEOUT_MS = parseInt(process.env.TASK_TIMEOUT_MS || "120000", 10);
  let running = true;
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  // sube una accion del worker por HTTP POST
  async function postAction(body) {
    try {
      const u = httpBase + "/api/worker-action?token=" + encodeURIComponent(cfg.token || "") +
        "&id=" + encodeURIComponent(cfg.workerId);
      const r = await fetch(u, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      if (r.ok) backoff = 3000;
      return r.ok;
    } catch (e) {
      return false;
    }
  }

  function handleMessage(msg) {
    if (msg.tipo === "tarea") {
      chain = chain.then(() => runTask(msg)).catch((e) => console.error("[bridge] error en tarea:", e));
    } else if (msg.tipo === "return_progress") {
      if (current && current.task_id === msg.task_id) {
        current.aborted = true;
        try { current.child.kill("SIGTERM"); } catch {}
        console.log(`[bridge] devolviendo progreso de ${msg.file} (orden del servidor)`);
      }
    } else if (msg.tipo === "cancel") {
      if (current && current.task_id === msg.task_id) {
        current.aborted = true;
        try { current.child.kill("SIGTERM"); } catch {}
        console.log(`[bridge] cancelada ${msg.task_id} (preempcion)`);
      }
    } else if (msg.tipo === "recall") {
      console.log(`[bridge] aviso del servidor: ${msg.note || ("se te quita " + msg.file)}`);
    }
  }

  async function pollLoop() {
    console.log("[bridge] registrando worker en", httpBase, "como", cfg.workerId);
    const reg = {
      tipo: "registro", id: cfg.workerId, name: cfg.workerName, perf: cfg.workerPerf,
      brand: hw.brand, model: hw.model, os: hw.os,
    };
    await postAction(reg);
    console.log(`[bridge] registrado (${hw.brand} ${hw.model} / ${hw.os})`);
    while (running) {
      try {
        const u = httpBase + "/api/worker-poll?token=" + encodeURIComponent(cfg.token || "") +
          "&id=" + encodeURIComponent(cfg.workerId) + "&timeout=15";
        const r = await fetch(u, { method: "GET" });
        if (!r.ok) throw new Error("poll status " + r.status);
        const data = await r.json();
        backoff = 3000;
        for (const m of (data.messages || [])) handleMessage(m);
      } catch (e) {
        console.error("[bridge] poll error:", e.message || e, "Reintento en " + (backoff / 1000) + "s");
        await sleep(backoff);
        backoff = Math.min(backoff * 2, MAX_BACKOFF);
        // re-registrarse por si el server reinicio y perdio este worker
        await postAction(reg);
      }
    }
  }

  // Fire-and-forget: log task completion to Engram (spec:58-72).
  // Failures are logged only — never break task flow.
  function logTaskToEngram(task_id, prompt, result) {
    if (!cfg.engramUrl) return;
    const body = {
      title: "Task " + task_id + " completed",
      type: "task",
      topic: "tasks/" + task_id,
      content: JSON.stringify({ prompt, result }),
      tags: ["worker"],
    };
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 5000);
    fetch(cfg.engramUrl + "/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: controller.signal,
    }).catch((e) => {
      console.error("[bridge] engram save failed:", e.message || e);
    }).finally(() => clearTimeout(timer));
  }

  function gitCommit(task_id) {
    if (!cfg.gitRepo) return "";
    try {
      const repo = cfg.gitRepo;
      const msg = `bridge: subtarea ${task_id}`;
      execSync(`git -C "${repo}" add -A`, { stdio: "ignore" });
      execSync(`git -C "${repo}" commit -m "${msg}"`, { stdio: "ignore" });
      if (cfg.gitPush) execSync(`git -C "${repo}" push`, { stdio: "ignore" });
      return `\n[git] commit${cfg.gitPush ? " + push" : ""} en ${repo}`;
    } catch (e) {
      return `\n[git] sin cambios o error: ${e.message}`;
    }
  }

  function runTask(msg) {
    return new Promise((resolve) => {
      const { task_id, subtask_id, kind, prompt, files, plan_summary, workers: wsummary, mode } = msg;
      const bin = cfg.opencodeCmd.split(/\s+/)[0];
      // cada tarea trabaja en su propia carpeta para que los archivos no se pisen
      const workDir = path.join(cfg.gitRepo || path.join(os.homedir(), "cai-workspace"), ".bridge_tasks", task_id);
      fs.mkdirSync(workDir, { recursive: true });

      let fullPrompt;
      if (kind === "plan") {
        const lista = (wsummary || []).map((w) => `- ${w.id} (peso ${w.weight})`).join("\n");
        fullPrompt =
          `Eres la PC principal. El usuario pide: "${prompt}".\n` +
          `Modo de reparto: ${mode}. PCs disponibles y sus pesos (%):\n${lista}\n` +
          `Define las unidades de trabajo como ARCHIVOS a modificar y divide segun los pesos.\n` +
          `Responde SOLO con un bloque JSON:\n` +
          `  modo "percent": {"mode":"percent","note":"...","assignments":{"<idPC>":["archivo1","archivo2"]}}\n` +
          `  modo "dynamic": {"mode":"dynamic","note":"...","files":["archivo1","archivo2"]}\n` +
          `Cada archivo debe asignarse a UNA sola PC. No repitas archivos.`;
        console.log(`[bridge] PLAN ${task_id} (principal)`);
      } else {
        const filePrompt = msg.file_prompt || "";
        fullPrompt =
          `Tarea global: ${prompt}\n` +
          (plan_summary ? `Plan: ${plan_summary}\n` : "") +
          `Archivo objetivo (uno por PC): ${(files || []).join(", ")}\n` +
          (filePrompt ? `Instrucciones especificas para este archivo:\n${filePrompt}\n` : "") +
          `Modifica SOLO este archivo (no te saltes):\n` +
          (files || []).map((f) => `- ${f}`).join("\n") +
          `\nImplementa los cambios necesarios.`;
        console.log(`[bridge] WORK ${task_id}/${subtask_id}: ${files ? files.join(",") : prompt.slice(0, 40)}`);
      }

      postAction({ tipo: "estado", estado: "ocupado" });
      current = { task_id, subtask_id, kind: kind || "work", file: (files || [])[0], aborted: false, child: null };
      const child = spawn(bin, ["run", fullPrompt], { env: process.env, cwd: workDir });
      current.child = child;
      let out = ""; let killed = false;
      const timer = setTimeout(() => {
        killed = true; child.kill("SIGKILL");
        out += `\n[timeout: limite ${TIMEOUT_MS / 1000}s]`;
      }, TIMEOUT_MS);
      // SOLO acumular output (sin mostrar en terminal local)
      child.stdout.on("data", (d) => (out += d.toString()));
      child.stderr.on("data", (d) => (out += d.toString()));
      child.on("close", (code) => {
        clearTimeout(timer);
        const wasAborted = current && current.aborted;
        if (!killed && !wasAborted) out += `\n[exit code: ${code}]`;
        if (kind !== "plan" && files) out += gitCommit(task_id);
        const status = wasAborted ? "returned" : "done";
        postAction({
          tipo: "resultado", task_id, subtask_id,
          kind: kind || "work", output: out, status,
        });
        postAction({ tipo: "estado", estado: "disponible" });
        current = null;
        console.log(`[bridge] ${kind || "work"} ${task_id} terminada (${status}, exit ${code})`);
        logTaskToEngram(task_id, prompt, out.slice(0, 2000));
        resolve();
      });
    });
  }

  pollLoop();

  const shutdown = () => {
    running = false;
    try { if (opencodeChild) opencodeChild.kill(); } catch {}
    try { if (current && current.child) current.child.kill("SIGTERM"); } catch {}
    process.exit(0);
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

// ---------- init interactivo ----------
function ask(rl, q, def) {
  return new Promise((res) => {
    rl.question(def ? `${q} [${def}]: ` : `${q}: `, (a) => res(a.trim() || def || ""));
  });
}

async function initBridge() {
  // modo no interactivo si vienen flags
  const flags = {};
  for (let i = 0; i < process.argv.length; i++) {
    const a = process.argv[i];
    if (a === "--host") flags.host = process.argv[++i];
    else if (a === "--port") flags.port = process.argv[++i];
    else if (a === "--token") flags.token = process.argv[++i];
    else if (a === "--id") flags.id = process.argv[++i];
    else if (a === "--perf") flags.perf = process.argv[++i];
  }
  const interactive = !flags.host;
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  try {
    const host = interactive ? await ask(rl, "Host o IP del servidor (ej. 192.168.1.10 o mipanel.duckdns.org)") : flags.host;
    const port = interactive ? await ask(rl, "Puerto", "8000") : (flags.port || "8000");
    const token = interactive ? await ask(rl, "Token compartido (el CONTROL_TOKEN del servidor)") : flags.token;
    const wid = interactive ? await ask(rl, "ID de esta PC", os.hostname()) : (flags.id || os.hostname());
    const perf = interactive ? await ask(rl, "Rendimiento 1-10", "5") : (flags.perf || "5");
    if (!host || !token) {
      console.error("[bridge] host y token son obligatorios");
      return;
    }
    const cfg = loadConfig();
    cfg.serverUrl = `ws://${host}:${port}/ws`;
    cfg.token = token;
    cfg.workerId = wid;
    cfg.workerPerf = parseInt(perf, 10) || 5;
    saveConfig(cfg);
    console.log("[bridge] configurado. Ahora ejecuta: bridge");
  } catch (e) {
    console.error("[bridge] error en init:", e.message);
  } finally {
    rl.close();
  }
}

// ---------- CLI ----------
function cmd_help() {
  const lines = [
    "bridge - puente entre tu PC y el panel de control web",
    "",
    "Uso:  bridge <comando> [opciones]",
    "Sin comando, arranca el puente (equivalente a 'bridge start').",
    "",
    "Comandos:",
    "  start                Conecta el puente al panel web y ejecuta las tareas",
    "                      de OpenCode que el panel le asigne. Es el modo normal.",
    "  init                 Configura el puente paso a paso (host del servidor,",
    "                      token, id y rendimiento de la PC). Guarda en",
    "                      ~/.bridge/config.json. Acepta flags no interactivos:",
    "                        --host <ip|dominio>  --port <puerto>",
    "                        --token <token>  --id <workerId>  --perf <1-100>",
    "  config get           Muestra toda la configuracion actual.",
    "  config set <k> <v>   Guarda una opcion. Claves:",
    "                        serverUrl   URL del panel (ws://host:puerto/ws)",
    "                        token       CONTROL_TOKEN compartido con el servidor",
    "                        workerId    identificador unico de esta PC",
    "                        workerName  nombre para mostrar en el panel",
    "                        perf        rendimiento 1-100 (para reparto %)",
    "                        weight      peso 0-100 para reparto por %",
    "                        autoLaunchOpencode  true/false (lanzar OpenCode al iniciar)",
    "                        opencodeCmd comando de OpenCode (def. 'opencode serve')",
    "                        gitRepo     ruta del repo donde trabajar",
    "                        gitPush     true/false (hacer push tras commit)",
    "  autostart enable     Enciende el puente solo al prender la PC (systemd/",
    "                      autostart de escritorio en Linux, schtasks en Windows).",
    "  autostart disable    Quita el autoinicio.",
    "  status               Muestra la configuracion y el estado de conexion.",
    "  help                 Muestra esta ayuda.",
    "",
    "Variables de entorno (override de la config):",
    "  SERVER_URL, WORKER_TOKEN, WORKER_ID, WORKER_NAME, WORKER_PERF, WORKER_WEIGHT,",
    "  OPENCODE_CMD, CONTROL_TOKEN (solo servidor)",
  ];
  console.log(lines.join("\n"));
}

function main() {
  const [,, cmd, sub, ...rest] = process.argv;
  const cfg = loadConfig();

  if (cmd === "config") {
    if (sub === "set") {
      const key = rest[0]; const val = rest.slice(1).join(" ");
      if (!(key in DEFAULTS)) { console.error("Clave desconocida:", key, "(usa:", Object.keys(DEFAULTS).join(", "), ")"); process.exit(1); }
      if (key === "workerPerf") cfg.workerPerf = parseInt(val, 10);
      else if (key === "autoLaunchOpencode") cfg.autoLaunchOpencode = val === "true" || val === "1";
      else cfg[key] = val;
      saveConfig(cfg);
    } else {
      console.log(JSON.stringify(cfg, null, 2));
    }
    return;
  }

  if (cmd === "autostart") {
    if (sub === "enable") registerAutostart();
    else if (sub === "disable") unregisterAutostart();
    else console.log("Uso: bridge autostart enable|disable");
    return;
  }

  if (cmd === "init") {
    initBridge();
    return;
  }

  if (cmd === "status") {
    console.log(JSON.stringify(cfg, null, 2));
    return;
  }

  if (!cmd || cmd === "start" || cmd === "help" || cmd === "-h" || cmd === "--help") {
    if (cmd === "help" || cmd === "-h" || cmd === "--help") {
      cmd_help();
      return;
    }
    startBridge(cfg);
    return;
  }

  console.error("Comando desconocido:", cmd, "(usa bridge help)");
  process.exit(1);
}

main();
