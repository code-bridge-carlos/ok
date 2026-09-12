package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds Carlos Code configuration
type Config struct {
	Port        string
	GatewayURL  string
	EngramURL   string
	Context7URL string
	PanelURL    string
	EditMode    bool
	ProjectPath string
	GitHubToken string
	RenderAPIKey string // optional: enables deploy trigger in /closeout
	RepoOwner   string
	RepoName    string
	// EmgranRepoOwner/Name point at the GitHub mirror for final snapshots.
	EmgranRepoOwner string
	EmgranRepoName  string
}

var config Config

// carlosCodePool is the pgx pool to Supabase for writing carlos_code_history.
// Nil when DATABASE_URL is not set — fire-and-forget writes degrade gracefully.
var carlosCodePool *pgxpool.Pool

// mcpHub is the global MCP client registry. C5 (context7_grep) is registered
// in main() best-effort so the planner can discover its tools.
var mcpHub = NewMCPClient()

// initCarlosCodeHistory connects to Supabase and creates the carlos_code_history
// table. Best-effort: errors are logged, never crash the service.
func initCarlosCodeHistory() {
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		log.Printf("[cc-history] DATABASE_URL no seteado: historial deshabilitado")
		return
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		log.Printf("[cc-history] parse fallo: %v", err)
		return
	}
	cfg.MaxConns = 2
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["sslmode"] = "require"
	cfg.ConnConfig.RuntimeParams["statement_cache_size"] = "0"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Printf("[cc-history] pool fallo: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		log.Printf("[cc-history] ping fallo: %v", err)
		pool.Close()
		return
	}
	carlosCodePool = pool
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS carlos_code_history (
			id SERIAL PRIMARY KEY,
			prompt TEXT,
			plan_summary TEXT,
			mode TEXT,
			workers INT,
			status TEXT,
			created_at TIMESTAMPTZ DEFAULT now()
		)`); err != nil {
		log.Printf("[cc-history] create table fallo: %v", err)
		return
	}
	if _, err := pool.Exec(ctx,
		`CREATE INDEX IF NOT EXISTS idx_carlos_code_history_created_at ON carlos_code_history(created_at)`); err != nil {
		log.Printf("[cc-history] create index fallo: %v", err)
		return
	}
	log.Printf("[cc-history] tabla carlos_code_history lista")
}

// saveCarlosCodeHistory writes a carlos_code_history row fire-and-forget.
// Failures are logged only — the plan/update response is never blocked.
func saveCarlosCodeHistory(prompt, planSummary, mode string, workers int, status string) {
	if carlosCodePool == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := carlosCodePool.Exec(ctx,
			`INSERT INTO carlos_code_history (prompt, plan_summary, mode, workers, status)
			 VALUES ($1, $2, $3, $4, $5)`,
			prompt, planSummary, mode, workers, status)
		if err != nil {
			log.Printf("[cc-history] save fallo: %v", err)
		}
	}()
}

// ─── F2 conversation sections (RAM-active + Supabase mirror) ─────────────────
// The active section lives in RAM for low-latency streaming; every mutation is
// mirrored to the carlos_sections table best-effort (fire-and-forget).
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type carlosSection struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	Messages  []chatMessage `json:"messages"`
	Tokens    int           `json:"tokens"`
	UpdatedAt time.Time     `json:"updated_at"`
}

var sectionsStore = struct {
	sync.RWMutex
	byID   map[string]*carlosSection
	active string
}{byID: make(map[string]*carlosSection)}

// streamCancels tracks in-flight /chat/stream requests by request_id so
// /chat/cancel can abort them mid-stream.
var streamCancels = struct {
	sync.Mutex
	m map[string]context.CancelFunc
}{m: make(map[string]context.CancelFunc)}

func registerStreamCancel(id string, fn context.CancelFunc) {
	streamCancels.Lock()
	defer streamCancels.Unlock()
	streamCancels.m[id] = fn
}

func removeStreamCancel(id string) {
	streamCancels.Lock()
	defer streamCancels.Unlock()
	delete(streamCancels.m, id)
}

func cancelStream(id string) bool {
	streamCancels.Lock()
	defer streamCancels.Unlock()
	fn, ok := streamCancels.m[id]
	if !ok {
		return false
	}
	fn()
	delete(streamCancels.m, id)
	return true
}

// initCarlosSections creates the carlos_sections table reusing carlosCodePool.
// Best-effort: errors are logged, never crash the service.
func initCarlosSections() {
	if carlosCodePool == nil {
		log.Printf("[cc-sections] pool no disponible: secciones solo en RAM")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := carlosCodePool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS carlos_sections (
			id TEXT PRIMARY KEY,
			title TEXT,
			messages_json TEXT,
			tokens INT,
			updated_at TIMESTAMPTZ DEFAULT now()
		)`); err != nil {
		log.Printf("[cc-sections] create table fallo: %v", err)
		return
	}
	log.Printf("[cc-sections] tabla carlos_sections lista")
	if _, err := carlosCodePool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS service_tables (
			service_id TEXT,
			key TEXT,
			value TEXT,
			updated_at TIMESTAMPTZ DEFAULT now(),
			PRIMARY KEY (service_id, key)
		)`); err != nil {
		log.Printf("[cc-sections] create service_tables fallo: %v", err)
		return
	}
	log.Printf("[cc-sections] tabla service_tables lista")
}

// loadServiceTables reads per-service key/value tables from Supabase.
// Empty serviceID returns all services as map[serviceID]map[key]value.
// Best-effort: any failure returns an empty map.
func loadServiceTables(serviceID string) map[string]map[string]string {
	out := map[string]map[string]string{}
	if carlosCodePool == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	scan := func(q string, args ...interface{}) {
		rows, err := carlosCodePool.Query(ctx, q, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var sid, k, v string
			if err := rows.Scan(&sid, &k, &v); err != nil {
				continue
			}
			if _, ok := out[sid]; !ok {
				out[sid] = map[string]string{}
			}
			out[sid][k] = v
		}
	}
	if serviceID != "" {
		scan(`SELECT service_id, key, value FROM service_tables WHERE service_id=$1 ORDER BY key`, serviceID)
	} else {
		scan(`SELECT service_id, key, value FROM service_tables ORDER BY service_id, key`)
	}
	return out
}

// saveServiceTableRow upserts one key/value row. Returns false on failure.
func saveServiceTableRow(serviceID, key, value string) bool {
	if carlosCodePool == nil || serviceID == "" || key == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := carlosCodePool.Exec(ctx, `
		INSERT INTO service_tables (service_id, key, value, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (service_id, key) DO UPDATE SET value=$3, updated_at=now()`,
		serviceID, key, value)
	return err == nil
}

// deleteServiceTableRow removes one key/value row. Returns false on failure.
func deleteServiceTableRow(serviceID, key string) bool {
	if carlosCodePool == nil || serviceID == "" || key == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := carlosCodePool.Exec(ctx,
		`DELETE FROM service_tables WHERE service_id=$1 AND key=$2`,
		serviceID, key)
	return err == nil
}

// serviceTablesBlock formats all service tables compactly for prompt
// injection so Carlos Code always knows each service's table. Capped.
func serviceTablesBlock() string {
	tabs := loadServiceTables("")
	if len(tabs) == 0 {
		return ""
	}
	var parts []string
	for sid, kv := range tabs {
		var pairs []string
		for k, v := range kv {
			pairs = append(pairs, k+"="+v)
		}
		parts = append(parts, sid+": "+strings.Join(pairs, ", "))
	}
	out := "## TABLAS DE SERVICIOS (cuentas, keys y datos por servicio)\n" + strings.Join(parts, "\n")
	if len(out) > 2000 {
		out = out[:2000] + "\n...[recortado]..."
	}
	return out
}

// saveCarlosSection mirrors a section to Supabase fire-and-forget.
func saveCarlosSection(sec carlosSection) {
	if carlosCodePool == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		msgs, _ := json.Marshal(sec.Messages)
		_, err := carlosCodePool.Exec(ctx, `
			INSERT INTO carlos_sections (id, title, messages_json, tokens, updated_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (id) DO UPDATE SET
				title=$2, messages_json=$3, tokens=$4, updated_at=now()`,
			sec.ID, sec.Title, string(msgs), sec.Tokens)
		if err != nil {
			log.Printf("[cc-sections] save fallo: %v", err)
		}
	}()
}

// ensureSection returns the section for id, creating it when needed. An empty
// id resolves to the active section, creating a fresh one when none exists.
func ensureSection(id string) *carlosSection {
	sectionsStore.Lock()
	defer sectionsStore.Unlock()
	if id == "" {
		id = sectionsStore.active
	}
	if id != "" {
		if sec, ok := sectionsStore.byID[id]; ok {
			sectionsStore.active = id
			return sec
		}
	}
	if id == "" {
		id = fmt.Sprintf("sec-%d", time.Now().UnixNano())
	}
	sec := &carlosSection{ID: id, Messages: []chatMessage{}, UpdatedAt: time.Now().UTC()}
	sectionsStore.byID[id] = sec
	sectionsStore.active = id
	return sec
}

// snapshotSection returns a detached copy for safe serialization.
func snapshotSection(sec *carlosSection) carlosSection {
	cp := carlosSection{
		ID: sec.ID, Title: sec.Title, Tokens: sec.Tokens, UpdatedAt: sec.UpdatedAt,
		Messages: append([]chatMessage(nil), sec.Messages...),
	}
	return cp
}

// loadSectionFromDB hydrates a RAM section from Supabase when missing locally.
func loadSectionFromDB(id string) {
	if carlosCodePool == nil || id == "" {
		return
	}
	sectionsStore.RLock()
	_, ok := sectionsStore.byID[id]
	sectionsStore.RUnlock()
	if ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var title, msgsJSON string
	var tokens int
	err := carlosCodePool.QueryRow(ctx,
		`SELECT title, messages_json, tokens FROM carlos_sections WHERE id=$1`, id).
		Scan(&title, &msgsJSON, &tokens)
	if err != nil {
		return
	}
	var msgs []chatMessage
	if msgsJSON != "" {
		_ = json.Unmarshal([]byte(msgsJSON), &msgs)
	}
	if msgs == nil {
		msgs = []chatMessage{}
	}
	sectionsStore.Lock()
	sectionsStore.byID[id] = &carlosSection{
		ID: id, Title: title, Messages: msgs, Tokens: tokens, UpdatedAt: time.Now().UTC(),
	}
	sectionsStore.Unlock()
}

// estimateTokens approximates token usage as len/4 (no tokenizer available).
func estimateTokens(s string) int {
	return len(s) / 4
}

// Section context budget: 200k tokens per section, auto-compact at 85%.
const (
	sectionMaxTokens = 200000
	sectionCompactAt = 170000 // 85% of sectionMaxTokens
)

// autoCompactIfNeeded triggers a background compact when a section reaches
// 85% of its budget. Fire-and-forget: never blocks the chat response.
func autoCompactIfNeeded(id string) {
	sectionsStore.RLock()
	sec, ok := sectionsStore.byID[id]
	over := ok && sec.Tokens >= sectionCompactAt
	sectionsStore.RUnlock()
	if !over {
		return
	}
	go func() {
		if _, _, compacted := doCompact(id); compacted {
			log.Printf("[cc-sections] auto-compact %s al 85%% del presupuesto", id)
		}
	}()
}

// evictInactiveSections persists and drops every non-active RAM section so
// only the selected one consumes memory. Listed sections still resolve from
// Supabase via loadSectionFromDB on next select.
func evictInactiveSections() {
	sectionsStore.Lock()
	active := sectionsStore.active
	// Safety: if no active section, don't evict anything (would lose all sections)
	if active == "" {
		sectionsStore.Unlock()
		log.Printf("[cc-sections] eviction skipped: no active section set")
		return
	}
	var snaps []carlosSection
	for id, sec := range sectionsStore.byID {
		if id == active {
			continue
		}
		snaps = append(snaps, snapshotSection(sec))
		delete(sectionsStore.byID, id)
	}
	sectionsStore.Unlock()
	for _, s := range snaps {
		saveCarlosSection(s)
	}
	if len(snaps) > 0 {
		log.Printf("[cc-sections] evictadas %d secciones inactivas de RAM", len(snaps))
	}
}

// sectionTitle derives a short title from the first user prompt.
func sectionTitle(prompt string) string {
	t := strings.TrimSpace(prompt)
	t = strings.ReplaceAll(t, "\n", " ")
	if len(t) > 60 {
		t = t[:60]
	}
	return t
}

// gatewayChat calls the Gateway chat completions endpoint with an http.Client
// plus timeout (never exec curl). It returns the assistant content and any
// reasoning content for reasoner models.
func gatewayChat(ctx context.Context, model string, messages []chatMessage) (string, string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model":       model,
		"messages":    messages,
		"stream":      false,
		"temperature": 0.2,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		config.GatewayURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("gateway returned %d", resp.StatusCode)
	}
	var gwResp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &gwResp); err != nil {
		return "", "", fmt.Errorf("failed to parse gateway response: %w", err)
	}
	if len(gwResp.Choices) == 0 {
		return "", "", fmt.Errorf("empty response from gateway")
	}
	content := gwResp.Choices[0].Message.Content
	if content == "" {
		content = gwResp.Choices[0].Text
	}
	return content, gwResp.Choices[0].Message.ReasoningContent, nil
}

// saveEngramTask posts a "Task {id} completed" observation to the Engram MCP
// service fire-and-forget. Failures are logged only — never block the response.
func saveEngramTask(title, content string, tags []string) {
	if config.EngramURL == "" {
		return
	}
	go func() {
		body := map[string]interface{}{
			"title":   title,
			"type":    "task",
			"topic":   "tasks/" + title,
			"content": content,
			"tags":    tags,
		}
		payload, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", config.EngramURL+"/save",
			bytes.NewReader(payload))
		if err != nil {
			log.Printf("[engram] save request failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("[engram] save failed: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			log.Printf("[engram] save returned %d", resp.StatusCode)
		}
	}()
}

// saveEmgranSnapshot writes the final summary write-through to Engram
// (Supabase via C4) and to the EMGRAN GitHub mirror, fire-and-forget.
// Failures are logged only — never block the response.
func saveEmgranSnapshot(title, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	if strings.TrimSpace(title) == "" {
		title = "Snapshot"
	}
	go func() {
		// (a) Engram POST, same payload shape as saveEngramTask.
		saveEngramTask(title, content, []string{"snapshot"})
		// (b) GitHub mirror best-effort.
		if config.GitHubToken == "" {
			log.Printf("[emgran] GITHUB_TOKEN not set: github mirror skipped")
			return
		}
		owner := config.EmgranRepoOwner
		repo := config.EmgranRepoName
		if owner == "" || repo == "" {
			log.Printf("[emgran] mirror repo not configured: github mirror skipped")
			return
		}
		// Skip the upsert when the mirror repo does not exist or is not
		// accessible; never block the finalize response.
		if out, status, err := emgranGhAPI("GET", "/repos/"+owner+"/"+repo, nil); err != nil || status != 200 {
			if err != nil {
				log.Printf("[emgran] mirror repo check failed: %v", err)
			} else {
				log.Printf("[emgran] mirror repo %s/%s not accessible (status %d): %s", owner, repo, status, strings.TrimSpace(string(out)))
			}
			return
		}
		date := time.Now().UTC().Format("2006-01-02")
		path := "snapshots/" + date + "/" + emgranSlug(title) + ".md"
		apiPath := "/repos/" + owner + "/" + repo + "/contents/" + path
		fileBody := "# " + title + "\n\n" + content + "\n"
		sha := ""
		if existing, status, err := emgranGhAPI("GET", apiPath, nil); err == nil && status == 200 {
			var file struct {
				Sha string `json:"sha"`
			}
			if json.Unmarshal(existing, &file) == nil {
				sha = file.Sha
			}
		}
		putBody := map[string]interface{}{
			"message": "Snapshot " + date + " " + title,
			"content": base64.StdEncoding.EncodeToString([]byte(fileBody)),
		}
		if sha != "" {
			putBody["sha"] = sha
		}
		out, status, err := emgranGhAPI("PUT", apiPath, putBody)
		if err != nil {
			log.Printf("[emgran] mirror upsert failed: %v", err)
			return
		}
		if status != 200 && status != 201 {
			log.Printf("[emgran] mirror upsert returned %d", status)
			return
		}
		_ = out
	}()
}

// emgranSlug derives a filesystem-safe slug from a title.
func emgranSlug(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen && b.Len() > 0 {
			b.WriteRune('-')
			prevHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}
	if slug == "" {
		slug = fmt.Sprintf("snapshot-%d", time.Now().Unix())
	}
	return slug
}

// emgranGhAPI performs one GitHub API request for the EMGRAN mirror repo.
// The token is only sent in the Authorization header, never logged.
func emgranGhAPI(method, apiPath string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(jsonBytes)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+apiPath, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+config.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

func main() {
	config = Config{
		Port:        getEnv("PORT", "8002"),
		GatewayURL:  getEnv("GATEWAY_URL", "https://carlos-gateway.onrender.com"),
		EngramURL:   getEnv("ENGRAM_URL", "https://engram-mcp-q9ln.onrender.com"),
		Context7URL: getEnv("CONTEXT7_URL", "https://context7-grep.onrender.com"),
		PanelURL:    getEnv("PANEL_URL", "https://bridgecarlos.onrender.com"),
		EditMode:    getEnv("EDIT_MODE", "false") == "true",
		ProjectPath: getEnv("PROJECT_PATH", "/workspaces/cai"),
		GitHubToken: getEnv("GITHUB_TOKEN", ""),
		RenderAPIKey: getEnv("RENDER_API_KEY", ""),
		RepoOwner:   getEnv("REPO_OWNER", "1000carlospena-prog"),
		RepoName:    getEnv("REPO_NAME", "cai"),
		// F7: GitHub mirror for finalized Engram snapshots.
		EmgranRepoOwner: getEnv("EMGRAN_REPO_OWNER", "1000carlos-prog"),
		EmgranRepoName:  getEnv("EMGRAN_REPO_NAME", "EMGRAN"),
	}

	log.Printf("Carlos Code starting on port %s", config.Port)
	log.Printf("Gateway: %s", config.GatewayURL)
	log.Printf("Project: %s", config.ProjectPath)

	// Connect to Supabase and create carlos_code_history table (best-effort).
	initCarlosCodeHistory()
	// Create carlos_sections table (best-effort, reuses the same pool).
	initCarlosSections()

	// Register C5 MCP server best-effort so getPlanFromGateway can list tools.
	if strings.TrimSpace(config.Context7URL) != "" {
		mcpHub.AddServer("c5", config.Context7URL)
	}

	// Routes
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/metrics", metricsHandler)
	http.HandleFunc("/plan", planHandler)
	http.HandleFunc("/subplans", subplansHandler)
	http.HandleFunc("/tools", toolsHandler)
	http.HandleFunc("/update", updateHandler)
	http.HandleFunc("/finalize", finalizeHandler)
	http.HandleFunc("/closeout", closeoutHandler)
	http.HandleFunc("/git", gitHandler)
	// F1 chat streaming (SSE) + cancellation.
	http.HandleFunc("/chat/stream", chatStreamHandler)
	http.HandleFunc("/chat/cancel", chatCancelHandler)
	// F1 models catalog (coherent with gateway modelsHandler ids).
	http.HandleFunc("/models", modelsHandler)
	// F2 conversation sections. C1 user commands map to these endpoints
	// (interpreted in C1, not here): /sections -> GET /sections,
	// /models -> GET /models, /clear -> POST /sections/clear,
	// /commands -> GET /tools, /compact -> POST /sections/compact,
	// /start and /ready -> GET /health.
	http.HandleFunc("/sections", sectionsHandler)
	http.HandleFunc("/seccions", sectionsHandler) // alias pedido por el usuario
	http.HandleFunc("/service-tables", serviceTablesHandler)
	http.HandleFunc("/service-table", serviceTableHandler)
	http.HandleFunc("/sections/select", sectionSelectHandler)
	http.HandleFunc("/sections/clear", sectionClearHandler)
	http.HandleFunc("/sections/compact", sectionCompactHandler)

	// CORS middleware
	handler := corsMiddleware(http.DefaultServeMux)

	log.Printf("Server listening on :%s", config.Port)
	if err := http.ListenAndServe(":"+config.Port, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// ─── F1 chat streaming (SSE) + cancellation ─────────────────────────────────
// POST /chat/stream {prompt, section_id, request_id, model} emits SSE events:
// token, thinking, tool_call, engram_query, tokens, cancelled, done.
func chatStreamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Prompt    string `json:"prompt"`
		SectionID string `json:"section_id"`
		RequestID string `json:"request_id"`
		Model     string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	model := req.Model
	if model != "deepseek-chat" && model != "deepseek-reasoner" {
		model = "deepseek-chat"
	}
	requestID := req.RequestID
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}

	ctx, cancel := context.WithCancel(r.Context())
	registerStreamCancel(requestID, cancel)
	defer func() {
		cancel()
		removeStreamCancel(requestID)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	send := func(event string, payload interface{}) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(data))
		flusher.Flush()
	}

	// Engram context (best-effort) re-emitted as an engram_query event.
	memory := engramMemory(req.Prompt, 5)
	send("engram_query", map[string]interface{}{
		"request_id": requestID,
		"memory":     memory,
	})

	// Append the user message to the section (RAM-active, Supabase mirror).
	sec := ensureSection(req.SectionID)
	sectionsStore.Lock()
	if sec.Title == "" {
		sec.Title = sectionTitle(req.Prompt)
	}
	sec.Messages = append(sec.Messages, chatMessage{Role: "user", Content: req.Prompt})
	sec.Tokens += estimateTokens(req.Prompt)
	sec.UpdatedAt = time.Now().UTC()
	history := append([]chatMessage(nil), sec.Messages...)
	sectionID := sec.ID
	sectionsStore.Unlock()

	// Trim history sent to the gateway to the last 20 messages.
	if len(history) > 20 {
		history = history[len(history)-20:]
	}

	content, reasoning, err := gatewayChat(ctx, model, history)
	if err != nil {
		if ctx.Err() != nil {
			send("cancelled", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
			})
			return
		}
		log.Printf("chat stream gateway error: %v", err)
		send("done", map[string]interface{}{
			"request_id": requestID, "section_id": sectionID,
			"error": err.Error(),
		})
		return
	}

	// Initial configuration sent as a token event so the client can display
	// the system prompt / temperature / Engram context immediately.
	send("token", map[string]interface{}{
		"request_id": requestID, "section_id": sectionID,
		"delta": fmt.Sprintf(`{"system":"Contexto cargado desde Engram y configuración del sesión. Temperatura: 0.2. Actúa como asistente de planificación inteligente. Usa las tools disponibles y respeta las secciones."}`),
	})

	if reasoning != "" {
		send("thinking", map[string]interface{}{
			"request_id": requestID, "section_id": sectionID, "delta": reasoning,
		})
	}

	// Forward any detected tool calls as 'tool_call' events.
	// (The DeepSeek gateway currently does not return structured tool calls,
	//  but the event format is kept for compatibility with the C3 TUI.)
	if strings.Contains(content, "<tool_calls>") || strings.Contains(content, "function_call") {
		send("tool_call", map[string]interface{}{
			"request_id": requestID, "section_id": sectionID,
			"delta": "tool_calls_detected",
		})
	}

	// Re-emit the gateway content in chunks so clients can render a stream.
	runes := []rune(content)
	const chunkSize = 100
	for i := 0; i < len(runes); i += chunkSize {
		select {
		case <-ctx.Done():
			send("cancelled", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
			})
			return
		default:
		}
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		send("token", map[string]interface{}{
			"request_id": requestID, "section_id": sectionID,
			"delta": string(runes[i:end]), "index": i / chunkSize,
		})
	}

	// Process commands after stream completes.
	// Only acts on the final completed response, not on cancelled streams.
	if ctx.Err() == nil {
		processCarlosCodeCommand(send, content, sectionID, requestID)
	}

	// Persist the assistant reply.
	sectionsStore.Lock()
	if live, ok := sectionsStore.byID[sectionID]; ok {
		live.Messages = append(live.Messages, chatMessage{Role: "assistant", Content: content})
		live.Tokens += estimateTokens(content)
		live.UpdatedAt = time.Now().UTC()
		sec = live
	}
	snap := snapshotSection(sec)
	sectionsStore.Unlock()
	saveCarlosSection(snap)
	autoCompactIfNeeded(sectionID)

	inTokens := estimateTokens(req.Prompt)
	outTokens := estimateTokens(content)
	send("tokens", map[string]interface{}{
		"request_id": requestID, "section_id": sectionID,
		"input": inTokens, "output": outTokens, "total": inTokens + outTokens,
	})
	send("done", map[string]interface{}{
		"request_id": requestID, "section_id": sectionID, "model": model,
	})
}

// chatCancelHandler aborts an in-flight /chat/stream request.
// POST /chat/cancel {request_id}.
func chatCancelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.RequestID == "" {
		http.Error(w, "request_id is required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"request_id": req.RequestID,
		"cancelled":  cancelStream(req.RequestID),
	})
}

// modelsHandler returns the model catalog, coherent with the gateway ids.
func modelsHandler(w http.ResponseWriter, r *http.Request) {
	models := []map[string]interface{}{
		{"id": "deepseek-chat", "object": "model", "owned_by": "deepseek", "number": 1},
		{"id": "deepseek-reasoner", "object": "model", "owned_by": "deepseek", "number": 2},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
		"models": []string{"deepseek-chat", "deepseek-reasoner"},
	})
}

// ─── F2 conversation sections ─────────────────────────────────────────────
// GET /sections lists sections (active overlaid from RAM, rest from Supabase).
func sectionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type summary struct {
		ID        string    `json:"id"`
		Title     string    `json:"title"`
		Tokens    int       `json:"tokens"`
		UpdatedAt time.Time `json:"updated_at"`
		Active    bool      `json:"active"`
	}
	out := []summary{}
	sectionsStore.RLock()
	active := sectionsStore.active
	for _, sec := range sectionsStore.byID {
		out = append(out, summary{
			ID: sec.ID, Title: sec.Title, Tokens: sec.Tokens,
			UpdatedAt: sec.UpdatedAt, Active: sec.ID == active,
		})
	}
	sectionsStore.RUnlock()
	if carlosCodePool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := carlosCodePool.Query(ctx,
			`SELECT id, title, tokens, updated_at FROM carlos_sections ORDER BY updated_at DESC LIMIT 50`)
		if err == nil {
			defer rows.Close()
			seen := map[string]bool{}
			for _, s := range out {
				seen[s.ID] = true
			}
			for rows.Next() {
				var s summary
				if err := rows.Scan(&s.ID, &s.Title, &s.Tokens, &s.UpdatedAt); err != nil {
					continue
				}
				s.Active = s.ID == active
				if !seen[s.ID] {
					out = append(out, s)
					seen[s.ID] = true
				}
			}
		} else {
			log.Printf("[cc-sections] list fallo: %v", err)
		}
	}
	if out == nil {
		out = []summary{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sections": out,
		"active":   active,
	})
}

// sectionSelectHandler activates a section, hydrating it from Supabase when it
// only exists there. POST /sections/select {section_id}.
func sectionSelectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SectionID string `json:"section_id"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	id := req.SectionID
	if id == "" {
		id = req.ID
	}
	if id != "" {
		loadSectionFromDB(id)
	}
	sec := ensureSection(id)
	if req.SectionID == "" && req.ID == "" {
		sectionsStore.Lock()
		fresh := &carlosSection{
			ID:       fmt.Sprintf("sec-%d", time.Now().UnixNano()),
			Messages: []chatMessage{}, UpdatedAt: time.Now().UTC(),
		}
		sectionsStore.byID[fresh.ID] = fresh
		sectionsStore.active = fresh.ID
		sec = fresh
		sectionsStore.Unlock()
	}
	sectionsStore.RLock()
	snap := snapshotSection(sec)
	sectionsStore.RUnlock()
	// Only the selected section stays in RAM; the rest live in Supabase.
	evictInactiveSections()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active":  snap.ID,
		"section": snap,
	})
}

