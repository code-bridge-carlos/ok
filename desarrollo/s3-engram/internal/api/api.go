// Package api expone la API REST de s3 (Engram) para s2 (spec S3, puntos 13-17).
// - POST   /observations          → insertar observación
// - PUT    /observations/{id}     → actualizar observación
// - GET    /observations/{id}     → obtener por id
// - GET    /observations/search   → búsqueda multi-criterio
// - GET    /health                → health check (keep-alive de s1)
// Autenticación: header X-Control-Token (o Authorization: Bearer) con CONTROL_TOKEN.
// CORS controlado por allowlist (CORS_ALLOWED_ORIGINS). JSON estructurado puro.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"s3-engram/internal/config"
	"s3-engram/internal/mirror"
	"s3-engram/internal/store"
)

// Server agrupa las dependencias de la API.
type Server struct {
	store  *store.Store
	cfg    config.Config
	mirror *mirror.Mirror
}

// obsResp envuelve una observación con el estado del espejo GitHub (spec S3).
type obsResp struct {
	store.Observation
	Mirror string `json:"mirror"`
}

// New construye el handler principal con las rutas registradas.
func New(st *store.Store, cfg config.Config, m *mirror.Mirror) http.Handler {
	s := &Server{store: st, cfg: cfg, mirror: m}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /observations", s.requireControl(s.handleCreate))
	mux.HandleFunc("PUT /observations/{id}", s.requireControl(s.handleUpdate))
	mux.HandleFunc("GET /observations/{id}", s.requireControl(s.handleGet))
	mux.HandleFunc("GET /observations/search", s.requireControl(s.handleSearch))
	mux.HandleFunc("OPTIONS /{path...}", s.handlePreflight)

	return mux
}

// ─── Middleware de autenticación ─────────────────────────────────────────────

// requireControl valida el token de control de s2 en cada request (spec punto 15).
func (s *Server) requireControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Control-Token")
		if token == "" {
			if b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); b != "" && b != r.Header.Get("Authorization") {
				token = b
			}
		}
		if token == "" || s.cfg.ControlToken == "" || token != s.cfg.ControlToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "token de control inválido o ausente",
			})
			return
		}
		next(w, r)
	}
}

// ─── CORS controlado (spec punto 16) ─────────────────────────────────────────

// originAllowed devuelve true si el Origin está en la allowlist.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true // llamadas servidor-a-servidor sin Origin
	}
	for _, o := range s.cfg.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !s.originAllowed(origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Control-Token, Authorization")
	w.Header().Set("Access-Control-Max-Age", "600")
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !s.originAllowed(origin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origen no permitido"})
		return
	}
	s.applyCORS(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"servicio": "s3-engram",
		"rol":     "memoria persistente (Engram) — API para s2",
	})
}

// handleCreate: POST /observations
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var o store.Observation
	if err := decodeJSON(w, r, &o); err != nil {
		return
	}
	o.ID = "" // nunca aceptar id externo en inserción

	created, err := s.store.Insert(r.Context(), &o)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp := obsResp{Observation: *created, Mirror: "ok"}
	if err := s.mirror.Sync(r.Context(), created); err != nil {
		log.Printf("s3: espejo GitHub falló (guardado en DB ok): %v", err)
		resp.Mirror = "error: " + err.Error()
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleUpdate: PUT /observations/{id}
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id requerido"})
		return
	}
	var o store.Observation
	if err := decodeJSON(w, r, &o); err != nil {
		return
	}

	updated, err := s.store.Update(r.Context(), id, &o)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if updated == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "observación no encontrada"})
		return
	}
	resp := obsResp{Observation: *updated, Mirror: "ok"}
	if err := s.mirror.Sync(r.Context(), updated); err != nil {
		log.Printf("s3: espejo GitHub falló (guardado en DB ok): %v", err)
		resp.Mirror = "error: " + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGet: GET /observations/{id}
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o, err := s.store.Get(r.Context(), id)
	if err != nil {
		log.Printf("s3: GET /observations/%s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "error interno"})
		return
	}
	if o == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "observación no encontrada"})
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// handleSearch: GET /observations/search?q=&tipo=&etiqueta=&desde=&hasta=&limite=
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	crit := store.SearchCriteria{
		Q:        q.Get("q"),
		Type:     q.Get("tipo"),
		Etiqueta: q.Get("etiqueta"),
		Desde:    q.Get("desde"),
		Hasta:    q.Get("hasta"),
	}
	if l := q.Get("limite"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			crit.Limite = n
		}
	}

	results, err := s.store.Search(r.Context(), crit)
	if err != nil {
		log.Printf("s3: búsqueda: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "error interno"})
		return
	}
	if results == nil {
		results = []store.Observation{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(results),
		"resultados": results,
	})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("s3: error escribiendo respuesta JSON: %v", err)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // 32 MiB tope
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido: " + err.Error()})
		return err
	}
	// El struct Observation usa tags al estilo spec ("tipo", "contexto", "etiquetas"),
	// así que el JSON entra directo sin renombrar nada.
	data, err := json.Marshal(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido: " + err.Error()})
		return err
	}
	return nil
}