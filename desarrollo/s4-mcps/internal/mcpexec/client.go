// Package mcpexec ejecuta herramientas de servidores MCP (Model Context Protocol)
// por stdio bajo demanda: cada MCP se lanza como proceso hijo solo cuando se
// usa por primera vez y se mata cuando no quedan peticiones pendientes tras un
// periodo de gracia (spec S4, puntos 4, 8, 10, 11, 30).
package mcpexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"s4-mcps/internal/mcp"
)

// Tool describe una herramienta expuesta por un MCP.
type Tool struct {
	Name        string         `json:"nombre"`
	Description string         `json:"descripcion,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ContentItem es una pieza de contenido del resultado de una llamada MCP.
type ContentItem struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Structured map[string]any `json:"structuredContent,omitempty"`
}

// CallResult es el resultado de tools/call.
type CallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError"`
}

// Executor gestiona los procesos MCP bajo demanda.
type Executor struct {
	reg     *mcp.Registry
	timeout time.Duration // tope de una ejecución
	grace   time.Duration // espera antes de matar un MCP sin peticiones
	baseEnv []string      // env adicional para todos los procesos MCP

	mu       sync.Mutex
	sessions map[string]*session
	spawnMu  map[string]*sync.Mutex // serializa el primer lanzamiento por MCP
}

// Opt configura el Executor.
type Opt func(*Executor)

// WithTimeout fija el tope de ejecución por herramienta.
func WithTimeout(d time.Duration) Opt { return func(e *Executor) { e.timeout = d } }

// WithGrace fija la espera antes de matar un MCP ocioso.
func WithGrace(d time.Duration) Opt { return func(e *Executor) { e.grace = d } }

// New construye el Executor para el registro de MCPs dado.
func New(reg *mcp.Registry, baseEnv []string, opts ...Opt) *Executor {
	e := &Executor{
		reg:      reg,
		timeout:  90 * time.Second,
		grace:    5 * time.Second,
		baseEnv:  baseEnv,
		sessions: map[string]*session{},
		spawnMu:  map[string]*sync.Mutex{},
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// ListTools devuelve las herramientas disponibles de un MCP (punto 8: verifica
// que el MCP solicitado esté disponible; la lista se pide a la sesión viva).
func (e *Executor) ListTools(ctx context.Context, mcpName string) ([]Tool, string, error) {
	def, ok := e.reg.Get(mcpName)
	if !ok {
		return nil, "", fmt.Errorf("MCP '%s' no está configurado", mcpName)
	}
	s, err := e.acquire(ctx, def)
	if err != nil {
		return nil, def.Comando, err
	}
	defer e.release(def, s)

	tools, err := s.listTools(ctx)
	if err != nil {
		e.kill(def, s)
		return nil, def.Comando, err
	}
	return tools, def.Comando, nil
}

// Execute llama a una herramienta de un MCP (spec punto 8). El proceso se
// reutiliza entre peticiones concurrentes y se mata en el idle tras la gracia.
func (e *Executor) Execute(ctx context.Context, mcpName, tool string, args map[string]any) (*CallResult, error) {
	def, ok := e.reg.Get(mcpName)
	if !ok {
		return nil, fmt.Errorf("MCP '%s' no está configurado", mcpName)
	}
	if strings.TrimSpace(tool) == "" {
		return nil, errors.New("herramienta vacía")
	}

	// Tope de ejecución configurado (MCP_TIMEOUT, default 90s).
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	s, err := e.acquire(ctx, def)
	if err != nil {
		return nil, err
	}
	defer e.release(def, s)

	res, err := s.callTool(ctx, tool, args)
	if err != nil {
		// Punto 11: si el MCP falla, se detiene el proceso y se devuelve error.
		e.kill(def, s)
		return nil, err
	}
	return res, nil
}

// ─── Sesiones ────────────────────────────────────────────────────────────────

// session envuelve un proceso MCP vivo más el despacho de JSON-RPC.
type session struct {
	def       mcp.Def
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	msgs      chan json.RawMessage
	stderr    *bytes.Buffer
	msgID     int
	isClosed  bool
	idleTimer *time.Timer
}

// acquire devuelve la sesión activa del MCP o lanza una nueva (punto 8).
// El primer lanzamiento por MCP se serializa para evitar procesos duplicados.
func (e *Executor) acquire(ctx context.Context, def mcp.Def) (*session, error) {
	e.mu.Lock()
	if s, ok := e.sessions[def.Nombre]; ok {
		if s.idleTimer != nil {
			s.idleTimer.Stop() // se cancela el kill programado: sigue en uso
		}
		e.mu.Unlock()
		return s, nil
	}
	sm := e.spawnMu[def.Nombre]
	if sm == nil {
		sm = &sync.Mutex{}
		e.spawnMu[def.Nombre] = sm
	}
	e.mu.Unlock()

	sm.Lock()
	defer sm.Unlock()

	// Re-chequeo tras esperar el candado de spawn.
	e.mu.Lock()
	if s, ok := e.sessions[def.Nombre]; ok {
		if s.idleTimer != nil {
			s.idleTimer.Stop()
		}
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()

	s, err := e.spawn(def)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.sessions[def.Nombre] = s
	e.mu.Unlock()
	return s, nil
}

// spawn lanza el proceso MCP y hace el handshake initialize (punto 8).
func (e *Executor) spawn(def mcp.Def) (*session, error) {
	cmd := exec.Command(def.Comando, def.Args...)
	cmd.Env = append(os.Environ(), e.baseEnv...)
	cmd.Env = append(cmd.Env, def.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creando stdin de %s: %w", def.Nombre, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("creando stdout de %s: %w", def.Nombre, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("lanzando MCP '%s': %w", def.Nombre, err)
	}

	s := &session{
		def:    def,
		cmd:    cmd,
		stdin:  stdin,
		msgs:   make(chan json.RawMessage, 32),
		stderr: &stderr,
		msgID:  1,
	}
	go s.readLoop(stdout)

	// Handshake con tope corto independiente del timeout de ejecución.
	hctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.initialize(hctx); err != nil {
		s.kill()
		return nil, fmt.Errorf("handshake con MCP '%s' falló: %w", def.Nombre, err)
	}
	return s, nil
}

// readLoop reenvía las líneas JSON del stdout del proceso a todos los lectores.
func (s *session) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20) // hasta 8 MiB por mensaje
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.msgs <- json.RawMessage(append([]byte(nil), line...))
	}
	close(s.msgs)
}

