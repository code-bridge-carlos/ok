package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// --- Request/Response types ---

type DeepSeekRequest struct {
	Prompt       string            `json:"prompt"`
	BearerToken  string            `json:"bearer_token"`
	Cookies      string            `json:"cookies"`
	Model        string            `json:"model"`
	Stream       bool              `json:"stream"`
	CustomHeaders map[string]string `json:"custom_headers,omitempty"`
}

type DeepSeekResponse struct {
	Content      string `json:"content"`
	Model        string `json:"model,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	Error        string `json:"error,omitempty"`
	StatusCode   int    `json:"status_code,omitempty"`
	RawResponse  string `json:"raw_response,omitempty"`
}

type BrowseRequest struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	WaitMS  int               `json:"wait_ms,omitempty"`
}

type BrowseResponse struct {
	HTML  string `json:"html,omitempty"`
	Error string `json:"error,omitempty"`
}

// --- Browser pool for concurrency control ---

type BrowserPool struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	ready  bool
}

var pool *BrowserPool

func initBrowser() {
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		chromePath = "/usr/bin/chromium"
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("single-process", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-translate", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("window-size", "1280,720"),
		chromedp.Flag("headless", true),
		chromedp.ExecPath(chromePath),
	)

	allocCtx, _ := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	pool = &BrowserPool{
		ctx:    browserCtx,
		cancel: browserCancel,
		ready:  true,
	}

	log.Println("Browser pool initialized")
}

func main() {
	log.Println("Starting browser service...")
	initBrowser()

	http.HandleFunc("/browse", browseHandler)
	http.HandleFunc("/api/deepseek", deepseekHandler)
	http.HandleFunc("/api/extract-cookies", extractCookiesHandler)
	http.HandleFunc("/screen", screenHandler)
	http.HandleFunc("/screen-info", screenInfoHandler)
	http.HandleFunc("/action", actionHandler)
	http.HandleFunc("/viewer", viewerHandler)
	http.HandleFunc("/", viewerHandler)
	http.HandleFunc("/health", healthHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Browser service on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// --- Handlers ---

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "browser-service",
		"browser": "chromium",
	})
}

// screenHandler captures a screenshot of the current browser page
func screenHandler(w http.ResponseWriter, r *http.Request) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	var buf []byte
	err := chromedp.Run(pool.ctx,
		chromedp.CaptureScreenshot(&buf),
	)

	w.Header().Set("Content-Type", "image/png")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("screenshot failed: " + err.Error()))
		return
	}
	w.Write(buf)
}

// screenInfoHandler returns viewport dimensions and current URL for the viewer
func screenInfoHandler(w http.ResponseWriter, r *http.Request) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	var width, height int
	var url string
	err := chromedp.Run(pool.ctx,
		chromedp.Evaluate(`window.innerWidth`, &width),
		chromedp.Evaluate(`window.innerHeight`, &height),
		chromedp.Evaluate(`location.href`, &url),
	)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"width":  width,
		"height": height,
		"url":    url,
	})
}

// viewerHandler serves the interactive browser viewer web UI
func viewerHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/viewer" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Visor de Navegador - C6</title>
<style>
  body { font-family: Arial, sans-serif; margin: 0; padding: 0; background: #1e1e2e; color: #cdd6f4; }
  .toolbar { background: #313244; padding: 10px 14px; display: flex; gap: 8px; align-items: center; flex-wrap: wrap; position: sticky; top: 0; z-index: 10; }
  .toolbar input[type=text] { flex: 1; min-width: 180px; padding: 7px 10px; border-radius: 6px; border: 1px solid #45475a; background: #1e1e2e; color: #cdd6f4; }
  .toolbar button { padding: 7px 12px; border-radius: 6px; border: none; background: #89b4fa; color: #11111b; cursor: pointer; font-weight: 600; }
  .toolbar button:hover { background: #74c7ec; }
  .toolbar .url { font-family: monospace; font-size: 12px; color: #a6adc8; }
  .screen-wrap { padding: 12px; text-align: center; }
  #screen { max-width: 100%; border: 2px solid #45475a; border-radius: 8px; cursor: crosshair; background: #000; }
  .status { padding: 6px 14px; font-size: 13px; color: #a6adc8; }
  .hint { font-size: 12px; color: #6c7086; padding: 0 14px 10px; }
  .coords { font-family: monospace; font-size: 12px; color: #f9e2af; }
  .kbd-wrap { padding: 10px 12px 16px; background: #262638; border-top: 1px solid #313244; position: sticky; bottom: 0; z-index: 9; }
  .kbd-row { display: flex; justify-content: center; gap: 5px; margin-bottom: 6px; }
  .kbd-key { width: 44px; height: 40px; border: 1px solid #45475a; border-radius: 6px; background: #313244; color: #cdd6f4; font-size: 14px; font-weight: 600; cursor: pointer; box-shadow: 0 2px 0 #181825; user-select: none; }
  .kbd-key:hover { background: #45475a; }
  .kbd-key.pressed { background: #89b4fa; color: #11111b; }
  .kbd-special { background: #45475a; }
  .kbd-shift.active { background: #f9e2af; color: #11111b; }
  .w66 { width: 66px; }
  .w88 { width: 88px; }
  .kbd-space { flex: 1; max-width: 560px; min-width: 240px; height: 40px; }
</style>
</head>
<body>
<div class="toolbar">
  <input type="text" id="navigateInput" placeholder="Ingresá URL y Enter" />
  <button onclick="navigate()">Ir</button>
  <button onclick="doAction('wait')">Esperar</button>
  <button onclick="scrollPage(-400)">Scroll ↑</button>
  <button onclick="scrollPage(400)">Scroll ↓</button>
  <button onclick="refresh()">Refrescar</button>
  <span class="url" id="curUrl"></span>
</div>
<div class="toolbar" style="background:#262638">
  <input type="text" id="typeInput" placeholder="Texto a escribir (correo, contraseña...)" style="flex:2" />
  <button onclick="typeText()">Escribir</button>
  <button onclick="pressKey('Enter')">Enter</button>
  <button onclick="pressKey('Tab')">Tab</button>
  <span class="hint">Primero hacé clic sobre el campo en la pantalla, después escribí acá.</span>
</div>
<div class="status">
  <span id="status">Cargando...</span>
  <span class="coords" id="coords"></span>
</div>
<div class="hint">Hacé <b>clic</b> sobre la pantalla para hacer clic en esa posición. Mové el mouse para ver coordenadas. El CAPTCHA se completa haciendo clic en la casilla y esperando.</div>
<div class="screen-wrap">
  <img id="screen" alt="Browser screen" />
</div>
<div class="kbd-wrap">
  <div class="kbd-row">
    <button class="kbd-key" data-base="1" data-shift="!">1</button>
    <button class="kbd-key" data-base="2" data-shift="@">2</button>
    <button class="kbd-key" data-base="3" data-shift="#">3</button>
    <button class="kbd-key" data-base="4" data-shift="$">4</button>
    <button class="kbd-key" data-base="5" data-shift="%">5</button>
    <button class="kbd-key" data-base="6" data-shift="^">6</button>
    <button class="kbd-key" data-base="7" data-shift="&amp;">7</button>
    <button class="kbd-key" data-base="8" data-shift="*">8</button>
    <button class="kbd-key" data-base="9" data-shift="(">9</button>
    <button class="kbd-key" data-base="0" data-shift=")">0</button>
    <button class="kbd-key" data-base="-" data-shift="_">-</button>
    <button class="kbd-key" data-base="=" data-shift="+">=</button>
    <button class="kbd-key kbd-special w88" data-special="Backspace">Backspace</button>
  </div>
  <div class="kbd-row">
    <button class="kbd-key kbd-special w66" data-special="Tab">Tab</button>
    <button class="kbd-key" data-base="q" data-shift="Q">q</button>
    <button class="kbd-key" data-base="w" data-shift="W">w</button>
    <button class="kbd-key" data-base="e" data-shift="E">e</button>
    <button class="kbd-key" data-base="r" data-shift="R">r</button>
    <button class="kbd-key" data-base="t" data-shift="T">t</button>
    <button class="kbd-key" data-base="y" data-shift="Y">y</button>
    <button class="kbd-key" data-base="u" data-shift="U">u</button>
    <button class="kbd-key" data-base="i" data-shift="I">i</button>
    <button class="kbd-key" data-base="o" data-shift="O">o</button>
    <button class="kbd-key" data-base="p" data-shift="P">p</button>
    <button class="kbd-key" data-base="[" data-shift="{">[</button>
    <button class="kbd-key" data-base="]" data-shift="}">]</button>
    <button class="kbd-key" data-base="\\" data-shift="|">\</button>
  </div>
  <div class="kbd-row">
    <button class="kbd-key kbd-special w88" data-special="Shift">Shift</button>
    <button class="kbd-key" data-base="a" data-shift="A">a</button>
    <button class="kbd-key" data-base="s" data-shift="S">s</button>
    <button class="kbd-key" data-base="d" data-shift="D">d</button>
    <button class="kbd-key" data-base="f" data-shift="F">f</button>
    <button class="kbd-key" data-base="g" data-shift="G">g</button>
    <button class="kbd-key" data-base="h" data-shift="H">h</button>
    <button class="kbd-key" data-base="j" data-shift="J">j</button>
    <button class="kbd-key" data-base="k" data-shift="K">k</button>
    <button class="kbd-key" data-base="l" data-shift="L">l</button>
    <button class="kbd-key" data-base=";" data-shift=":">;</button>
    <button class="kbd-key" data-base="'" data-shift="&quot;">'</button>
    <button class="kbd-key kbd-special w88" data-special="Enter">Enter</button>
  </div>
  <div class="kbd-row">
    <button class="kbd-key kbd-special w88 kbd-shift" data-special="Shift">Shift</button>
    <button class="kbd-key" data-base="z" data-shift="Z">z</button>
    <button class="kbd-key" data-base="x" data-shift="X">x</button>
    <button class="kbd-key" data-base="c" data-shift="C">c</button>
    <button class="kbd-key" data-base="v" data-shift="V">v</button>
    <button class="kbd-key" data-base="b" data-shift="B">b</button>
    <button class="kbd-key" data-base="n" data-shift="N">n</button>
    <button class="kbd-key" data-base="m" data-shift="M">m</button>
    <button class="kbd-key" data-base="," data-shift="&lt;">,</button>
    <button class="kbd-key" data-base="." data-shift="&gt;">.</button>
    <button class="kbd-key" data-base="/" data-shift="?">/</button>
    <button class="kbd-key kbd-special w88 kbd-shift" data-special="Shift">Shift</button>
  </div>
  <div class="kbd-row">
    <button class="kbd-key kbd-space" data-base="Space">Espacio</button>
  </div>
</div>

<script>
let viewportW = 1280, viewportH = 720;
const screenEl = document.getElementById('screen');
const statusEl = document.getElementById('status');
const coordsEl = document.getElementById('coords');
const curUrlEl = document.getElementById('curUrl');

function setStatus(msg) { statusEl.textContent = msg; }

async function refresh() {
  screenEl.src = '/screen?t=' + Date.now();
}

async function loadInfo() {
  try {
    const resp = await fetch('/screen-info?t=' + Date.now());
    const info = await resp.json();
    viewportW = info.width || 1280;
    viewportH = info.height || 720;
    curUrlEl.textContent = info.url || '';
  } catch(e) {}
}

screenEl.addEventListener('load', loadInfo);

screenEl.addEventListener('mousemove', (e) => {
  const rect = screenEl.getBoundingClientRect();
  const x = Math.round((e.clientX - rect.left) * (viewportW / rect.width));
  const y = Math.round((e.clientY - rect.top) * (viewportH / rect.height));
  coordsEl.textContent = 'x=' + x + ' y=' + y;
});

screenEl.addEventListener('click', async (e) => {
  const rect = screenEl.getBoundingClientRect();
  const x = Math.round((e.clientX - rect.left) * (viewportW / rect.width));
  const y = Math.round((e.clientY - rect.top) * (viewportH / rect.height));
  setStatus('Clic en ' + x + ',' + y + '...');
  try {
    const resp = await fetch('/action', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action: 'clickxy', x: x, y: y })
    });
    await resp.json();
    setStatus('Clic enviado.');
  } catch(e) { setStatus('Error: ' + e); }
  setTimeout(refresh, 400);
});

async function scrollPage(dy) {
  setStatus('Scrolleando ' + (dy > 0 ? 'abajo' : 'arriba') + '...');
  try {
    const resp = await fetch('/action', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action: 'scroll', value: String(dy) })
    });
    await resp.json();
  } catch(e) {}
  setTimeout(refresh, 300);
}

async function navigate() {
  const url = document.getElementById('navigateInput').value.trim();
  if (!url) return;
  setStatus('Navegando a ' + url + '...');
  try {
    const resp = await fetch('/action', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action: 'navigate', value: url })
    });
    await resp.json();
  } catch(e) { setStatus('Error: ' + e); }
  setTimeout(() => { refresh(); loadInfo(); }, 1500);
}
document.getElementById('navigateInput').addEventListener('keydown', (e)=>{ if(e.key==='Enter') navigate(); });

async function doAction(action) {
  setStatus('Ejecutando ' + action + '...');
  try {
    const resp = await fetch('/action', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action: action })
    });
    await resp.json();
  } catch(e) {}
  setTimeout(() => { refresh(); loadInfo(); }, 1200);
}

async function typeText() {
  const text = document.getElementById('typeInput').value;
  if (!text) { setStatus('Escribí un texto primero.'); return; }
  setStatus('Escribiendo "' + text.substring(0,20) + '"...');
  try {
    const resp = await fetch('/action', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action: 'typefocused', value: text })
    });
    const res = await resp.json();
    setStatus(res.error ? 'Error: ' + res.error : 'Texto escrito.');
  } catch(e) { setStatus('Error: ' + e); }
  setTimeout(() => { refresh(); loadInfo(); }, 300);
}

async function pressKey(key) {
  setStatus('Presionando ' + key + '...');
  try {
    const resp = await fetch('/action', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action: 'press', key: key })
    });
    const res = await resp.json();
    setStatus(res.error ? 'Error: ' + res.error : key + ' presionado.');
  } catch(e) { setStatus('Error: ' + e); }
  setTimeout(() => { refresh(); loadInfo(); }, 300);
}

// ---- On-screen keyboard ----
let shifted = false;
function flashKey(el) { el.classList.add('pressed'); setTimeout(() => el.classList.remove('pressed'), 140); }
function kbdLabel(el) { if (el.dataset.shift) el.textContent = (shifted && el.dataset.shift) ? el.dataset.shift : el.dataset.base; }
function updateShift() {
  document.querySelectorAll('.kbd-key[data-base][data-shift]').forEach(kbdLabel);
  document.querySelectorAll('.kbd-shift').forEach(el => el.classList.toggle('active', shifted));
}
function sendKey(ch) {
  fetch('/action', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ action: 'press', key: ch })
  }).then(r => r.json()).catch(() => {})
    .then(() => { setTimeout(() => { refresh(); loadInfo(); }, 300); });
}
function kbClick(el) {
  flashKey(el);
  const special = el.dataset.special;
  if (special === 'Shift') { shifted = !shifted; updateShift(); return; }
  if (special) { sendKey(special); return; }
  const ch = (shifted && el.dataset.shift) ? el.dataset.shift : el.dataset.base;
  sendKey(ch === 'Space' ? ' ' : ch);
}
document.querySelectorAll('.kbd-key').forEach(el => el.addEventListener('click', () => kbClick(el)));

// Physical keyboard passthrough: typing with your own keyboard while the
// viewer page has focus sends a real key event to the remote browser.
// Skipped while the cursor is inside the viewer's own inputs (URL, text box).
document.addEventListener('keydown', (e) => {
  const t = e.target;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA')) return;
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  e.preventDefault();
  if (e.key === 'Shift' || e.key === 'CapsLock' || e.key === 'ContextMenu') return;
  sendKey(e.key === ' ' ? ' ' : e.key);
});

document.getElementById('typeInput').addEventListener('keydown', (e)=>{ if(e.key==='Enter') typeText(); });

refresh();
setInterval(refresh, 1000);
loadInfo();
</script>
</body>
</html>`))
}

