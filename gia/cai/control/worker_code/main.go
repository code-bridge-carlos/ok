// worker-code — un worker superligero para el ecosistema Carlos.
//
// Es un agente de implementación mínimo ("inspirado en OpenCode" pero recortado):
// se registra como worker en el panel C1, drena la cola por HTTP long-polling y
// SOLO implementa las tareas que C1 le envía (un archivo + el prompt de la tarea).
// No planifica (eso es Carlos Code C3), no navega, no coordina otras PCs: se queda
// con las herramientas mínimas para implementar (read/write/edit/glob/grep/run
// confinados al PROJECT_PATH) y reporta el resultado al panel.
//
// Variables de entorno:
//   PANEL_URL        Base HTTP del panel C1 (ej. https://bridgecarlos.onrender.com)
//   WORKER_TOKEN     CONTROL_TOKEN compartido con el servidor
//   WORKER_ID        ID único de este worker (por defecto hostname)
//   WORKER_NAME      Nombre para mostrar en el panel
//   WORKER_PERF      Rendimiento 1-100 (para reparto %)
//   WORKER_WEIGHT    Peso 0-100 para reparto por %
//   PROJECT_PATH     Ruta del checkout del repo donde implementar (obligatorio)
//   GATEWAY_URL      URL del Gateway C2 (para generar la implementación)
//   GIT_REPO         Repo local para commit (por defecto PROJECT_PATH)
//   GIT_COMMIT       true/false — commitear tras implementar (default false)
//   GIT_PUSH         true/false — hacer push tras commit (default false)
//   POLL_INTERVAL    Segundos entre polls (default 10, máximo 15 por cap Render)

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Config struct {
	PanelURL     string
	Token        string
	WorkerID     string
	WorkerName   string
	WorkerPerf   int
	WorkerWeight int
	ProjectPath  string
	GatewayURL   string
	GitRepo      string
	GitCommit    bool
	GitPush      bool
	PollInterval int
}

// Cada worker se configura por ENV VAR (PanelURL, WORKER_TOKEN, WORKER_ID,
// WORKER_NAME, PROJECT_PATH, GIT_COMMIT/PUSH, etc.) desde su servicio Render.
// No hay perfiles hardcodeados por cuenta.

var cfg Config
var startTime = time.Now()

// ---------------------------------------------------------------------------
// Console: single in-memory section per worker (TUI text backend).
// Workers never touch Engram/Git/Render; they only implement the sub-prompt
// sent by C1. This section keeps a text log of the current task progress
// (max 50k estimated tokens = len/4) with auto-clear. No /sections endpoint.
// ---------------------------------------------------------------------------

const consoleMaxTokens = 50000

var (
	consoleMu      sync.Mutex
	consoleSection strings.Builder
	consoleSubsMu  sync.Mutex
	consoleSubs    = map[chan string]struct{}{}
)

func consoleEstTokens() int {
	return consoleSection.Len() / 4
}

func consoleAppend(s string) {
	consoleMu.Lock()
	consoleSection.WriteString(s)
	over := consoleSection.Len()/4 >= consoleMaxTokens
	if over {
		consoleSection.Reset()
	}
	consoleMu.Unlock()
	if over {
		log.Printf("console auto-clear (50k tokens)")
	}
}

func consoleClear() {
	consoleMu.Lock()
	consoleSection.Reset()
	consoleMu.Unlock()
	log.Printf("console cleared")
}

// consoleEmit appends a human-readable line to the section and broadcasts
// the SSE message to all /console/stream subscribers.
func consoleEmit(event string, data map[string]interface{}) {
	b, _ := json.Marshal(data)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(b))
	line := fmt.Sprintf("[%s] %s\n", event, string(b))
	consoleAppend(line)
	consoleSubsMu.Lock()
	for ch := range consoleSubs {
		select {
		case ch <- msg:
		default:
		}
	}
	consoleSubsMu.Unlock()
}