// sectionClearHandler empties a section (active by default).
// POST /sections/clear {section_id?}.
func sectionClearHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SectionID string `json:"section_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.SectionID != "" {
		loadSectionFromDB(req.SectionID)
	}
	sec := ensureSection(req.SectionID)
	sectionsStore.Lock()
	sec.Messages = []chatMessage{}
	sec.Tokens = 0
	sec.UpdatedAt = time.Now().UTC()
	snap := snapshotSection(sec)
	sectionsStore.Unlock()
	saveCarlosSection(snap)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"section_id": snap.ID,
		"cleared":    true,
	})
}

// serviceTablesHandler reads per-service tables.
// GET /service-tables[?service_id=] -> {"tables": {service: {k: v}}}.
func serviceTablesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sid := r.URL.Query().Get("service_id")
	tabs := loadServiceTables(sid)
	if tabs == nil {
		tabs = map[string]map[string]string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"tables": tabs})
}

// serviceTableHandler writes per-service tables.
// POST /service-table {service_id, key, value} upserts.
// DELETE /service-table?service_id=&key= removes one row.
func serviceTableHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			ServiceID string `json:"service_id"`
			Key       string `json:"key"`
			Value     string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.ServiceID) == "" || strings.TrimSpace(req.Key) == "" {
			http.Error(w, "service_id and key are required", http.StatusBadRequest)
			return
		}
		ok := saveServiceTableRow(req.ServiceID, strings.TrimSpace(req.Key), req.Value)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": ok})
	case http.MethodDelete:
		sid := r.URL.Query().Get("service_id")
		key := r.URL.Query().Get("key")
		if sid == "" || key == "" {
			http.Error(w, "service_id and key are required", http.StatusBadRequest)
			return
		}
		ok := deleteServiceTableRow(sid, key)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": ok})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// doCompact summarizes a section via the gateway and trims it, keeping the
// summary plus the most recent messages. Returns summary, token count and
// whether it compacted. Shared by the manual handler and auto-compact.
func doCompact(id string) (summary string, tokens int, compacted bool) {
	if id != "" {
		loadSectionFromDB(id)
	}
	sec := ensureSection(id)
	sectionsStore.RLock()
	history := append([]chatMessage(nil), sec.Messages...)
	sectionID := sec.ID
	sectionsStore.RUnlock()

	const keepLast = 6
	if len(history) <= keepLast {
		return "", 0, false
	}

	// Build a summarization prompt from the older messages.
	var sb strings.Builder
	sb.WriteString("Resume esta conversacion en 10 lineas o menos, preservando decisiones, contexto y tareas abiertas:\n\n")
	for _, m := range history[:len(history)-keepLast] {
		sb.WriteString(m.Role + ": " + m.Content + "\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	summary, _, err := gatewayChat(ctx, "deepseek-chat", []chatMessage{
		{Role: "user", Content: sb.String()},
	})
	if err != nil {
		log.Printf("[cc-sections] compact gateway error: %v", err)
		summary = "Resumen no disponible (gateway inaccesible); se conservan los mensajes recientes."
	}

	compactedMsgs := []chatMessage{
		{Role: "system", Content: "Resumen compactado: " + summary},
	}
	compactedMsgs = append(compactedMsgs, history[len(history)-keepLast:]...)

	sectionsStore.Lock()
	total := 0
	if live, ok := sectionsStore.byID[sectionID]; ok {
		live.Messages = compactedMsgs
		for _, m := range compactedMsgs {
			total += estimateTokens(m.Content)
		}
		live.Tokens = total
		live.UpdatedAt = time.Now().UTC()
		sec = live
	}
	snap := snapshotSection(sec)
	sectionsStore.Unlock()
	saveCarlosSection(snap)
	return summary, snap.Tokens, true
}

// sectionCompactHandler summarizes via the gateway and trims the section,
// keeping the summary plus the most recent messages.
// POST /sections/compact {section_id?}.
func sectionCompactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SectionID string `json:"section_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	summary, tokens, compacted := doCompact(req.SectionID)
	sec := ensureSection(req.SectionID)
	sectionsStore.RLock()
	snap := snapshotSection(sec)
	sectionsStore.RUnlock()
	if !compacted {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"section_id": snap.ID,
			"compacted":  false,
			"reason":     "section too short to compact",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"section_id": snap.ID,
		"compacted":  true,
		"summary":    summary,
		"messages":   snap.Messages,
		"tokens":     tokens,
	})
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// Health check handler
func healthHandler(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"status":    "ok",
		"edit_mode": config.EditMode,
		"project":   config.ProjectPath,
		"gateway":   config.GatewayURL,
		"engram":    config.EngramURL,
		"context7":  config.Context7URL,
		"timestamp": time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ram_mb":     float64(m.Alloc) / 1024 / 1024,
		"ram_sys":    float64(m.Sys) / 1024 / 1024,
		"goroutines": runtime.NumGoroutine(),
		"uptime_sec": time.Since(startTime).Seconds(),
	})
}

