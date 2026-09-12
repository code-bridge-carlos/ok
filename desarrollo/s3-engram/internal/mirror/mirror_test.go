package mirror

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"s3-engram/internal/store"
)

// TestMirrorSync ejercita el espejo contra un servidor fake de la Contents API.
func TestMirrorSync(t *testing.T) {
	var (
		gotPath    string
		gotAuth    string
		gotMsg     string
		gotContent string
		gotSHA     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodGet:
			// El archivo ya existe → devuelve sha para que el PUT lo actualice.
			if strings.HasSuffix(r.URL.Path, "observaciones/11111111-2222-3333-4444-555555555555.md") {
				json.NewEncoder(w).Encode(map[string]string{"sha": "abc123"})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			gotPath = r.URL.Path
			var body struct {
				Message string `json:"message"`
				SHA     string `json:"sha"`
				Content string `json:"content"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("PUT body inválido: %v", err)
			}
			gotMsg = body.Message
			gotSHA = body.SHA
			dec, err := base64.StdEncoding.DecodeString(body.Content)
			if err != nil {
				t.Fatalf("content no es base64: %v", err)
			}
			gotContent = string(dec)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	m := &Mirror{Token: "tok", Owner: "1000carlos-prog", Repo: "EMGRAN"}
	m.cli = srv.Client()
	m.baseURL = srv.URL // reemplazo el host en las URLs

	now := time.Date(2026, 9, 12, 5, 0, 0, 0, time.UTC)
	ctxf := "contexto de prueba"
	o := &store.Observation{
		ID: "11111111-2222-3333-4444-555555555555", Title: `Título "con" comillas`,
		Content: "Cuerpo del archivo espejo", Type: "decision", Context: &ctxf,
		Etiquetas: []string{"go", "s3"}, Autor: "s2", Project: "ok", Scope: "project",
		CreatedAt: now, UpdatedAt: now,
	}

	if err := m.Sync(context.Background(), o); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization incorrecto: %q", gotAuth)
	}
	if !strings.HasSuffix(gotPath, "observaciones/"+o.ID+".md") {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	if !strings.HasPrefix(gotMsg, "sync [decision]") {
		t.Fatalf("commit message incorrecto: %q", gotMsg)
	}
	if gotSHA != "abc123" {
		t.Fatalf("sha no se reutilizó para actualizar: %q", gotSHA)
	}
	if !strings.Contains(gotContent, `title: "Título \"con\" comillas"`) || !strings.Contains(gotContent, "Cuerpo del archivo espejo") {
		t.Fatalf("contenido renderizado incompleto:\n%s", gotContent)
	}
	if !strings.Contains(gotContent, "contexto de prueba") || !strings.Contains(gotContent, "etiquetas: [go, s3]") {
		t.Fatalf("frontmatter incompleto:\n%s", gotContent)
	}

	// Actualización con frontmatter RFC3339 para created_at
	if !strings.Contains(gotContent, "created_at: 2026-09-12T05:00:00Z") {
		t.Fatalf("created_at mal formateado:\n%s", gotContent)
	}
}

// TestMirrorDesactivado: token vacío ⇒ no-op sin llamadas de red.
func TestMirrorDesactivado(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()

	m := New("", "1000carlos-prog", "EMGRAN")
	m.cli = srv.Client()
	m.baseURL = srv.URL

	o := &store.Observation{ID: "x", Title: "t", Content: "c"}
	if err := m.Sync(context.Background(), o); err != nil {
		t.Fatalf("Sync con mirror desactivado: %v", err)
	}
	if calls != 0 {
		t.Fatalf("mirror desactivado hizo %d llamadas", calls)
	}
}

// TestMirrorError5xx: la API devuelve 422 ⇒ Sync devuelve error.
func TestMirrorError5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"message":"sha wasn't supplied"}`)
	}))
	defer srv.Close()

	m := &Mirror{Token: "tok", Owner: "o", Repo: "r"}
	m.cli = srv.Client()
	m.baseURL = srv.URL

	o := &store.Observation{ID: "x", Title: "t", Content: "c"}
	if err := m.Sync(context.Background(), o); err == nil {
		t.Fatal("Sync debería fallar ante 422")
	}
}