func consoleSubscribe() (chan string, func()) {
	ch := make(chan string, 64)
	consoleSubsMu.Lock()
	consoleSubs[ch] = struct{}{}
	consoleSubsMu.Unlock()
	return ch, func() {
		consoleSubsMu.Lock()
		delete(consoleSubs, ch)
		close(ch)
		consoleSubsMu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Config / helpers
// ---------------------------------------------------------------------------

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "true" || v == "1" || strings.EqualFold(v, "yes")
}

func loadConfig() error {
	host, _ := os.Hostname()
	cfg = Config{
		PanelURL:     strings.TrimRight(getEnv("PANEL_URL", ""), "/"),
		Token:        getEnv("WORKER_TOKEN", ""),
		WorkerID:     getEnv("WORKER_ID", host),
		WorkerName:   getEnv("WORKER_NAME", host),
		WorkerPerf:   atoi(getEnv("WORKER_PERF", "5")),
		WorkerWeight: atoi(getEnv("WORKER_WEIGHT", "10")),
		ProjectPath:  getEnv("PROJECT_PATH", ""),
		GatewayURL:   getEnv("GATEWAY_URL", "https://carlos-gateway.onrender.com"),
		GitRepo:      getEnv("GIT_REPO", ""),
		GitCommit:    getEnvBool("GIT_COMMIT", false),
		GitPush:      getEnvBool("GIT_PUSH", false),
		PollInterval: atoi(getEnv("POLL_INTERVAL", "10")),
	}
	if cfg.ProjectPath == "" {
		return fmt.Errorf("PROJECT_PATH es obligatorio")
	}
	if cfg.PanelURL == "" {
		return fmt.Errorf("PANEL_URL es obligatorio")
	}
	if cfg.GitRepo == "" {
		cfg.GitRepo = cfg.ProjectPath
	}
	if cfg.PollInterval < 2 {
		cfg.PollInterval = 2
	}
	if cfg.PollInterval > 15 {
		cfg.PollInterval = 15 // cap del proxy de Render ~20s
	}
	return nil
}

func atoi(s string) int {
	n := 0
	fmt.Sscanf(s, "%d", &n)
	return n
}

// jsonString devuelve s como literal JSON válido (con comillas y escapes).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// HTTP hacia el panel C1
// ---------------------------------------------------------------------------

var client = &http.Client{Timeout: 20 * time.Second}

func postAction(body map[string]interface{}) error {
	u := fmt.Sprintf("%s/api/worker-action?token=%s&id=%s",
		cfg.PanelURL, url.QueryEscape(cfg.Token), url.QueryEscape(cfg.WorkerID))
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("postAction %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func register() {
	hw := detectHardware()
	body := map[string]interface{}{
		"tipo":   "registro",
		"id":     cfg.WorkerID,
		"name":   cfg.WorkerName,
		"perf":   cfg.WorkerPerf,
		"weight": cfg.WorkerWeight,
		"brand":  hw.brand,
		"model":  hw.model,
		"os":     hw.os,
	}
	if err := postAction(body); err != nil {
		log.Printf("registro falló: %v", err)
	} else {
		log.Printf("registrado como %q", cfg.WorkerName)
	}
}

// poll drena la cola una vez (long-poll <= PollInterval s).
func poll() ([]map[string]interface{}, error) {
	u := fmt.Sprintf("%s/api/worker-poll?token=%s&id=%s&timeout=%d",
		cfg.PanelURL, url.QueryEscape(cfg.Token), url.QueryEscape(cfg.WorkerID), cfg.PollInterval)
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Messages, nil
}

// ---------------------------------------------------------------------------
// Hardware
// ---------------------------------------------------------------------------

type hwInfo struct{ brand, model, os string }

func detectHardware() hwInfo {
	h := hwInfo{os: runtimeOS()}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	if runtimeOS() == "linux" {
		h.brand = read("/sys/class/dmi/id/sys_vendor")
		h.model = read("/sys/class/dmi/id/product_name")
	}
	return h
}

func runtimeOS() string {
	return strings.ToLower(os.Getenv("WORKER_OS"))
}

// ---------------------------------------------------------------------------
// Mensajes del servidor
// ---------------------------------------------------------------------------

func handleMessage(m map[string]interface{}) {
	tipo, _ := m["tipo"].(string)
	switch tipo {
	case "tarea":
		runTask(m)
	case "status":
		_ = postAction(map[string]interface{}{"tipo": "estado", "estado": "disponible", "task_id": nil})
	case "restart":
		log.Printf("reinicio pedido por el servidor (%v)", m["reason"])
	case "recall", "return_progress", "cancel":
		// worker-code es secuencial y no mantiene procesos largos abortables:
		// los ignoramos (el servidor reintenta/reasigna). Implementa un archivo
		// a la vez de forma atómica.
		log.Printf("ignorando msg tipo=%s", tipo)
	default:
		log.Printf("tipo desconocido: %s", tipo)
	}
}

// ---------------------------------------------------------------------------
// Ejecución de una subtarea de implementación
// ---------------------------------------------------------------------------

func runTask(m map[string]interface{}) {
	taskID, _ := m["task_id"].(string)
	subtaskID, _ := m["subtask_id"].(string)
	kind, _ := m["kind"].(string)
	globalPrompt, _ := m["prompt"].(string)
	filePrompt, _ := m["file_prompt"].(string)
	files := toStringSlice(m["files"])

	_ = postAction(map[string]interface{}{"tipo": "estado", "estado": "ocupado"})
	log.Printf("tarea %s/%s kind=%s files=%v", taskID, subtaskID, kind, files)
	consoleEmit("thinking", map[string]interface{}{"task_id": taskID, "subtask_id": subtaskID, "kind": kind, "files": files})

	var out strings.Builder
	status := "done"

	// Solo implementamos subtareas de trabajo (kind=work). Los planes los arma C3.
	if kind != "" && kind != "work" {
		out.WriteString(fmt.Sprintf("[worker-code] kind %q no soportado", kind))
		status = "done"
	} else if len(files) == 0 {
		out.WriteString("[worker-code] sin archivos para implementar")
	} else {
		for _, f := range files {
			consoleEmit("preparing_write", map[string]interface{}{"task_id": taskID, "subtask_id": subtaskID, "file": f})
			res := implementFile(f, globalPrompt, filePrompt)
			consoleEmit("tokens", map[string]interface{}{"task_id": taskID, "subtask_id": subtaskID, "file": f, "tokens": len(res) / 4})
			out.WriteString(res)
			out.WriteString("\n")
		}
		if cfg.GitCommit {
			out.WriteString(gitCommit(taskID))
		}
	}

	_ = postAction(map[string]interface{}{"tipo": "progress", "task_id": taskID, "subtask_id": subtaskID, "kind": "work", "output": out.String()})
	_ = postAction(map[string]interface{}{"tipo": "resultado", "task_id": taskID, "subtask_id": subtaskID, "kind": "work", "output": out.String(), "status": status})
	_ = postAction(map[string]interface{}{"tipo": "estado", "estado": "disponible"})
	consoleEmit("done", map[string]interface{}{"task_id": taskID, "subtask_id": subtaskID, "status": status, "tokens": out.Len() / 4})
	log.Printf("tarea %s/%s terminada (%s)", taskID, subtaskID, status)
}

func toStringSlice(v interface{}) []string {
	var out []string
	switch t := v.(type) {
	case []interface{}:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = t
	case string:
		out = []string{t}
	}
	return out
}

// ---------------------------------------------------------------------------
// Implementación de un archivo (las "herramientas" mínimas del worker)
// ---------------------------------------------------------------------------

// implementFile es el corazón del worker: lee el archivo objetivo en el repo,
// pide al Gateway la implementación concreta y la aplica (write/edit atómico).
// Solo usa read/write sobre PROJECT_PATH. Devuelve un texto de log para el panel.
func implementFile(relPath, globalPrompt, filePrompt string) string {
	// Seguridad: la ruta DEBE vivir dentro de PROJECT_PATH.
	target, err := safePath(cfg.ProjectPath, relPath)
	if err != nil {
		return fmt.Sprintf("[worker-code] ruta inválida %q: %v", relPath, err)
	}

	oldContent, readErr := os.ReadFile(target)
	oldText := string(oldContent)

	// Combinar contexto de la tarea con el archivo actual.
	req := fmt.Sprintf(`Eres un agente de implementación mínimo. Modificá SOLO el archivo indicado.
TAREA GLOBAL: %s
%s
ARCHIVO A IMPLEMENTAR: %s
CONTENIDO ACTUAL DEL ARCHIVO:
%s

Instrucciones:
1. Determiná los cambios concretos para cumplir la tarea en ESTE archivo.
2. Devolvé EXCLUSIVAMENTE el contenido COMPLETO y final del archivo (nada de explicaciones, nada de markdown, nada de fragmentos).
3. Si no hacés cambios, devolvé el contenido original sin tocar.`,
		globalPrompt, filePrompt, relPath, oldText)

	newText, gErr := generateContent(req)
	if gErr != nil {
		if readErr != nil {
			return fmt.Sprintf("[worker-code] error: no se pudo leer %s (%v) ni generar (%v)", relPath, readErr, gErr)
		}
		return fmt.Sprintf("[worker-code] error generando contenido para %s: %v", relPath, gErr)
	}

	// Aplicar solo si cambió el contenido.
	if newText == oldText {
		return fmt.Sprintf("[worker-code] %s: sin cambios", relPath)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Sprintf("[worker-code] error creando dir para %s: %v", relPath, err)
	}
	if err := os.WriteFile(target, []byte(newText), 0o644); err != nil {
		return fmt.Sprintf("[worker-code] error escribiendo %s: %v", relPath, err)
	}
	return fmt.Sprintf("[worker-code] %s: implementado (%d -> %d bytes)", relPath, len(oldText), len(newText))
}

// generateContent pide al Gateway C2 el contenido final del archivo.
func generateContent(req string) (string, error) {
	payload := fmt.Sprintf(`{
		"model": "deepseek-chat",
		"messages": [{"role":"user","content":%s}],
		"stream": false,
		"temperature": 0.2
	}`, jsonString(req))

	resp, err := client.Post(cfg.GatewayURL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		return "", fmt.Errorf("gateway connect: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gateway HTTP %d: %s", resp.StatusCode, string(body))
	}
	var gw struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &gw); err != nil {
		return "", fmt.Errorf("gateway parse: %w", err)
	}
	if len(gw.Choices) == 0 {
		return "", fmt.Errorf("gateway sin choices")
	}
	return strings.TrimSpace(gw.Choices[0].Message.Content), nil
}

// safePath resuelve relPath dentro de root; error si escapa o es absoluto.
func safePath(root, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("ruta vacía")
	}
	p := filepath.Join(root, relPath)
	absRoot, _ := filepath.Abs(root)
	absP, _ := filepath.Abs(p)
	if !strings.HasPrefix(absP, absRoot+string(filepath.Separator)) && absP != absRoot {
		return "", fmt.Errorf("fuera de PROJECT_PATH")
	}
	return absP, nil
}