var startTime = time.Now()

// fetchWorkersFromPanel queries C1 for available workers.
func fetchWorkersFromPanel() []string {
	if config.PanelURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(config.PanelURL + "/api/workers")
	if err != nil {
		log.Printf("fetchWorkers: %v", err)
		return nil
	}
	defer resp.Body.Close()
	var data struct {
		Workers []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"workers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}
	var ids []string
	for _, w := range data.Workers {
		if w.Status == "disponible" {
			ids = append(ids, w.ID)
		}
	}
	return ids
}

// Plan handler - main endpoint for task planning
func planHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// If no workers passed, fetch from C1 panel
	if len(req.Workers) == 0 {
		if panelWorkers := fetchWorkersFromPanel(); len(panelWorkers) > 0 {
			req.Workers = panelWorkers
			log.Printf("Fetched %d workers from panel", len(panelWorkers))
		}
	}

	prefix := req.Prompt
	if len(prefix) > 50 {
		prefix = prefix[:50]
	}
	log.Printf("Plan request: prompt=%s workers=%v", prefix, req.Workers)

	// Plannings are cancellable via POST /chat/cancel {request_id}, same as
	// chat streams. The gateway call below honors the same context.
	ctx := r.Context()
	if req.RequestID != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), 120*time.Second)
		registerStreamCancel(req.RequestID, cancel)
		defer removeStreamCancel(req.RequestID)
		defer cancel()
	}

	// Try to get plan from Gateway
	plan, err := getPlanFromGateway(ctx, req)
	if err != nil {
		log.Printf("Gateway error: %v, falling back to direct assignment", err)
		// Fallback: simple direct assignment
		plan = fallbackPlan(req)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(plan)

	// Fire-and-forget: persist plan history (never block the response).
	saveCarlosCodeHistory(req.Prompt, plan.Note, plan.Mode, len(req.Workers), "ok")
	// F7: the plan stays in carlos_code_history; Engram is written on finalize.
}

