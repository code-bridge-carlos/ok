package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/1000carlospena-prog/carlos-gateway/internal/proxy"
	"github.com/1000carlospena-prog/carlos-gateway/pkg/types"
)

//go:embed runner.js setup.bat index.html wasm.wasm.b64 web/static/proxy-ui.html
var assets embed.FS

// ─── Types ──────────────────────────────────────────────────────────────────────

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

type WSMessage struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Role    string          `json:"role,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Content string          `json:"content,omitempty"`
	Error   string          `json:"error,omitempty"`
	Bearer  string          `json:"bearer,omitempty"`
	Cookies string          `json:"cookies,omitempty"`
}

// ─── Auth Store ─────────────────────────────────────────────────────────────────

var authStore = struct {
	sync.RWMutex
	bearer  string
	cookies string
	ready   bool
}{}

const authStoreFile = "/tmp/auth-store.json"

// authPGPool es el pool a Supabase (DATABASE_URL) para persistir el auth de
// forma durable. Si DATABASE_URL no esta seteado o falla, se degrada a /
//tmp/auth-store.json y a memoria (no crashea).
var authPGPool *pgxpool.Pool

// initAuthPG conecta a Supabase y restaura el auth persistido. Es best-effort:
// los errores solo se loguean. Los parámetros coinciden con control/server/db.py
// (ssl require + statement_cache_size=0 para el pooler pgbouncer de Supabase).
func initAuthPG() {
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		log.Printf("[auth-db] DATABASE_URL no seteado: auth solo en memoria+archivo")
		return
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		log.Printf("[auth-db] parse fallo: %v", err)
		return
	}
	cfg.MaxConns = 2
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["sslmode"] = "require"
	cfg.ConnConfig.RuntimeParams["statement_cache_size"] = "0"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Printf("[auth-db] pool fallo: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		log.Printf("[auth-db] ping fallo: %v", err)
		pool.Close()
		return
	}
	authPGPool = pool
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS gateway_auth (
			id TEXT PRIMARY KEY,
			bearer TEXT NOT NULL,
			cookies TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ DEFAULT now()
		)`); err != nil {
		log.Printf("[auth-db] create table fallo: %v", err)
		return
	}
	log.Printf("[auth-db] Supabase listo (pooler)")
	restoreAuthFromPG(ctx)
}