// nextID genera ids correlativos para JSON-RPC.
func (s *session) nextID() int {
	id := s.msgID
	s.msgID++
	return id
}

// initialize hace el handshake MCP: initialize + notificación initialized.
func (s *session) initialize(ctx context.Context) error {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "s4-mcps", "version": "1.0"},
		},
	}
	if err := s.writeJSON(req); err != nil {
		return err
	}
	if _, err := s.waitID(ctx, 0); err != nil {
		return err
	}
	// Notificación de inicialización completa.
	notif := map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}
	return s.writeJSON(notif)
}

func (s *session) listTools(ctx context.Context) ([]Tool, error) {
	id := s.nextID()
	if err := s.writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list"}); err != nil {
		return nil, err
	}
	raw, err := s.waitID(ctx, id)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("respuesta tools/list inválida: %w", err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	out := make([]Tool, 0, len(resp.Result.Tools))
	for _, t := range resp.Result.Tools {
		out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, nil
}

func (s *session) callTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	id := s.nextID()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
	if err := s.writeJSON(req); err != nil {
		return nil, err
	}
	raw, err := s.waitID(ctx, id)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result *struct {
			Content []ContentItem `json:"content"`
			IsError bool          `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("respuesta tools/call inválida: %w", err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if resp.Result == nil {
		return nil, errors.New("tools/call devolvió resultado vacío")
	}
	if resp.Result.Content == nil {
		resp.Result.Content = []ContentItem{}
	}
	return &CallResult{Content: resp.Result.Content, IsError: resp.Result.IsError}, nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// writeJSON serializa y escribe una petición al proceso.
func (s *session) writeJSON(v any) error {
	if s.isClosed {
		return errors.New("proceso MCP cerrado")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		s.kill()
		return fmt.Errorf("escribiendo al MCP: %w", err)
	}
	return nil
}

// waitID espera un mensaje con el id pedido, ignorando notificaciones.
func (s *session) waitID(ctx context.Context, id int) (json.RawMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout esperando respuesta del MCP: %w", ctx.Err())
		case raw, ok := <-s.msgs:
			if !ok {
				return nil, fmt.Errorf("el proceso MCP terminó (stderr: %s)", s.stderrLog())
			}
			var head struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &head) != nil {
				continue
			}
			if head.Method != "" {
				continue // notificación del servidor (logging etc.)
			}
			var gotID int
			if json.Unmarshal(head.ID, &gotID) != nil {
				continue
			}
			if gotID == id {
				return raw, nil
			}
		}
	}
}

func (s *session) stderrLog() string {
	if s.stderr == nil {
		return ""
	}
	return strings.TrimSpace(s.stderr.String())
}

// ─── Ciclo de vida ───────────────────────────────────────────────────────────

// release marca el fin de una petición y programa el kill tras la gracia:
// el proceso queda vivo solo si llega otra petición y cancela el temporizador.
func (e *Executor) release(def mcp.Def, s *session) {
	e.scheduleIdleKill(def)
}

// scheduleIdleKill lanza un temporizador que mata el proceso si no vuelve a
// usarse dentro de la gracia. El acquire anterior cancela el temporizador.
func (e *Executor) scheduleIdleKill(def mcp.Def) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[def.Nombre]
	if s == nil {
		return
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(e.grace, func() {
		e.mu.Lock()
		cur := e.sessions[def.Nombre]
		e.mu.Unlock()
		if cur == s {
			e.kill(def, s)
		}
	})
}

// kill detiene el proceso MCP y lo retira del registro de sesiones.
func (e *Executor) kill(def mcp.Def, s *session) {
	e.mu.Lock()
	if cur := e.sessions[def.Nombre]; cur == s {
		delete(e.sessions, def.Nombre)
	}
	e.mu.Unlock()
	s.kill()
}

// kill mata el proceso y cierra sus recursos.
func (s *session) kill() {
	if s.isClosed {
		return
	}
	s.isClosed = true
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	_ = s.stdin.Close()
}

// SesionesActivas devuelve los nombres de MCPs con proceso vivo (para el
// estado del servicio: GET /mcp no debe lanzar procesos).
func (e *Executor) SesionesActivas() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.sessions))
	for name := range e.sessions {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Shutdown mata todas las sesiones vivas (para cierre ordenado del servicio).
func (e *Executor) Shutdown() {
	e.mu.Lock()
	sessions := make([]string, 0, len(e.sessions))
	for name := range e.sessions {
		sessions = append(sessions, name)
	}
	e.mu.Unlock()
	for _, name := range sessions {
		e.mu.Lock()
		s := e.sessions[name]
		e.mu.Unlock()
		if s != nil {
			e.kill(mcp.Def{Nombre: name}, s)
		}
	}
}