// Tools handler - list available tools
func toolsHandler(w http.ResponseWriter, r *http.Request) {
	tools := []map[string]interface{}{
		{
			"name":        "glob",
			"description": "Search files by pattern",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Glob pattern to match files",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			"name":        "grep",
			"description": "Search content in files",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Regex pattern to search",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File or directory to search in",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			"name":        "read",
			"description": "Read file contents",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to read",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "edit",
			"description": "Edit file (exact string replacement)",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to edit",
					},
					"old": map[string]interface{}{
						"type":        "string",
						"description": "String to replace",
					},
					"new": map[string]interface{}{
						"type":        "string",
						"description": "Replacement string",
					},
				},
				"required": []string{"path", "old", "new"},
			},
		},
		{
			"name":        "write",
			"description": "Write new file",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to write",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "File content",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "bash",
			"description": "Execute shell command. BLOCKED and rejected by the executor: destructive commands such as rm -rf, rm --no-preserve-root, mkfs, dd, fork bombs (:(){:|:&};:), shutdown/reboot/halt/poweroff, and recursive writes to / (e.g. chmod -R 777 /).",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "Command to execute",
					},
					"timeout_ms": map[string]interface{}{
						"type":        "integer",
						"description": "Timeout in milliseconds (default: 30000)",
					},
				},
				"required": []string{"command"},
			},
		},
		{
			"name":        "git_status",
			"description": "Get GitHub repo info (name, branch, visibility, clone URL)",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			"name":        "git_list",
			"description": "List files in a directory on GitHub",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Directory path (empty = root)",
					},
				},
			},
		},
		{
			"name":        "git_read",
			"description": "Read file contents from GitHub",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to read",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "git_commit",
			"description": "Create or update a file on GitHub (auto-commits)",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to create/update",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "File content",
					},
					"message": map[string]interface{}{
						"type":        "string",
						"description": "Commit message",
					},
				},
				"required": []string{"path", "content", "message"},
			},
		},
		{
			"name":        "git_delete",
			"description": "Delete a file on GitHub",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to delete",
					},
					"message": map[string]interface{}{
						"type":        "string",
						"description": "Commit message",
					},
					"sha": map[string]interface{}{
						"type":        "string",
						"description": "Current file SHA (from git_read)",
					},
				},
				"required": []string{"path", "message", "sha"},
			},
		},
		{
			"name":        "git_commits",
			"description": "List recent commits on GitHub",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"limit": map[string]interface{}{
						"type":        "string",
						"description": "Number of commits to return (default: 10)",
					},
				},
			},
		},
		{
			"name":        "question",
			"description": "Ask the user a clarifying question and wait for the answer",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"question": map[string]interface{}{
						"type":        "string",
						"description": "Question to ask the user",
					},
					"options": map[string]interface{}{
						"type":        "string",
						"description": "Optional comma-separated answer options",
					},
				},
				"required": []string{"question"},
			},
		},
		{
			"name":        "todowrite",
			"description": "Create or update the task todo list for the current plan",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"todos": map[string]interface{}{
						"type":        "string",
						"description": "JSON array of {content, status, id} todo items",
					},
				},
				"required": []string{"todos"},
			},
		},
		{
			"name":        "webfetch",
			"description": "Fetch a URL and return its content as markdown",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"url": map[string]interface{}{
						"type":        "string",
						"description": "Fully-formed URL to fetch",
					},
					"format": map[string]interface{}{
						"type":        "string",
						"description": "Response format: markdown, text or html (default: markdown)",
					},
				},
				"required": []string{"url"},
			},
		},
		{
			"name":        "websearch",
			"description": "Live web search for up-to-date information",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Search query",
					},
					"num_results": map[string]interface{}{
						"type":        "integer",
						"description": "Number of results to return (default: 8)",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "lsp",
			"description": "Language server query: symbols, references or diagnostics for a file",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"action": map[string]interface{}{
						"type":        "string",
						"description": "One of: symbols, references, diagnostics",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path to query",
					},
					"symbol": map[string]interface{}{
						"type":        "string",
						"description": "Symbol name (for references)",
					},
				},
				"required": []string{"action", "path"},
			},
		},
		{
			"name":        "skill",
			"description": "Load a project skill (SKILL.md) by name to honor project conventions",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Skill name to load",
					},
				},
				"required": []string{"name"},
			},
		},
	}

	resp := map[string]interface{}{
		"tools": tools,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// loadSkills walks the project's skills/ directory and returns the concatenated
// content of every SKILL.md it finds, so planning can honor project conventions.
func loadSkills(projectRoot string) string {
	skillsDir := filepath.Join(projectRoot, "skills")
	var parts []string
	// Walk only if the directory exists; missing/empty is a no-op.
	filepath.Walk(skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries silently
		}
		if info.IsDir() || filepath.Base(path) != "SKILL.md" {
			return nil
		}
		rel, relErr := filepath.Rel(skillsDir, path)
		if relErr != nil {
			rel = path
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		parts = append(parts, fmt.Sprintf("### Skill: %s\n%s", rel, string(data)))
		return nil
	})
	if len(parts) == 0 {
		return ""
	}
	return "## CONVENCIONES DEL PROYECTO (skills)\n" + strings.Join(parts, "\n\n---\n\n")
}