// initGatewayHistory creates the gateway_history table if it doesn't exist.
// Runs on the same authPGPool established by initAuthPG (Supabase pooler).
func initGatewayHistory() {
	if authPGPool == nil {
		log.Printf("[gw-history] sin pool (DATABASE_URL no seteado): tabla no creada")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := authPGPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS gateway_history (
			id SERIAL PRIMARY KEY,
			request_path TEXT,
			prompt TEXT,
			response_snippet TEXT,
			status INT,
			latency_ms INT,
			created_at TIMESTAMPTZ DEFAULT now()
		)`)
	if err != nil {
		log.Printf("[gw-history] create table fallo: %v", err)
		return
	}
	_, err = authPGPool.Exec(ctx,
		`CREATE INDEX IF NOT EXISTS idx_gateway_history_created_at ON gateway_history(created_at)`)
	if err != nil {
		log.Printf("[gw-history] create index fallo: %v", err)
		return
	}
	log.Printf("[gw-history] tabla gateway_history lista")
}

// saveGatewayHistory writes a gateway_history row fire-and-forget with a 5 s
// context timeout. Failures are logged only — the client response is never blocked.
func saveGatewayHistory(requestPath, prompt, responseSnippet string, status, latencyMs int) {
	if authPGPool == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := authPGPool.Exec(ctx,
			`INSERT INTO gateway_history (request_path, prompt, response_snippet, status, latency_ms)
			 VALUES ($1, $2, $3, $4, $5)`,
			requestPath, prompt, responseSnippet, status, latencyMs)
		if err != nil {
			log.Printf("[gw-history] save fallo: %v", err)
		}
	}()
}

// restoreAuthFromPG carga el auth durado en Supabase si existe.
func restoreAuthFromPG(ctx context.Context) {
	var bearer, cookies string
	err := authPGPool.QueryRow(ctx,
		`SELECT bearer, cookies FROM gateway_auth WHERE id='default'`).Scan(&bearer, &cookies)
	if err != nil {
		log.Printf("[auth-db] sin auth persistido: %v", err)
		return
	}
	if bearer == "" {
		return
	}
	authStore.Lock()
	defer authStore.Unlock()
	authStore.bearer = bearer
	authStore.cookies = cookies
	authStore.ready = true
	log.Printf("Auth restored from Supabase (bearer_len=%d)", len(bearer))
}

// persistAuthToPG escribe el auth actual a Supabase. Best-effort.
func persistAuthToPG() {
	if authPGPool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := authPGPool.Exec(ctx,
		`INSERT INTO gateway_auth (id,bearer,cookies,updated_at)
		 VALUES ('default',$1,$2,now())
		 ON CONFLICT (id) DO UPDATE SET bearer=$1, cookies=$2, updated_at=now()`,
		authStore.bearer, authStore.cookies)
	if err != nil {
		log.Printf("[auth-db] persist fallo: %v", err)
		return
	}
	log.Printf("[auth-db] auth persistido en Supabase")
}

func storeAuth(bearer, cookies string) {
	authStore.Lock()
	defer authStore.Unlock()
	authStore.bearer = bearer
	authStore.cookies = cookies
	authStore.ready = true
	log.Printf("Auth stored: bearer_len=%d cookies_len=%d", len(bearer), len(cookies))
	persistAuthLocked()
	persistAuthToPG()
}

// persistAuthLocked writes the current auth to an ephemeral file so a container
// restart (same deploy) does not lose the session. Run with authStore held.
func persistAuthLocked() {
	data, err := json.Marshal(struct {
		Bearer  string `json:"bearer"`
		Cookies string `json:"cookies"`
	}{authStore.bearer, authStore.cookies})
	if err != nil {
		return
	}
	if err := os.WriteFile(authStoreFile, data, 0o600); err != nil {
		log.Printf("WARN: could not persist auth file: %v", err)
		return
	}
	log.Printf("Auth persisted to %s", authStoreFile)
}

// restoreAuthFromFile loads auth persisted by a previous process lifetime.
func restoreAuthFromFile() {
	data, err := os.ReadFile(authStoreFile)
	if err != nil {
		return // no persisted session yet
	}
	var v struct {
		Bearer  string `json:"bearer"`
		Cookies string `json:"cookies"`
	}
	if err := json.Unmarshal(data, &v); err != nil || v.Bearer == "" {
		log.Printf("WARN: ignoring invalid persisted auth file")
		return
	}
	authStore.Lock()
	defer authStore.Unlock()
	authStore.bearer = v.Bearer
	authStore.cookies = v.Cookies
	authStore.ready = true
	log.Printf("Auth restored from %s (bearer_len=%d)", authStoreFile, len(v.Bearer))
}

func getAuth() (string, string, bool) {
	authStore.RLock()
	defer authStore.RUnlock()
	return authStore.bearer, authStore.cookies, authStore.ready
}

// ─── Session Store (per-connection) ────────────────────────────────────────────
type DeepSeekSession struct {
	chatSessionID   string
	parentMessageID string
	fileIDs         []string
}

// ─── Global Proxy Manager ──────────────────────────────────────────────────────
var proxyManager *proxy.Manager

// ─── WASM PoW Solver ───────────────────────────────────────────────────────────
var wasmBytes []byte

func init() {
	data, err := assets.ReadFile("wasm.wasm.b64")
	if err != nil {
		log.Printf("WASM base64 not found: %v", err)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		log.Printf("WASM base64 decode error: %v", err)
		return
	}
	wasmBytes = decoded
	log.Printf("WASM loaded: %d bytes", len(wasmBytes))
}

func solvePowSHA256(challenge map[string]interface{}) (interface{}, error) {
	algorithm, _ := challenge["algorithm"].(string)
	target, _ := challenge["challenge"].(string)
	salt, _ := challenge["salt"].(string)
	var difficulty int64
	switch d := challenge["difficulty"].(type) {
	case float64:
		difficulty = int64(d)
	case json.Number:
		difficulty, _ = d.Int64()
	}
	expireAt, _ := challenge["expire_at"].(string)
	_ = expireAt

	if algorithm != "sha256" {
		return nil, fmt.Errorf("unsupported algorithm: %s", algorithm)
	}

	start := time.Now()
	targetDiff := int64(difficulty)
	if difficulty > 1000 {
		targetDiff = int64(math.Floor(math.Log2(float64(difficulty))))
	}

	for nonce := 0; nonce <= 10000000; nonce++ {
		input := salt + target + strconv.Itoa(nonce)
		hash := sha256.Sum256([]byte(input))
		hashHex := fmt.Sprintf("%x", hash)

		zeroBits := int64(0)
		for _, ch := range hashHex {
			val := 0
			if ch >= '0' && ch <= '9' {
				val = int(ch - '0')
			} else if ch >= 'a' && ch <= 'f' {
				val = int(ch-'a') + 10
			}
			if val == 0 {
				zeroBits += 4
			} else {
				for i := 3; i >= 0; i-- {
					if val&(1<<uint(i)) != 0 {
						break
					}
					zeroBits++
				}
				break
			}
		}

		if zeroBits >= targetDiff {
			powData := make(map[string]interface{})
			for k, v := range challenge {
				powData[k] = v
			}
			powData["answer"] = nonce
			powData["target_path"] = "/api/v0/chat/completion"
			powJSON, _ := json.Marshal(powData)
			powB64 := base64.StdEncoding.EncodeToString(powJSON)
			log.Printf("[PoW] SHA256 solved: %vms nonce=%d zeros=%d need=%d", time.Since(start), nonce, zeroBits, targetDiff)
			return powB64, nil
		}
	}
	return nil, fmt.Errorf("SHA256 timeout")
}

func solvePowWASM(challenge map[string]interface{}) (interface{}, error) {
	if wasmBytes == nil {
		return nil, fmt.Errorf("WASM not available")
	}

	salt, _ := challenge["salt"].(string)
	target, _ := challenge["challenge"].(string)
	var difficulty int64
	switch d := challenge["difficulty"].(type) {
	case float64:
		difficulty = int64(d)
	case json.Number:
		difficulty, _ = d.Int64()
	}
	expireAt := int64(0)
	switch e := challenge["expire_at"].(type) {
	case float64:
		expireAt = int64(e)
	case json.Number:
		expireAt, _ = e.Int64()
	}
	prefix := fmt.Sprintf("%s_%d_", salt, expireAt)

	start := time.Now()

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	wasi_snapshot_preview1.Instantiate(ctx, r)

	mod, err := r.Instantiate(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("wasm instantiate: %w", err)
	}

	allocFn := mod.ExportedFunction("__wbindgen_export_0")
	stackFn := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	solveFn := mod.ExportedFunction("wasm_solve")

	if allocFn == nil || stackFn == nil || solveFn == nil {
		return nil, fmt.Errorf("wasm: missing exports (alloc=%v stack=%v solve=%v)", allocFn != nil, stackFn != nil, solveFn != nil)
	}

	memory := mod.Memory()
	if memory == nil {
		return nil, fmt.Errorf("wasm: no memory exported")
	}

	challengeBytes := []byte(target)
	prefixBytes := []byte(prefix)

	challengePtr, err := allocFn.Call(ctx, uint64(len(challengeBytes)), 1)
	if err != nil {
		return nil, fmt.Errorf("wasm alloc challenge: %w", err)
	}
	if !memory.Write(uint32(challengePtr[0]), challengeBytes) {
		return nil, fmt.Errorf("wasm: failed to write challenge")
	}

	prefixPtr, err := allocFn.Call(ctx, uint64(len(prefixBytes)), 1)
	if err != nil {
		return nil, fmt.Errorf("wasm alloc prefix: %w", err)
	}
	if !memory.Write(uint32(prefixPtr[0]), prefixBytes) {
		return nil, fmt.Errorf("wasm: failed to write prefix")
	}

	retptrResults, err := stackFn.Call(ctx, ^uint64(15))
	if err != nil {
		return nil, fmt.Errorf("wasm stack: %w", err)
	}
	retptr := uint32(retptrResults[0])

	_, err = solveFn.Call(ctx, uint64(retptr), challengePtr[0], uint64(len(challengeBytes)), prefixPtr[0], uint64(len(prefixBytes)), uint64(math.Float64bits(float64(difficulty))))
	if err != nil {
		return nil, fmt.Errorf("wasm_solve: %w", err)
	}

	statusBytes, ok := memory.Read(retptr, 4)
	if !ok {
		return nil, fmt.Errorf("wasm: failed to read status")
	}
	status := int32(statusBytes[0]) | int32(statusBytes[1])<<8 | int32(statusBytes[2])<<16 | int32(statusBytes[3])<<24

	answerBytes, ok := memory.Read(retptr+8, 8)
	if !ok {
		return nil, fmt.Errorf("wasm: failed to read answer")
	}
	answer := math.Float64frombits(
		uint64(answerBytes[0]) | uint64(answerBytes[1])<<8 |
			uint64(answerBytes[2])<<16 | uint64(answerBytes[3])<<24 |
			uint64(answerBytes[4])<<32 | uint64(answerBytes[5])<<40 |
			uint64(answerBytes[6])<<48 | uint64(answerBytes[7])<<56)

	if status == 0 {
		return nil, fmt.Errorf("DeepSeekHashV1 failed: status=0 (challenge=%s, prefix=%s, diff=%d)", target, prefix, difficulty)
	}

	log.Printf("[PoW] WASM solved: %vms answer=%.0f", time.Since(start), answer)

	powData := make(map[string]interface{})
	for k, v := range challenge {
		powData[k] = v
	}
	powData["answer"] = answer
	powData["target_path"] = "/api/v0/chat/completion"
	powJSON, _ := json.Marshal(powData)
	powB64 := base64.StdEncoding.EncodeToString(powJSON)
	return powB64, nil
}

func solvePow(challenge map[string]interface{}) (interface{}, error) {
	algorithm, _ := challenge["algorithm"].(string)
	if algorithm == "sha256" {
		return solvePowSHA256(challenge)
	}
	if algorithm == "DeepSeekHashV1" {
		return solvePowWASM(challenge)
	}
	return nil, fmt.Errorf("unknown algorithm: %s", algorithm)
}

// ─── DeepSeek HTTP Client ──────────────────────────────────────────────────────
var deepseekUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func deepseekPOST(path string, body interface{}, bearer, cookies, powResponse string) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req, err := http.NewRequest("POST", "https://chat.deepseek.com"+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}

	for k, v := range deepseekHeaders(bearer) {
		req.Header.Set(k, v)
	}
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}
	if powResponse != "" {
		req.Header.Set("x-ds-pow-response", powResponse)
	}

	// Go direct — PoW and session don't need proxy (no rate limit per IP)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

func deepseekHeaders(bearer string) map[string]string {
	h := map[string]string{
		"User-Agent":                deepseekUA,
		"Content-Type":             "application/json",
		"Accept":                   "*/*",
		"Referer":                  "https://chat.deepseek.com/",
		"Origin":                   "https://chat.deepseek.com",
		"x-client-platform":        "web",
		"x-client-version":         "1.7.0",
		"x-app-version":            "20241129.1",
		"x-client-locale":          "zh_CN",
		"x-client-timezone-offset": "28800",
	}
	if bearer != "" {
		h["Authorization"] = "Bearer " + bearer
	}
	return h
}

func createChatSession(bearer, cookies string) (string, error) {
	_, body, err := deepseekPOST("/api/v0/chat_session/create", map[string]string{}, bearer, cookies, "")
	if err != nil {
		return "", fmt.Errorf("session create failed: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("session parse failed: %w", err)
	}

	data, _ := result["data"].(map[string]interface{})
	bizData, _ := data["biz_data"].(map[string]interface{})
	sessionID, _ := bizData["id"].(string)
	if sessionID == "" {
		sessionID, _ = bizData["chat_session_id"].(string)
	}
	if sessionID == "" {
		return "", fmt.Errorf("session ID missing in response: %s", string(body))
	}

	return sessionID, nil
}

func deepseekChatCompletion(prompt, model, bearer, cookies string, temperature float64) (string, error) {
	sessionID, err := createChatSession(bearer, cookies)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	log.Printf("Session created: %s", sessionID)

	powBody, _ := json.Marshal(map[string]string{
		"target_path": "/api/v0/chat/completion",
	})
	powReq, _ := http.NewRequest("POST", "https://chat.deepseek.com/api/v0/chat/create_pow_challenge", bytes.NewReader(powBody))
	for k, v := range deepseekHeaders(bearer) {
		powReq.Header.Set(k, v)
	}
	if cookies != "" {
		powReq.Header.Set("Cookie", cookies)
	}

	powClient := &http.Client{Timeout: 30 * time.Second}
	// PoW goes direct — no proxy needed
	powResp, err := powClient.Do(powReq)
	if err != nil {
		return "", fmt.Errorf("PoW request failed: %w", err)
	}
	defer powResp.Body.Close()
	powBodyBytes, _ := io.ReadAll(powResp.Body)

	var powResult map[string]interface{}
	if err := json.Unmarshal(powBodyBytes, &powResult); err != nil {
		return "", fmt.Errorf("PoW parse failed: %w", err)
	}

	var challenge map[string]interface{}
	if data, ok := powResult["data"].(map[string]interface{}); ok {
		if bizData, ok := data["biz_data"].(map[string]interface{}); ok {
			challenge, _ = bizData["challenge"].(map[string]interface{})
		}
		if challenge == nil {
			challenge, _ = data["challenge"].(map[string]interface{})
		}
	}
	if challenge == nil {
		challenge, _ = powResult["challenge"].(map[string]interface{})
	}
	if challenge == nil {
		return "", fmt.Errorf("PoW challenge missing: %s", string(powBodyBytes))
	}

	powSolution, err := solvePow(challenge)
	if err != nil {
		return "", fmt.Errorf("PoW solve: %w", err)
	}
	powSolutionStr, _ := powSolution.(string)

	thinkingEnabled := true
	if model == "deepseek-chat" {
		thinkingEnabled = false
	}

	chatBody := map[string]interface{}{
		"chat_session_id":   sessionID,
		"parent_message_id": nil,
		"prompt":            prompt,
		"ref_file_ids":      []string{},
		"thinking_enabled":  thinkingEnabled,
		"search_enabled":    true,
		"preempt":           false,
		"temperature":       temperature,
	}

	chatJSON, _ := json.Marshal(chatBody)
	chatReq, _ := http.NewRequest("POST", "https://chat.deepseek.com/api/v0/chat/completion", bytes.NewReader(chatJSON))
	for k, v := range deepseekHeaders(bearer) {
		chatReq.Header.Set(k, v)
	}
	if cookies != "" {
		chatReq.Header.Set("Cookie", cookies)
	}
	chatReq.Header.Set("x-ds-pow-response", powSolutionStr)

	// Go direct — free HTTP proxies can't tunnel SSE streaming from DeepSeek
	chatClient := &http.Client{Timeout: 120 * time.Second}
	chatResp, err := chatClient.Do(chatReq)
	if err != nil {
		return "", fmt.Errorf("chat request failed: %w", err)
	}
	defer chatResp.Body.Close()

	return parseSSEResponse(chatResp.Body, prompt)
}

// ─── SSE Parser ────────────────────────────────────────────────────────────────
func parseSSEResponse(reader io.Reader, prompt string) (string, error) {
	var fullText strings.Builder
	lineCount := 0
	dataCount := 0

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		lineCount++
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		dataStr := strings.TrimPrefix(line, "data: ")
		dataStr = strings.TrimSpace(dataStr)
		dataCount++
		if dataStr == "[DONE]" || dataStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			log.Printf("SSE parse error: %v data=%s", err, dataStr[:min(100, len(dataStr))])
			continue
		}

		pStr, _ := data["p"].(string)
		vVal := data["v"]

		if strings.Contains(pStr, "reasoning") || data["type"] == "thinking" {
			continue
		}

		if vStr, ok := vVal.(string); ok && vStr != "" {
			if !strings.Contains(pStr, "reasoning") {
				fullText.WriteString(vStr)
			}
			continue
		}

		if content, ok := data["content"].(string); ok && content != "" {
			if data["type"] != "thinking" && data["type"] != "reasoning" {
				fullText.WriteString(content)
			}
			continue
		}

		if choices, ok := data["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok {
						fullText.WriteString(content)
					}
				}
			}
		}

		if resp, ok := data["v"].(map[string]interface{}); ok {
			if frags, ok := resp["response"].(map[string]interface{}); ok {
				if fragArr, ok := frags["fragments"].([]interface{}); ok {
					for _, f := range fragArr {
						if frag, ok := f.(map[string]interface{}); ok {
							if content, ok := frag["content"].(string); ok {
								if fragType, _ := frag["type"].(string); fragType != "THINKING" && fragType != "reasoning" {
									fullText.WriteString(content)
								}
							}
						}
					}
				}
			}
		}
	}

	result := fullText.String()
	result = strings.ReplaceAll(result, "FINISHED", "")
	if prompt != "" {
		// Strip prompt from END
		result = strings.TrimSuffix(result, prompt)
		promptNoPunct := strings.TrimRight(prompt, ":;,.")
		if promptNoPunct != prompt {
			result = strings.TrimSuffix(result, promptNoPunct)
		}
		result = strings.TrimSpace(result)

		// Strip prompt from BEGINNING (model sometimes echoes it)
		for _, pfx := range []string{prompt, promptNoPunct} {
			if pfx != "" && strings.HasPrefix(result, pfx) {
				result = strings.TrimSpace(result[len(pfx):])
			}
		}

		// Strip last word of prompt from BEGINNING (model echoes final word)
		promptWords := strings.Fields(prompt)
		if len(promptWords) > 0 {
			lastWord := promptWords[len(promptWords)-1]
			lastWordNoPunct := strings.TrimRight(lastWord, ":;,.")
			for _, pfx := range []string{lastWord, lastWordNoPunct} {
				if pfx != "" && strings.HasPrefix(result, pfx) {
					candidate := strings.TrimSpace(result[len(pfx):])
					if candidate != "" {
						result = candidate
					}
				}
			}
		}

		// Case-insensitive suffix strip
		for _, suffix := range []string{prompt, promptNoPunct} {
			if suffix != "" {
				lower := strings.ToLower(result)
				lowerSuffix := strings.ToLower(suffix)
				if strings.HasSuffix(lower, lowerSuffix) {
					result = strings.TrimSpace(result[:len(result)-len(suffix)])
				}
			}
		}
	}
	log.Printf("SSE parse done: lines=%d data_events=%d result_len=%d", lineCount, dataCount, len(result))
	return strings.TrimSpace(result), nil
}

// ─── C2 WebSocket Hub ──────────────────────────────────────────────────────────
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func c2WSHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	defer conn.Close()

	conn.SetReadLimit(65536)

	_, data, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "register" {
		conn.Close()
		return
	}

	execID := "runner-" + generateUUID()[:8]
	log.Printf("C2: Registered %s as %s", msg.Role, execID)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Runner %s disconnected", execID)
			return
		}
		var m WSMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case "auth":
			if m.Bearer != "" {
				storeAuth(m.Bearer, m.Cookies)
			}
		case "pong":
		}
	}
}

// ─── HTTP Handlers ──────────────────────────────────────────────────────────────
func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bearer, cookies, hasAuth := getAuth()
	if !hasAuth {
		http.Error(w, `{"error":"No auth token. Connect runner first."}`, http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Build prompt — with tools if provided
	var prompt string
	if len(req.Tools) > 0 {
		prompt = BuildPromptWithTools(req.Messages, req.Tools)
	} else {
		for _, m := range req.Messages {
			if m.Role == "user" {
				prompt = m.Content
			}
		}
		if prompt == "" && len(req.Messages) > 0 {
			prompt = req.Messages[len(req.Messages)-1].Content
		}
	}

	log.Printf("Chat request: model=%s prompt_len=%d tools=%d", req.Model, len(prompt), len(req.Tools))

	// Forward temperature when set (>0); default 0.7 preserves prior behavior.
	temperature := req.Temperature
	if temperature <= 0 {
		temperature = 0.7
	}

	result, err := deepseekChatCompletion(prompt, req.Model, bearer, cookies, temperature)
	if err != nil {
		log.Printf("DeepSeek error: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}

	// Detect tool call in response
	toolCall := DetectToolCall(result)

	// Build OpenAI-compatible response
	openAIResp := map[string]interface{}{
		"id":      "chatcmpl-" + generateUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"usage":   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}

	if toolCall != nil {
		// Execute the tool
		toolResult := ExecuteToolCall(toolCall)

		// Build response with tool result
		toolResultMsg := Message{Role: "tool", Content: toolResult}
		messagesWithResult := append(req.Messages, Message{Role: "assistant", Content: ""}, toolResultMsg)
		followUpPrompt := BuildPromptWithTools(messagesWithResult, req.Tools)

		followUpResult, err := deepseekChatCompletion(followUpPrompt, req.Model, bearer, cookies, temperature)
		if err != nil {
			followUpResult = fmt.Sprintf("Error executing tool: %v", err)
		}

		openAIResp["choices"] = []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": followUpResult,
				},
				"finish_reason": "stop",
			},
		}
	} else {
		openAIResp["choices"] = []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": result,
				},
				"finish_reason": "stop",
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(openAIResp)

	// Fire-and-forget: persist gateway history (never block the response).
	responseSnippet := result
	if len(responseSnippet) > 500 {
		responseSnippet = responseSnippet[:500]
	}
	latencyMs := int(time.Since(startTime).Milliseconds())
	saveGatewayHistory(r.URL.Path, prompt, responseSnippet, http.StatusOK, latencyMs)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	_, _, hasAuth := getAuth()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"auth":      hasAuth,
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ram_mb":     float64(m.Alloc) / 1024 / 1024,
		"ram_sys":    float64(m.Sys) / 1024 / 1024,
		"goroutines": runtime.NumGoroutine(),
		"uptime_sec": time.Since(startTime).Seconds(),
	})
}

var startTime = time.Now()

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	models := []map[string]interface{}{
		{"id": "deepseek-chat", "object": "model", "created": time.Now().Unix(), "owned_by": "deepseek"},
		{"id": "deepseek-reasoner", "object": "model", "created": time.Now().Unix(), "owned_by": "deepseek"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

func installJSHandler(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("runner.js")
	if err != nil {
		http.Error(w, "Runner JS not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Content-Disposition", "attachment; filename=runner.js")
	w.Write(data)
}

func setupBatHandler(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("setup.bat")
	if err != nil {
		http.Error(w, "Setup bat not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=setup.bat")
	w.Write(data)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Index not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ─── Proxy UI Handlers ─────────────────────────────────────────────────────────
func proxyStatusHandler(w http.ResponseWriter, r *http.Request) {
	if proxyManager == nil {
		http.Error(w, "Proxy manager not initialized", http.StatusServiceUnavailable)
		return
	}
	proxyManager.StatusHandler(w, r)
}

func proxyRefreshHandler(w http.ResponseWriter, r *http.Request) {
	if proxyManager == nil {
		http.Error(w, "Proxy manager not initialized", http.StatusServiceUnavailable)
		return
	}
	proxyManager.RefreshHandler(w, r)
}

func proxyChangeHandler(w http.ResponseWriter, r *http.Request) {
	if proxyManager == nil {
		http.Error(w, "Proxy manager not initialized", http.StatusServiceUnavailable)
		return
	}
	proxyManager.ChangeHandler(w, r)
}

func proxyListHandler(w http.ResponseWriter, r *http.Request) {
	if proxyManager == nil {
		http.Error(w, "Proxy manager not initialized", http.StatusServiceUnavailable)
		return
	}
	proxyManager.ListHandler(w, r)
}

func proxyUIHandler(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("web/static/proxy-ui.html")
	if err != nil {
		http.Error(w, "Proxy UI not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func proxyLogsHandler(w http.ResponseWriter, r *http.Request) {
	if proxyManager == nil {
		http.Error(w, "Proxy manager not initialized", http.StatusServiceUnavailable)
		return
	}
	proxyManager.LogsHandler(w, r)
}

func proxyClearCacheHandler(w http.ResponseWriter, r *http.Request) {
	if proxyManager == nil {
		http.Error(w, "Proxy manager not initialized", http.StatusServiceUnavailable)
		return
	}
	proxyManager.ClearCacheHandler(w, r)
}

// ─── Main ────────────────────────────────────────────────────────────────────────
func generateUUID() string {
	u := make([]byte, 16)
	rand.Read(u)
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// ─── HTTP Auth Endpoint ──────────────────────────────────────────────────────────
// apiAuthHandler accepts fresh DeepSeek credentials without needing the WS
// runner: POST /api/auth {"bearer":"...","cookies":"..."}.
func apiAuthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var m struct {
		Bearer  string `json:"bearer"`
		Cookies string `json:"cookies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if m.Bearer == "" {
		http.Error(w, "bearer is required (cookies alone are ignored)", http.StatusBadRequest)
		return
	}
	storeAuth(m.Bearer, m.Cookies)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"bearer_len":%d,"cookies_len":%d}`, len(m.Bearer), len(m.Cookies))
}

func main() {
	// Restore persisted auth (survives container restarts of the same deploy).
	restoreAuthFromFile()
	// Connect to Supabase (DATABASE_URL) and restore durable auth if present.
	initAuthPG()
	// Create gateway_history table if it doesn't exist (same pool).
	initGatewayHistory()

	// Initialize Proxy Manager
	var err error
	proxyManager, err = proxy.NewManager(&types.ProxyManagerConfig{
		ProxyListURL:     "https://raw.githubusercontent.com/proxmint/free-proxy-list/main/proxies/all.txt",
		RefreshInterval:  time.Hour,
		MaxProxiesPerHour: 100,
		TestTimeout:      10 * time.Second,
		RequestTimeout:   10 * time.Second,
		MaxFailures:      5,
		FailoverRetries:  100,
		FailoverTimeout:  10 * time.Second,
	})
	if err != nil {
		log.Fatalf("Failed to create proxy manager: %v", err)
	}

	if err := proxyManager.Start(); err != nil {
		log.Fatalf("Failed to start proxy manager: %v", err)
	}
	defer proxyManager.Stop()

	// CORS middleware wrapper
	cors := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}

	// HTTP Handlers
	http.HandleFunc("/", cors(indexHandler))
	http.HandleFunc("/c2/ws", cors(c2WSHandler))
	http.HandleFunc("/v1/chat/completions", cors(chatCompletionsHandler))
	http.HandleFunc("/v1/models", cors(modelsHandler))
	http.HandleFunc("/health", cors(healthHandler))
	http.HandleFunc("/metrics", cors(metricsHandler))
	http.HandleFunc("/api/auth", cors(apiAuthHandler))
	http.HandleFunc("/install.js", cors(installJSHandler))
	http.HandleFunc("/setup.bat", cors(setupBatHandler))

	// Proxy Management Endpoints
	http.HandleFunc("/proxy/status", cors(proxyStatusHandler))
	http.HandleFunc("/proxy/refresh", cors(proxyRefreshHandler))
	http.HandleFunc("/proxy/change", cors(proxyChangeHandler))
	http.HandleFunc("/proxy/list", cors(proxyListHandler))
	http.HandleFunc("/proxy/ui", cors(proxyUIHandler))
	http.HandleFunc("/proxy/logs", cors(proxyLogsHandler))
	http.HandleFunc("/proxy/clear-cache", cors(proxyClearCacheHandler))

	log.Println("Gateway listening on :8080 — proxy via proxmint/free-proxy-list")
	log.Fatal(http.ListenAndServe(":8080", nil))
}