// actionHandler performs browser actions (click, type, navigate)
func actionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Action   string `json:"action"`             // "navigate", "click", "clickxy", "type", "press", "wait"
		Value    string `json:"value"`              // URL, text, or key
		Selector string `json:"selector,omitempty"` // CSS selector for click/type
		X        int    `json:"x,omitempty"`        // x coordinate for clickxy
		Y        int    `json:"y,omitempty"`        // y coordinate for clickxy
		Key      string `json:"key,omitempty"`      // key for press action
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	var err error
	switch req.Action {
	case "navigate":
		err = chromedp.Run(pool.ctx, chromedp.Navigate(req.Value))
	case "click":
		err = chromedp.Run(pool.ctx, chromedp.Click(req.Selector, chromedp.ByQuery))
	case "clickxy":
		// Click at absolute viewport coordinates
		err = chromedp.Run(pool.ctx,
			chromedp.MouseClickXY(float64(req.X), float64(req.Y)),
			chromedp.Sleep(300*time.Millisecond),
		)
	case "type":
		err = chromedp.Run(pool.ctx, chromedp.SendKeys(req.Selector, req.Value, chromedp.ByQuery))
	case "typefocused":
		// Click first if coordinates provided (to focus the field), then
		// type into the currently focused element via Input.insertText
		if req.X != 0 || req.Y != 0 {
			err = chromedp.Run(pool.ctx,
				chromedp.MouseClickXY(float64(req.X), float64(req.Y)),
				chromedp.Sleep(400*time.Millisecond),
			)
			if err != nil {
				break
			}
		}
		// Focus the active element, then insert each rune with a real key
		// event (keyDown/char/keyUp) — the same per-key path the on-screen
		// keyboard uses and the only one React reliably registers. A single
		// KeyEvent(whole string) is unreliable and can silently drop text.
		actions := []chromedp.Action{chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, evalErr := runtime.Evaluate(`(() => { const el = document.activeElement; if (el) el.focus(); return true; })()`).WithAwaitPromise(true).Do(ctx)
			return evalErr
		})}
		for _, r := range req.Value {
			actions = append(actions, chromedp.KeyEvent(string(r)))
		}
		err = chromedp.Run(pool.ctx, actions...)
	case "press":
		// Send a key press to the focused element
		err = chromedp.Run(pool.ctx,
			chromedp.Sleep(100*time.Millisecond),
			chromedp.KeyEvent(req.Key),
		)
	case "wait":
		err = chromedp.Run(pool.ctx, chromedp.Sleep(2*time.Second))
	case "scroll":
		// Scroll the page: value is pixel delta, positive=down, negative=up
		var dy int
		dy, err = strconv.Atoi(req.Value)
		if err != nil {
			err = fmt.Errorf("scroll value must be a number")
			break
		}
		if dy == 0 {
			err = fmt.Errorf("scroll value must be non-zero")
			break
		}
		err = chromedp.Run(pool.ctx, chromedp.Evaluate(
			fmt.Sprintf("window.scrollBy(0, %d)", dy), &struct{}{},
		))
	case "screenshot":
		// Return screenshot inline
		var buf []byte
		err = chromedp.Run(pool.ctx, chromedp.CaptureScreenshot(&buf))
		if err == nil {
			w.Header().Set("Content-Type", "image/png")
			w.Write(buf)
			return
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func browseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req BrowseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	if req.WaitMS == 0 {
		req.WaitMS = 2000
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	var html string
	err := chromedp.Run(pool.ctx,
		chromedp.Navigate(req.URL),
		chromedp.Sleep(time.Duration(req.WaitMS)*time.Millisecond),
		chromedp.OuterHTML("html", &html),
	)

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(BrowseResponse{Error: err.Error()})
		return
	}
	json.NewEncoder(w).Encode(BrowseResponse{HTML: html})
}

func deepseekHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req DeepSeekRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "deepseek-chat"
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	content, statusCode, raw, err := executeDeepSeekViaBrowser(req)

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(DeepSeekResponse{
			Error:      err.Error(),
			StatusCode: statusCode,
			RawResponse: raw,
		})
		return
	}
	json.NewEncoder(w).Encode(DeepSeekResponse{
		Content:    content,
		Model:      req.Model,
		StatusCode: statusCode,
	})
}

func extractCookiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req BrowseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		req.URL = "https://chat.deepseek.com"
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	var cookies string
	err := chromedp.Run(pool.ctx,
		chromedp.Navigate(req.URL),
		chromedp.Sleep(3*time.Second),
		chromedp.ActionFunc(func(ctx context.Context) error {
			// Use JS to get cookies
			var result interface{}
			if err := chromedp.Evaluate(`document.cookie`, &result).Do(ctx); err != nil {
				return err
			}
			if s, ok := result.(string); ok {
				cookies = s
			}
			return nil
		}),
	)

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"cookies": cookies})
}

// --- Core: execute DeepSeek request inside real browser ---

func executeDeepSeekViaBrowser(req DeepSeekRequest) (string, int, string, error) {
	// Escape prompt for JS string
	escapedPrompt := strings.ReplaceAll(req.Prompt, `\`, `\\`)
	escapedPrompt = strings.ReplaceAll(escapedPrompt, `"`, `\"`)
	escapedPrompt = strings.ReplaceAll(escapedPrompt, "\n", `\n`)
	escapedPrompt = strings.ReplaceAll(escapedPrompt, "\r", ``)

	escapedModel := req.Model
	escapedBearer := req.BearerToken

	var statusCode int

	var rawResult interface{}
	err := chromedp.Run(pool.ctx,
		// Navigate to DeepSeek first to load the domain
		chromedp.Navigate("https://chat.deepseek.com"),
		chromedp.Sleep(2*time.Second),
		// Set cookies via CDP
		chromedp.ActionFunc(func(ctx context.Context) error {
			cookieStr := req.Cookies
			if cookieStr == "" {
				return nil
			}
			pairs := strings.Split(cookieStr, "; ")
			for _, pair := range pairs {
				parts := strings.SplitN(pair, "=", 2)
				if len(parts) == 2 {
					cookieParams := &network.SetCookieParams{
						Name:   parts[0],
						Value:  parts[1],
						Domain: "chat.deepseek.com",
						Path:   "/",
					}
					if err := cookieParams.Do(ctx); err != nil {
						log.Printf("Failed to set cookie %s: %v", parts[0], err)
					}
				}
			}
			log.Printf("Set %d cookies in browser", len(pairs))
			return nil
		}),
		// Reload page with cookies
		chromedp.Navigate("https://chat.deepseek.com"),
		chromedp.Sleep(2*time.Second),
		// Execute fetch and wait for result using CDP Runtime.evaluate
		chromedp.ActionFunc(func(ctx context.Context) error {
			// Use CDP directly to evaluate async JS and wait for promise
			script := fmt.Sprintf(`(async () => {
				const sid = crypto.randomUUID();
				const body = JSON.stringify({
					model: "%s",
					messages: [{ role: "user", content: "%s" }],
					stream: false,
					chat_session_id: sid
				});
				const headers = {
					"Content-Type": "application/json",
					"Authorization": "Bearer %s",
					"Origin": "https://chat.deepseek.com",
					"Referer": "https://chat.deepseek.com/",
					"x-client-bundle-id": "com.deepseek.chat",
					"x-client-locale": "es",
					"x-client-platform": "web",
					"x-client-timezone-offset": "-14400",
					"x-client-version": "2.4.0"
				};
				try {
					const resp = await fetch("https://chat.deepseek.com/api/v0/chat/completion", {
						method: "POST",
						headers: headers,
						body: body,
						credentials: "include"
					});
					const text = await resp.text();
					return { status: resp.status, body: text.substring(0, 4000), name: "" };
				} catch(e) {
					return { status: 0, body: e.message, name: e.name };
				}
			})()`, escapedModel, escapedPrompt, escapedBearer)

			// Use cdproto Runtime to evaluate and wait for promise
			result, _, err := runtime.Evaluate(script).WithAwaitPromise(true).Do(ctx)
			if err != nil {
				return err
			}
			if result.Value != nil {
				rawResult = result.Value
			}
			return nil
		}),
	)

	if err != nil {
		return "", 0, "", fmt.Errorf("browser execution failed: %w", err)
	}

	// Convert result to string
	var rawResponse string
	switch v := rawResult.(type) {
	case string:
		rawResponse = v
	default:
		b, _ := json.Marshal(rawResult)
		rawResponse = string(b)
	}

	// Parse the response
	var result struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		URL    string `json:"url,omitempty"`
		OK     bool   `json:"ok,omitempty"`
		Stack  string `json:"stack,omitempty"`
		Name   string `json:"name,omitempty"`
	}
	if err := json.Unmarshal([]byte(rawResponse), &result); err != nil {
		return "", 0, rawResponse, fmt.Errorf("failed to parse browser response: %w", err)
	}

	log.Printf("Fetch result: status=%d url=%s ok=%v name=%s body=%.200s", result.Status, result.URL, result.OK, result.Name, result.Body)

	statusCode = result.Status
	if statusCode != 200 {
		return "", statusCode, result.Body, fmt.Errorf("DeepSeek returned status %d: %s", statusCode, result.Body)
	}

	// Parse SSE or JSON response
	content := parseSSEResponse(result.Body)
	if content != "" {
		return content, statusCode, "", nil
	}

	// Try JSON parse
	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.Body), &apiResp); err == nil {
		if apiResp.Error != nil {
			return "", statusCode, result.Body, fmt.Errorf("API error: %s", apiResp.Error.Message)
		}
		if len(apiResp.Choices) > 0 {
			return apiResp.Choices[0].Message.Content, statusCode, "", nil
		}
	}

	return result.Body, statusCode, "", nil
}

// parseSSEResponse extracts content from Server-Sent Events
func parseSSEResponse(sse string) string {
	var content string
	lines := strings.Split(sse, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				if len(event.Choices) > 0 {
					content += event.Choices[0].Delta.Content
				}
			}
		}
	}
	return content
}