// loadProjectDocs reads project-level rule files (AGENTS.md) so planning
// honors conventions beyond skills/. Best-effort, capped, empty-safe.
func loadProjectDocs(projectRoot string) string {
	for _, name := range []string{"AGENTS.md", "agents.md"} {
		data, err := os.ReadFile(filepath.Join(projectRoot, name))
		if err != nil || len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		text := string(data)
		if len(text) > 4000 {
			text = text[:4000] + "\n...[recortado]..."
		}
		return "## REGLAS DEL PROYECTO (" + name + ")\n" + text
	}
	return ""
}

// sectionHistoryText formats the last maxMsgs messages of a section as
// "usuario:/asistente:" lines, capped at maxChars. Empty when no history.
func sectionHistoryText(id string, maxMsgs, maxChars int) string {
	if id == "" {
		sectionsStore.RLock()
		id = sectionsStore.active
		sectionsStore.RUnlock()
	}
	if id == "" {
		return ""
	}
	loadSectionFromDB(id)
	sectionsStore.RLock()
	sec, ok := sectionsStore.byID[id]
	var msgs []chatMessage
	if ok {
		msgs = append([]chatMessage(nil), sec.Messages...)
	}
	sectionsStore.RUnlock()
	if len(msgs) == 0 {
		return ""
	}
	if len(msgs) > maxMsgs {
		msgs = msgs[len(msgs)-maxMsgs:]
	}
	var b strings.Builder
	b.WriteString("## HISTORIAL DE LA SECCION\n")
	for _, m := range msgs {
		role := "usuario"
		if m.Role == "assistant" {
			role = "asistente"
		}
		b.WriteString(role + ": " + m.Content + "\n")
	}
	out := b.String()
	if len(out) > maxChars {
		out = "...[recortado]...\n" + out[len(out)-maxChars:]
	}
	return out
}

// engramMemory pulls recent + query-relevant observations from the Engram MCP
// service and formats them for injection into the planning prompt. A best effort:
// any failure returns an empty string so planning never breaks on memory down.
func engramMemory(query string, limit int) string {
	if config.EngramURL == "" || limit <= 0 {
		return ""
	}
	if limit > 50 {
		limit = 50
	}

	// Collect unique observations keyed by id to avoid dupes between /recent and /search.
	seen := make(map[int64]struct{})
	recent := engramFetch(config.EngramURL+"/recent?limit=10", seen)
	search := engramFetch(config.EngramURL+"/search?q="+url.QueryEscape(query)+fmt.Sprintf("&limit=%d", limit), seen)

	var lines []string
	for _, o := range append(recent, search...) {
		title, _ := o["title"].(string)
		content, _ := o["content"].(string)
		if title == "" && content == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", title, content))
	}
	if len(lines) == 0 {
		return ""
	}
	return "## MEMORIA DE CONTEXTO (Engram)\n" + strings.Join(lines, "\n")
}

