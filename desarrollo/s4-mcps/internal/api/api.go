// Package api expone la API REST de s4 (spec S4, puntos 12-15, 25, 31):
//
//	GET  /health                       → estado del servicio (sin auth)
//	GET  /mcp                          → MCPs configurados y su estado (auth)
//	POST /mcp/{mcp}/ejecutar           → ejecutar herramienta de un MCP (auth)
//
// Autenticación: cabecera X-Control-Token (o Authorization: Bearer <token>),
// misma convención que s3. CORS: solo orígenes de CORS_ALLOWED_ORIGINS.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"s4-mcps/internal/mcp"
	"s4-mcps/internal/mcpexec"
)

// Server implementa la API de s4.
type Server struct {
	controlToken string
	origins      []string
	reg          *mcp.Registry
	exec         *mcpexec.Executor
	mux          *http.ServeMux
	// registrar persiste en mcp_logs (spec punto 19); nil si sin base de datos.
	registrar func(mcp, herramienta string, parametros map[string]any, resultado any, estado, errMsg string)
}

// WithRegistro configura el registro opcional de ejecuciones (mcp_logs).
func WithRegistro(fn func(mcp, herramienta string, parametros map[string]any, resultado any, estado, errMsg string)) func(*Server) {
	return func(s *Server) { s.registrar = fn }
}

// New construye el servidor con sus rutas y opciones.
func New(controlToken string, origins []string, reg *mcp.Registry, exec *mcpexec.Executor, opts ...func(*Server)) *Server {
	s := &Server{
		controlToken: controlToken,
		origins:      origins,
		reg:          reg,
		exec:         exec,
		mux:          http.NewServeMux(),
	}
	for _, o := range opts {
		o(s)
	}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /mcp", s.handleListaMCPs)
	s.mux.HandleFunc("POST /mcp/{mcp}/ejecutar", s.handleEjecutar)
	return s
}

// ServeHTTP aplica logging, CORS y autenticación antes de despachar.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	allowed := s.applyCORS(w, r)
	if r.Method == http.MethodOptions {
		if !allowed {
			writeError(w, http.StatusForbidden, "origen no permitido")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/health" && !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "token de control inválido o ausente")
		return
	}
	log.Printf("%s %s → %d (%s)", r.Method, r.URL.Path, http.StatusOK, time.Since(start))
	s.mux.ServeHTTP(w, r)
}

// ─── Auth y CORS ─────────────────────────────────────────────────────────────

// authorized valida la cabecera X-Control-Token o Authorization: Bearer.
func (s *Server) authorized(r *http.Request) bool {
	if s.controlToken == "" {
		return true // sin token configurado el servicio queda abierto (desarrollo)
	}
	tok := r.Header.Get("X-Control-Token")
	if tok == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return tok != "" && tok == s.controlToken
}

// applyCORS añade las cabeceras CORS si el origen está permitido.
// Devuelve true si la petición es aceptable (origen permitido o sin Origin).
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if !contains(s.origins, origin) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, X-Control-Token, Authorization")
	h.Set("Vary", "Origin")
	return true
}

// ─── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"estado": "ok"})
}

func (s *Server) handleListaMCPs(w http.ResponseWriter, _ *http.Request) {
	activos := map[string]bool{}
	for _, n := range s.exec.SesionesActivas() {
		activos[n] = true
	}
	out := make([]map[string]any, 0, len(s.reg.Nombres()))
	for _, nombre := range s.reg.Nombres() {
		def, _ := s.reg.Get(nombre)
		out = append(out, map[string]any{
			"nombre":        nombre,
			"comando":       def.Comando,
			"sesion_activa": activos[nombre],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mcps": out})
}

type ejecutarReq struct {
	Herramienta string         `json:"herramienta"`
	Parametros  map[string]any `json:"parametros"`
}

func (s *Server) handleEjecutar(w http.ResponseWriter, r *http.Request) {
	mcpName := strings.ToLower(r.PathValue("mcp"))
	var req ejecutarReq
	if err := decodeJSON(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Herramienta) == "" {
		writeError(w, http.StatusBadRequest, "falta 'herramienta'")
		return
	}
	if _, ok := s.reg.Get(mcpName); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("MCP '%s' no está configurado", mcpName))
		return
	}

	log.Printf("ejecutando %s/%s parametros=%v", mcpName, req.Herramienta, req.Parametros)
	res, err := s.exec.Execute(r.Context(), mcpName, req.Herramienta, req.Parametros)
	if err != nil {
		log.Printf("fallo MCP %s/%s: %v", mcpName, req.Herramienta, err)
		if s.registrar != nil {
			s.registrar(mcpName, req.Herramienta, req.Parametros, nil, "error", err.Error())
		}
		// Spec punto 11: fallo del MCP → error estructurado + proceso detenido.
		writeError(w, http.StatusBadGateway, fmt.Sprintf("fallo del MCP '%s': %v", mcpName, err))
		return
	}
	if s.registrar != nil {
		estado := "ok"
		if res.IsError {
			estado = "error"
		}
		s.registrar(mcpName, req.Herramienta, req.Parametros, res, estado, "")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"mcp":         mcpName,
		"herramienta": req.Herramienta,
		"resultado":   res,
	})
}

// ─── Utilidades ──────────────────────────────────────────────────────────────

// decodeJSON lee el body con un límite y decodifica el JSON (mismos tags de
// spec; sin renombrados).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, max int64) error {
	body := http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Si sobra contenido tras el primer objeto, el body no era un JSON único.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("se esperaba un solo objeto JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