func gitCommit(taskID string) string {
	repo := cfg.GitRepo
	msg := fmt.Sprintf("worker-code: tarea %s", taskID)
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out) + " err:" + err.Error()
		}
		return string(out)
	}
	var sb strings.Builder
	sb.WriteString(run("add", "-A"))
	sb.WriteString(run("commit", "-m", msg))
	if cfg.GitPush {
		sb.WriteString(run("push"))
	}
	return "\n[git] " + sb.String()
}

// ---------------------------------------------------------------------------
// Health server (para correr como web service en Render plan free)
// ---------------------------------------------------------------------------

// startHealthServer levanta en PORT (Render inyecta PORT=10000) un /health que
// responde 200. Lo necesitamos porque Render en free plan solo admite web
// services; el worker de fondo corre como web service sin puerto de entrada real.
func startHealthServer() {
	port := getEnv("PORT", "10000")
	mux := http.NewServeMux()
	cors := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(204)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/health", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","worker":%s}`, jsonString(cfg.WorkerID))
	}))
	mux.HandleFunc("/metrics", cors(func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ram_mb":%.1f,"ram_sys":%.1f,"goroutines":%d,"uptime_sec":%.0f}`,
			float64(m.Alloc)/1024/1024, float64(m.Sys)/1024/1024, runtime.NumGoroutine(), time.Since(startTime).Seconds())
	}))
	mux.HandleFunc("/models", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `["deepseek-chat","deepseek-reasoner"]`)
	}))
	mux.HandleFunc("/console/commands", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"commands":["/models","/clear","/commands"]}`)
	}))
	mux.HandleFunc("/console/clear", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		consoleClear()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	mux.HandleFunc("/console/stream", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ch, unsub := consoleSubscribe()
		defer unsub()
		consoleMu.Lock()
		snapshot := consoleSection.String()
		tokens := consoleSection.Len() / 4
		consoleMu.Unlock()
		fmt.Fprintf(w, "event: thinking\ndata: %s\n\n", jsonString("attached tokens="+fmt.Sprint(tokens)))
		if snapshot != "" {
			if len(snapshot) > 4000 {
				snapshot = snapshot[len(snapshot)-4000:]
			}
			fmt.Fprintf(w, "event: tokens\ndata: %s\n\n", jsonString(snapshot))
		}
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprint(w, msg)
				fl.Flush()
			}
		}
	}))
	mux.HandleFunc("/", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "worker-code (C6) en línea — worker id: %s\n", cfg.WorkerID)
	}))
	go func() {
		log.Printf("health server en :%s", port)
		if err := http.ListenAndServe(":"+port, mux); err != nil {
			log.Printf("health server error: %v", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// Loop principal
// ---------------------------------------------------------------------------

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("worker-code arrancando (id=%s, project=%s, panel=%s)", cfg.WorkerID, cfg.ProjectPath, cfg.PanelURL)

	startHealthServer()
	register()
	backoff := 2 * time.Second
	lastReg := time.Now()
	const regInterval = 90 * time.Second
	for {
		msgs, err := poll()
		if err != nil {
			log.Printf("poll error: %v — reintento en %s", err, backoff)
			time.Sleep(backoff)
			backoff = time.Duration(float64(backoff) * 1.5)
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			// re-registro por si el server reinició
			register()
			lastReg = time.Now()
			continue
		}
		backoff = 2 * time.Second
		for _, m := range msgs {
			handleMessage(m)
		}
		// El panel pierde en memoria el registro de workers al reiniciarse;
		// re-registrar periódicamente aunque el poll funcione.
		if time.Since(lastReg) >= regInterval {
			register()
			lastReg = time.Now()
		}
	}
}