// engramFetch performs one GET against the Engram MCP and returns the typed
// observations from the response envelope. Never fails the caller.
func engramFetch(url string, seen map[int64]struct{}) []map[string]interface{} {
	out := []map[string]interface{}{}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("Engram fetch error (%s): %v", url, err)
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("Engram non-200 (%d) from %s", resp.StatusCode, url)
		return out
	}

	var envelope struct {
		Observations []map[string]interface{} `json:"observations"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return out
	}
	for _, o := range envelope.Observations {
		id, _ := o["id"].(float64)
		key := int64(id)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, o)
	}
	return out
}

// c5ToolsBlock fetches the C5 MCP tool catalog best-effort with a short
// timeout and formats it as a prompt block so the planner knows the
// available git_*, render_*, context7_* and grep_* tools. Any failure or
// empty catalog returns "" so planning continues without tools.
func c5ToolsBlock() string {
	type result struct {
		tools []Tool
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		tools, err := mcpHub.GetTools("c5")
		ch <- result{tools: tools, err: err}
	}()
	var tools []Tool
	select {
	case r := <-ch:
		if r.err != nil {
			log.Printf("C5 tools fetch failed: %v", r.err)
			return ""
		}
		tools = r.tools
	case <-time.After(3 * time.Second):
		log.Printf("C5 tools fetch timed out")
		return ""
	}
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## MCP TOOLS (c5)\n")
	for _, t := range tools {
		desc := strings.TrimSpace(t.Description)
		if len(desc) > 120 {
			desc = desc[:120]
		}
		if desc == "" {
			sb.WriteString("- " + t.Name + "\n")
			continue
		}
		sb.WriteString("- " + t.Name + ": " + desc + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// getPlanFromGateway calls the Gateway to get a plan
func getPlanFromGateway(ctx context.Context, req PlanRequest) (PlanResponse, error) {
	// Gather project conventions (skills/, AGENTS.md), section conversation,
	// persistent memory (Engram) and C5 tools to ground the planner.
	// All are best-effort and empty-safe.
	root := req.ProjectPath
	if root == "" {
		root = config.ProjectPath
	}
	skills := loadSkills(root)
	docs := loadProjectDocs(root)
	history := sectionHistoryText(req.SectionID, 20, 6000)
	memory := engramMemory(req.Prompt, 10)
	// Discover C5 MCP tools best-effort so the planner knows git_*, render_*,
	// context7_* and grep_* exist. Failures degrade to no tools block.
	toolsBlock := c5ToolsBlock()

	contextBlock := ""
	if hist := history; hist != "" {
		contextBlock += hist + "\n\n"
	}
	if skills != "" {
		contextBlock += skills + "\n\n"
	}
	if docs != "" {
		contextBlock += docs + "\n\n"
	}
	if memory != "" {
		contextBlock += memory + "\n\n"
	}
	if toolsBlock != "" {
		contextBlock += toolsBlock + "\n\n"
	}
	// Per-service tables (accounts, keys, notes): Carlos Code always sees
	// and may use them when planning.
	if svcTabs := serviceTablesBlock(); svcTabs != "" {
		contextBlock += svcTabs + "\n\n"
	}
	if contextBlock != "" {
		contextBlock = "## CONTEXTO\n" + contextBlock
	}

	prompt := fmt.Sprintf(`Sos un orquestador de DESARROLLO DE SOFTWARE. La palabra 'plan' significa plan de implementación de código (tareas, archivos, pasos), NUNCA plano arquitectónico de construcción ni diseño de casas/piscinas. Si el pedido no es de software, igual lo tratás como tarea de código/documentación del repo.
Analiza esta tarea y genera un plan de distribución.
Si detectás puntos a mejorar en la tarea (riesgos, archivos faltantes, ambigüedades, sugerencias), incluilos como sugerencias al inicio de note.

TAREA: %s

%sARCHIVOS EXISTENTES: %v

WORKERS DISPONIBLES: %v

Genera un JSON con:
{
  "mode": "percent",
  "note": "descripción breve del plan",
  "assignments": {
    "worker_id": ["archivo1", "archivo2"]
  }
}

Responde SOLO con el JSON, sin texto adicional.`, req.Prompt, contextBlock, req.ExistingFiles, req.Workers)

	// Call gateway with http.Client (context-aware, cancellable) — never curl.
	payload, _ := json.Marshal(map[string]interface{}{
		"model":       "deepseek-chat",
		"messages":    []chatMessage{{Role: "user", Content: prompt}},
		"stream":      false,
		"temperature": 0.2,
	})
	gwReq, err := http.NewRequestWithContext(ctx, "POST",
		config.GatewayURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return PlanResponse{}, fmt.Errorf("gateway request failed: %w", err)
	}
	gwReq.Header.Set("Content-Type", "application/json")
	output, err := http.DefaultClient.Do(gwReq)
	if err != nil {
		return PlanResponse{}, fmt.Errorf("gateway call failed: %w", err)
	}
	defer output.Body.Close()
	raw, _ := io.ReadAll(output.Body)
	if output.StatusCode != 200 {
		return PlanResponse{}, fmt.Errorf("gateway returned %d", output.StatusCode)
	}

	// Parse gateway response
	var gatewayResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(raw, &gatewayResp); err != nil {
		return PlanResponse{}, fmt.Errorf("failed to parse gateway response: %w", err)
	}

	if len(gatewayResp.Choices) == 0 {
		return PlanResponse{}, fmt.Errorf("empty response from gateway")
	}

	content := gatewayResp.Choices[0].Message.Content

	// Parse plan from content
	var plan PlanResponse
	if err := json.Unmarshal([]byte(content), &plan); err != nil {
		// Try to extract JSON from content
		jsonStart := strings.Index(content, "{")
		jsonEnd := strings.LastIndex(content, "}") + 1
		if jsonStart >= 0 && jsonEnd > jsonStart {
			if err := json.Unmarshal([]byte(content[jsonStart:jsonEnd]), &plan); err != nil {
				return PlanResponse{}, fmt.Errorf("failed to parse plan JSON: %w", err)
			}
		} else {
			return PlanResponse{}, fmt.Errorf("no JSON found in response")
		}
	}

	return plan, nil
}

// fallbackPlan creates a simple plan when gateway fails
func fallbackPlan(req PlanRequest) PlanResponse {
	assignments := make(map[string][]string)
	workers := req.Workers
	if len(workers) == 0 {
		workers = []string{"local"}
	}

	// Simple round-robin assignment
	for i, file := range req.ExistingFiles {
		worker := workers[i%len(workers)]
		assignments[worker] = append(assignments[worker], file)
	}

	// Si no hay existing_files (el request no mandó project_path o el repo
	// no existe), derivar rutas del prompt: p.ej. docs/ecosistema.md o
	// *.md, *.py, *.go. Nunca inventar "main.py".
	if len(assignments) == 0 {
		files := filesFromPrompt(req.Prompt)
		if len(files) == 0 {
			files = []string{"README.md"}
		}
		assignments[workers[0]] = files
	}

	note := req.Prompt
	if len(note) > 50 {
		note = note[:50]
	}
	return PlanResponse{
		Mode:        "percent",
		Note:        fmt.Sprintf("Plan directo: %s", note),
		Assignments: assignments,
	}
}

// subplansHandler splits a development plan into 2-8 sequential sub-prompts.
// POST /subplans {prompt, section_id, request_id} -> {"subs": [...]}.
// It asks the gateway (temperature 0.2 via gatewayChat) for the split; on any
// gateway failure it falls back to a local paragraph/sentence split.
func subplansHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Prompt    string `json:"prompt"`
		SectionID string `json:"section_id"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	const systemPrompt = "Dividí este plan de desarrollo de software en 2-8 sub-prompts secuenciales e independientes, cada uno ejecutable por un worker con su archivo(s). Cada sub-prompt debe traer su contexto necesario (archivos, criterios, memoria relevante). Respondé SOLO JSON: {\"subs\": [\"...\", ...]}"
	// Hydrate the split with the section conversation, Engram memory, skills
	// and C5 tools (Gentle-AI style: each sub-task carries what it needs).
	// All best-effort: an empty context still yields a valid split.
	var ctxParts []string
	if hist := sectionHistoryText(req.SectionID, 20, 6000); hist != "" {
		ctxParts = append(ctxParts, hist)
	}
	if mem := engramMemory(req.Prompt, 5); mem != "" {
		ctxParts = append(ctxParts, mem)
	}
	if sk := loadSkills(config.ProjectPath); sk != "" {
		ctxParts = append(ctxParts, sk)
	}
	if tb := c5ToolsBlock(); tb != "" {
		ctxParts = append(ctxParts, tb)
	}
	messages := []chatMessage{
		{Role: "system", Content: systemPrompt},
	}
	if len(ctxParts) > 0 {
		ctxText := "## CONTEXTO PARA DIVIDIR\n" + strings.Join(ctxParts, "\n\n")
		if len(ctxText) > 12000 {
			ctxText = ctxText[:12000] + "\n...[recortado]..."
		}
		messages = append(messages, chatMessage{Role: "user", Content: ctxText})
	}
	messages = append(messages, chatMessage{Role: "user", Content: req.Prompt})
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	if req.RequestID != "" {
		registerStreamCancel(req.RequestID, cancel)
		defer removeStreamCancel(req.RequestID)
	} else {
		defer cancel()
	}
	content, _, err := gatewayChat(ctx, "deepseek-chat", messages)
	subs := []string{}
	if err != nil {
		log.Printf("subplans gateway error: %v, using local fallback", err)
		subs = splitSubsLocal(req.Prompt)
	} else {
		subs = parseSubsJSON(content)
		if len(subs) == 0 {
			log.Printf("subplans: gateway returned no parseable subs, using local fallback")
			subs = splitSubsLocal(req.Prompt)
		}
	}
	if subs == nil {
		subs = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"subs": subs})
}

// parseSubsJSON parses {"subs": [...]} from gateway content, tolerating noise
// by extracting the first {...} block (same convention as getPlanFromGateway).
// It drops empty strings and caps the result at 8.
func parseSubsJSON(content string) []string {
	var parsed struct {
		Subs []string `json:"subs"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		jsonStart := strings.Index(content, "{")
		jsonEnd := strings.LastIndex(content, "}") + 1
		if jsonStart < 0 || jsonEnd <= jsonStart {
			return []string{}
		}
		if err := json.Unmarshal([]byte(content[jsonStart:jsonEnd]), &parsed); err != nil {
			return []string{}
		}
	}
	out := make([]string, 0, len(parsed.Subs))
	for _, s := range parsed.Subs {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
		if len(out) == 8 {
			break
		}
	}
	return out
}

// splitSubsLocal is the local fallback: split by blank lines, then by
// sentence boundaries (".", "!", "?" followed by space and an uppercase,
// digit, "(" or "[" start). Go RE2 has no lookbehind, so the sentence split
// is done manually. Caps at 8.
func splitSubsLocal(text string) []string {
	blankRe := regexp.MustCompile(`\n\s*\n`)
	parts := []string{}
	for _, p := range blankRe.Split(strings.TrimSpace(text), -1) {
		if t := strings.TrimSpace(p); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) <= 1 {
		parts = splitSentences(strings.TrimSpace(text))
	}
	if len(parts) > 8 {
		parts = parts[:8]
	}
	if parts == nil {
		parts = []string{}
	}
	return parts
}

// splitSentences splits text after ".", "!" or "?" when followed by
// whitespace and a sentence starter (upper letter, digit, "(" or "[").
func splitSentences(text string) []string {
	var parts []string
	runes := []rune(text)
	start := 0
	i := 0
	for i < len(runes) {
		if runes[i] == '.' || runes[i] == '!' || runes[i] == '?' {
			j := i + 1
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t' || runes[j] == '\n' || runes[j] == '\r') {
				j++
			}
			if j > i+1 && j < len(runes) {
				next := runes[j]
				if next == '(' || next == '[' || (next >= 'A' && next <= 'Z') || (next >= '0' && next <= '9') {
					chunk := strings.TrimSpace(string(runes[start:j]))
					if len(chunk) > 2 {
						parts = append(parts, chunk)
					}
					start = j
					i = j
					continue
				}
			}
		}
		i++
	}
	if tail := strings.TrimSpace(string(runes[start:])); len(tail) > 2 {
		parts = append(parts, tail)
	} else if len(parts) == 0 && strings.TrimSpace(text) != "" {
		parts = append(parts, strings.TrimSpace(text))
	}
	return parts
}

// filesFromPrompt extrae rutas de archivo que el prompt mencione explícitamente
// (p.ej. docs/ecosistema.md, src/main.go, README.md — con/ sin terminación .ext).
// Devuelve una lista ordenada y única de rutas relativas.
func filesFromPrompt(prompt string) []string {
	seen := map[string]bool{}
	var files []string
	re := regexp.MustCompile(`[A-Za-z0-9_./-]+\.(md|py|go|js|ts|tsx|json|yaml|yml|toml|txt|html|css|sh|sql)`)
	for _, m := range re.FindAllString(prompt, -1) {
		m = strings.Trim(m, " ./")
		if m == "" || strings.Contains(m, "://") {
			continue
		}
		if !seen[m] {
			seen[m] = true
			files = append(files, m)
		}
	}
	return files
}

// jsonEscape marshals a string to a valid, quoted JSON string literal so it can
// be safely embedded inside a larger JSON payload. Returns the value WITH its
// surrounding double quotes (e.g. `"hola\nmundo"`).
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ─── Git Handler (GitHub API) ────────────────────────────────────────────────
func gitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Action string            `json:"action"` // status, clone, commit, push, pull, list, read, update
		Args   map[string]string `json:"args"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if config.GitHubToken == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "GITHUB_TOKEN not configured",
		})
		return
	}

	log.Printf("Git action: %s args=%v", req.Action, req.Args)

	var result map[string]interface{}

	switch req.Action {
	case "status":
		result = ghRepoInfo()
	case "list":
		result = ghListFiles(req.Args["path"])
	case "read":
		result = ghReadFile(req.Args["path"])
	case "commit":
		result = ghCommitFile(req.Args["path"], req.Args["content"], req.Args["message"])
	case "update":
		result = ghUpdateFile(req.Args["path"], req.Args["content"], req.Args["message"], req.Args["sha"])
	case "delete":
		result = ghDeleteFile(req.Args["path"], req.Args["message"], req.Args["sha"])
	case "commits":
		result = ghListCommits(req.Args["limit"])
	default:
		result = map[string]interface{}{"error": "unknown action: " + req.Action}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ─── GitHub API Helpers ──────────────────────────────────────────────────────
func ghAPI(method, path string, body interface{}) ([]byte, int, error) {
	url := "https://api.github.com" + path

	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", "Bearer "+config.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

func ghRepoInfo() map[string]interface{} {
	out, status, err := ghAPI("GET", "/repos/"+config.RepoOwner+"/"+config.RepoName, nil)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	if status != 200 {
		return map[string]interface{}{"error": string(out), "status": status}
	}

	var repo struct {
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		UpdatedAt     string `json:"updated_at"`
	}
	json.Unmarshal(out, &repo)

	return map[string]interface{}{
		"name":           repo.Name,
		"full_name":      repo.FullName,
		"default_branch": repo.DefaultBranch,
		"private":        repo.Private,
		"updated_at":     repo.UpdatedAt,
		"clone_url":      "https://github.com/" + config.RepoOwner + "/" + config.RepoName + ".git",
	}
}

func ghListFiles(path string) map[string]interface{} {
	if path == "" {
		path = ""
	}
	apiPath := "/repos/" + config.RepoOwner + "/" + config.RepoName + "/contents/" + path
	out, status, err := ghAPI("GET", apiPath, nil)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	if status != 200 {
		return map[string]interface{}{"error": string(out), "status": status}
	}

	var files []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
		Size int    `json:"size"`
		Sha  string `json:"sha"`
	}
	json.Unmarshal(out, &files)

	var result []map[string]interface{}
	for _, f := range files {
		result = append(result, map[string]interface{}{
			"name": f.Name,
			"path": f.Path,
			"type": f.Type,
			"size": f.Size,
			"sha":  f.Sha,
		})
	}

	return map[string]interface{}{"files": result, "count": len(result)}
}

func ghReadFile(path string) map[string]interface{} {
	apiPath := "/repos/" + config.RepoOwner + "/" + config.RepoName + "/contents/" + path
	out, status, err := ghAPI("GET", apiPath, nil)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	if status != 200 {
		return map[string]interface{}{"error": string(out), "status": status}
	}

	var file struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Content  string `json:"content"`
		Sha      string `json:"sha"`
		Size     int    `json:"size"`
		Encoding string `json:"encoding"`
	}
	json.Unmarshal(out, &file)

	// Content is base64 encoded
	content, _ := base64.StdEncoding.DecodeString(file.Content)

	return map[string]interface{}{
		"name":    file.Name,
		"path":    file.Path,
		"content": string(content),
		"sha":     file.Sha,
		"size":    file.Size,
	}
}

func ghCommitFile(path, content, message string) map[string]interface{} {
	if message == "" {
		message = "Update " + path
	}

	// Get current SHA first (for update)
	apiPath := "/repos/" + config.RepoOwner + "/" + config.RepoName + "/contents/" + path
	existing, status, _ := ghAPI("GET", apiPath, nil)

	var sha string
	if status == 200 {
		var file struct {
			Sha string `json:"sha"`
		}
		json.Unmarshal(existing, &file)
		sha = file.Sha
	}

	// Create or update
	body := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if sha != "" {
		body["sha"] = sha
	}

	out, status, err := ghAPI("PUT", apiPath, body)
	if err != nil {
		return map[string]interface{}{"error": err.Error(), "success": false}
	}
	if status != 200 && status != 201 {
		return map[string]interface{}{"error": string(out), "status": status, "success": false}
	}

	var result struct {
		Commit struct {
			Sha string `json:"sha"`
		} `json:"commit"`
		Content struct {
			Path string `json:"path"`
			Sha  string `json:"sha"`
		} `json:"content"`
	}
	json.Unmarshal(out, &result)

	return map[string]interface{}{
		"success":    true,
		"commit_sha": result.Commit.Sha,
		"file_sha":   result.Content.Sha,
		"path":       path,
	}
}

func ghUpdateFile(path, content, message, sha string) map[string]interface{} {
	return ghCommitFile(path, content, message)
}

func ghDeleteFile(path, message, sha string) map[string]interface{} {
	if message == "" {
		message = "Delete " + path
	}
	if sha == "" {
		return map[string]interface{}{"error": "sha is required for delete"}
	}

	apiPath := "/repos/" + config.RepoOwner + "/" + config.RepoName + "/contents/" + path
	body := map[string]interface{}{
		"message": message,
		"sha":     sha,
	}

	out, status, err := ghAPI("DELETE", apiPath, body)
	if err != nil {
		return map[string]interface{}{"error": err.Error(), "success": false}
	}
	if status != 200 {
		return map[string]interface{}{"error": string(out), "status": status, "success": false}
	}

	return map[string]interface{}{"success": true, "path": path}
}

func ghListCommits(limit string) map[string]interface{} {
	if limit == "" {
		limit = "10"
	}
	apiPath := "/repos/" + config.RepoOwner + "/" + config.RepoName + "/commits?per_page=" + limit
	out, status, err := ghAPI("GET", apiPath, nil)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	if status != 200 {
		return map[string]interface{}{"error": string(out), "status": status}
	}

	var commits []struct {
		Sha    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	json.Unmarshal(out, &commits)

	var result []map[string]interface{}
	for _, c := range commits {
		result = append(result, map[string]interface{}{
			"sha":     c.Sha[:8],
			"message": c.Commit.Message,
			"author":  c.Commit.Author.Name,
			"date":    c.Commit.Author.Date,
		})
	}

	return map[string]interface{}{"commits": result, "count": len(result)}
}

// ─── Update Handler ───────────────────────────────────────────────────────────
func updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get current project state
	var gitState map[string]interface{}
	if config.GitHubToken == "" {
		gitState = map[string]interface{}{"error": "GITHUB_TOKEN not configured"}
	} else {
		gitState = ghRepoInfo()
	}

	state := map[string]interface{}{
		"project":   config.ProjectPath,
		"git":       gitState,
		"services":  checkServices(),
		"timestamp": time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)

	// Fire-and-forget: persist update history (never block the response).
	saveCarlosCodeHistory("", "update", "update", 0, "ok")
}

// ─── Finalize Handler ─────────────────────────────────────────────────────────
func finalizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Summary string `json:"summary"`
		Context string `json:"context"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Allow empty body
	}

	// Get final state
	state := map[string]interface{}{
		"status":    "finalized",
		"project":   config.ProjectPath,
		"git":       ghRepoInfo(),
		"summary":   req.Summary,
		"context":   req.Context,
		"timestamp": time.Now().UTC(),
	}

	// Save context to file
	if req.Summary != "" {
		contextFile := config.ProjectPath + "/.carlos-context.json"
		contextData, _ := json.MarshalIndent(state, "", "  ")
		os.WriteFile(contextFile, contextData, 0644)
		log.Printf("Context saved to %s", contextFile)
		// F7: write-through to Engram + EMGRAN mirror once all subtasks end.
		title := req.Summary
		if len(title) > 80 {
			title = title[:80]
		}
		content := req.Summary
		if strings.TrimSpace(req.Context) != "" {
			content += "\n\nContext:\n" + req.Context
		}
		saveEmgranSnapshot(title, content)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// renderTriggerDeploy fires a Render deploy for serviceID. Best-effort:
// missing key/service or API errors are returned as error strings, never
// crash the caller. The key travels only in the Authorization header.
func renderTriggerDeploy(serviceID string) (string, error) {
	if config.RenderAPIKey == "" {
		return "", fmt.Errorf("RENDER_API_KEY not configured")
	}
	if serviceID == "" {
		return "", fmt.Errorf("deploy service id missing")
	}
	payload := []byte(`{"clearCache": false}`)
	req, err := http.NewRequest("POST",
		"https://api.render.com/v1/services/"+serviceID+"/deploys",
		bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+config.RenderAPIKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("render returned %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	return out.ID, nil
}

// ─── Closeout Handler ───────────────────────────────────────────────────────
// POST /closeout {task_id, prompt, outputs[], deploy_service_id?} consolidates
// a finished distributed task: Engram+EMGRAN snapshot, closeout record commit
// to the task repo, and optional Render deploy. Every step is best-effort and
// reported in the response; nothing blocks on missing credentials.
func closeoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TaskID          string   `json:"task_id"`
		Prompt          string   `json:"prompt"`
		Outputs         []string `json:"outputs"`
		DeployServiceID string   `json:"deploy_service_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.TaskID) == "" {
		http.Error(w, "task_id is required", http.StatusBadRequest)
		return
	}
	result := map[string]interface{}{"task_id": req.TaskID}

	var sb strings.Builder
	sb.WriteString("Task " + req.TaskID + " completed.\n\nPrompt:\n" + req.Prompt + "\n")
	for i, o := range req.Outputs {
		if len(o) > 2000 {
			o = o[:2000] + "\n...[recortado]..."
		}
		sb.WriteString(fmt.Sprintf("\n--- worker %d ---\n%s\n", i+1, o))
	}
	summary := sb.String()

	// 1. Engram + EMGRAN mirror (final snapshot).
	saveEmgranSnapshot("Task "+req.TaskID+" completed", summary)
	result["engram"] = true

	// 2. Closeout record in the task repo.
	if config.GitHubToken == "" {
		result["commit"] = map[string]interface{}{"ok": false, "error": "GITHUB_TOKEN not configured"}
	} else {
		res := ghCommitFile("closeouts/"+req.TaskID+".md", summary, "closeout tarea "+req.TaskID)
		result["commit"] = res
	}

	// 3. Optional Render deploy (only with key + service id).
	if req.DeployServiceID == "" {
		result["deploy"] = map[string]interface{}{"ok": false, "error": "deploy_service_id missing (skipped)"}
	} else if id, err := renderTriggerDeploy(req.DeployServiceID); err != nil {
		result["deploy"] = map[string]interface{}{"ok": false, "error": err.Error()}
	} else {
		result["deploy"] = map[string]interface{}{"ok": true, "deploy_id": id}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ─── Check Services ──────────────────────────────────────────────────────────
func checkServices() map[string]interface{} {
	services := map[string]string{
		"gateway": config.GatewayURL,
		"engram":  config.EngramURL,
	}

	status := make(map[string]interface{})
	for name, url := range services {
		cmd := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", url+"/health")
		out, err := cmd.CombinedOutput()
		if err != nil {
			status[name] = "error"
		} else {
			code := strings.TrimSpace(string(out))
			if code == "200" {
				status[name] = "ok"
			} else {
				status[name] = "error:" + code
			}
		}
	}
	return status
}

// CORS middleware
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Types
type PlanRequest struct {
	Prompt        string   `json:"prompt"`
	Workers       []string `json:"workers"`
	ExistingFiles []string `json:"existing_files"`
	ProjectPath   string   `json:"project_path"`
	SectionID     string   `json:"section_id"`
	RequestID     string   `json:"request_id"`
}

type PlanResponse struct {
	Mode        string              `json:"mode"`
	Note        string              `json:"note"`
	Assignments map[string][]string `json:"assignments"`
}

// MCP Client for external services
type MCPClient struct {
	mu      sync.RWMutex
	servers map[string]string // name -> URL
}

func NewMCPClient() *MCPClient {
	return &MCPClient{
		servers: make(map[string]string),
	}
}

func (c *MCPClient) AddServer(name, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servers[name] = url
}

func (c *MCPClient) GetTools(serverName string) ([]Tool, error) {
	c.mu.RLock()
	url, ok := c.servers[serverName]
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	// HTTP GET to server's tools endpoint
	cmd := exec.Command("curl", "-s", url+"/tools")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get tools from %s: %w", serverName, err)
	}

	var resp struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(output, &resp); err != nil {
		return nil, err
	}

	return resp.Tools, nil
}

// Tool represents a callable tool
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// processCarlosCodeCommand processes commands from the C3 chat stream.
// Supported commands: /start, /ready, /seccions, /clear, /models, /commands
func processCarlosCodeCommand(send func(string, interface{}), content string, sectionID, requestID string) {
	constants := map[string]func(){
		"/start": func() {
			log.Printf("[cc] /start received for section %s", sectionID)
			// Divide el plan actual en sub-planes y espera aprobacion
			// Esto se hace via POST /subplans desde la vista C1
			send("done", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
				"model": "deepseek-chat",
				"note": "Plan division requested via /start - use /ready to approve",
			})
		},
		"/ready": func() {
			log.Printf("[cc] /ready received for section %s", sectionID)
			// Marcar que el plan fue aprobado y enviar a C1 para dispatch
			send("done", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
				"model": "deepseek-chat",
				"note": "Plan approved via /ready - dispatching to workers",
			})
		},
		"/seccions": func() {
			log.Printf("[cc] /seccions received for section %s", sectionID)
			// Listar secciones disponibles
			send("token", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
				"delta": `{"system":"Secciones disponibles: carga las existentes o selecciona una nueva. Usa /sections para listarlas y enter para seleccionar."}`,
			})
		},
		"/clear": func() {
			log.Printf("[cc] /clear received for section %s", sectionID)
			// Limpiar la sección actual
			sectionsStore.Lock()
			if sec, ok := sectionsStore.byID[sectionID]; ok {
				sec.Messages = sec.Messages[:0]
				sec.Tokens = 0
				sec.UpdatedAt = time.Now().UTC()
				sectionsStore.Unlock()
				send("token", map[string]interface{}{
					"request_id": requestID, "section_id": sectionID,
					"delta": "Sección limpiada correctamente",
				})
			} else {
				sectionsStore.Unlock()
				send("done", map[string]interface{}{
					"request_id": requestID, "section_id": sectionID,
					"error": "Sección no encontrada",
				})
			}
		},
		"/models": func() {
			log.Printf("[cc] /models received for section %s", sectionID)
			// Mostrar catálogo de modelos disponibles
			send("token", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
				"delta": `{"system":"Modelos disponibles: deepseek-chat (1), deepseek-reasoner (2). Escribe el número para seleccionar."}`,
			})
		},
		"/commands": func() {
			log.Printf("[cc] /commands received for section %s", sectionID)
			// Listar comandos disponibles
			send("token", map[string]interface{}{
				"request_id": requestID, "section_id": sectionID,
				"delta": `{"system":"Comandos disponibles: /start (dividir plan), /ready (aprobar y enviar a workers), /seccions (cambiar sección), /clear (limpiar), /models (seleccionar IA), /commands (ver esta lista). Enter = OK, Esc = cancelar."}`,
			})
		},
	}
	
	cmd := strings.TrimSpace(content)
	// Normalizar: quitar posible / al inicio y convertir a minúsculas para comparar
	normalized := strings.TrimPrefix(cmd, "/")
	normalized = strings.ToLower(strings.TrimSpace(normalized))
	
	if handler, ok := constants[normalized]; ok {
		handler()
	} else {
		// Comando no reconocido, enviar ayuda
		send("token", map[string]interface{}{
			"request_id": requestID, "section_id": sectionID,
			"delta": `{"system":"Comando no reconocido. Escribe /commands para ver la lista disponible."}`,
		})
	}
}
