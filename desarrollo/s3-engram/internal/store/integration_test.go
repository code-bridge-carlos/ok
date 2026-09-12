package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegrationCRUDyBusqueda prueba insert/get/update/search contra Postgres real.
// Se activa con TEST_DATABASE_URL (local o Supabase); sin variable, se salta.
func TestIntegrationCRUDyBusqueda(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL no definido — se salta la integración")
	}

	ctx := context.Background()
	st, err := New(dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	// Insert o1 (decisión, con contexto y etiquetas)
	ctx1 := "contexto: decisión de arquitectura"
	o1, err := st.Insert(ctx, &Observation{
		Title:     "Integración s3 busca",
		Content:   "Cuerpo con la palabra zorzal y referencia a pgx",
		Type:      "decision",
		Context:   &ctx1,
		Etiquetas: []string{"go", "s3"},
		Autor:     "test",
		Project:   "ok",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("Insert o1: %v", err)
	}
	if o1.ID == "" {
		t.Fatal("Insert no devolvió id")
	}

	// Insert o2 (bugfix, otra etiqueta)
	o2, err := st.Insert(ctx, &Observation{
		Title:     "Bug menor en auth",
		Content:   "Token expiraba antes de tiempo",
		Type:      "bugfix",
		Etiquetas: []string{"auth"},
		Autor:     "test",
	})
	if err != nil {
		t.Fatalf("Insert o2: %v", err)
	}

	// Get por id
	got, err := st.Get(ctx, o1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Title != o1.Title || got.Autor != "test" || got.Scope != "project" {
		t.Fatalf("Get devolvió fila inesperada: %+v", got)
	}
	if len(got.Etiquetas) != 2 || got.Etiquetas[0] != "go" {
		t.Fatalf("etiquetas mal persistidas: %v", got.Etiquetas)
	}

	// Búsqueda por texto en content
	byQ, err := st.Search(ctx, SearchCriteria{Q: "zorzal"})
	if err != nil {
		t.Fatalf("Search q: %v", err)
	}
	if !contains(byQ, o1.ID) || contains(byQ, o2.ID) {
		t.Fatalf("Search q incorrecta: %v", byQ)
	}

	// Búsqueda por tipo
	byType, err := st.Search(ctx, SearchCriteria{Type: "bugfix"})
	if err != nil {
		t.Fatalf("Search tipo: %v", err)
	}
	if !contains(byType, o2.ID) || contains(byType, o1.ID) {
		t.Fatalf("Search tipo incorrecta: %v", byType)
	}

	// Búsqueda por etiqueta
	byTag, err := st.Search(ctx, SearchCriteria{Etiqueta: "go"})
	if err != nil {
		t.Fatalf("Search etiqueta: %v", err)
	}
	if !contains(byTag, o1.ID) || contains(byTag, o2.ID) {
		t.Fatalf("Search etiqueta incorrecta: %v", byTag)
	}

	// Búsqueda por fecha (ventana que incluye o1, excluye o2 si fuera más nuevo)
	desde := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	hasta := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	byDate, err := st.Search(ctx, SearchCriteria{Desde: desde, Hasta: hasta})
	if err != nil {
		t.Fatalf("Search fecha: %v", err)
	}
	if len(byDate) != 2 {
		t.Fatalf("Search fecha debería devolver 2, devolvió %d", len(byDate))
	}

	// Update
	o1.Title = "Integración s3 busca (editada)"
	upd, err := st.Update(ctx, o1.ID, o1)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd == nil || upd.Title != o1.Title {
		t.Fatalf("Update no persistió: %+v", upd)
	}
	if !upd.UpdatedAt.After(o1.CreatedAt) {
		t.Fatalf("updated_at no se actualizó: created=%v updated=%v", o1.CreatedAt, upd.UpdatedAt)
	}

	// Update de id inexistente → nil sin error
	missing, err := st.Update(ctx, "00000000-0000-0000-0000-000000000000", o1)
	if err != nil {
		t.Fatalf("Update inexistente: %v", err)
	}
	if missing != nil {
		t.Fatalf("Update inexistente debería devolver nil: %+v", missing)
	}

	// Get de id inexistente → nil
	nilGet, err := st.Get(ctx, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("Get inexistente: %v", err)
	}
	if nilGet != nil {
		t.Fatalf("Get inexistente debería devolver nil: %+v", nilGet)
	}

	// Límite
	lim, err := st.Search(ctx, SearchCriteria{Limite: 1})
	if err != nil {
		t.Fatalf("Search limite: %v", err)
	}
	if len(lim) != 1 {
		t.Fatalf("Search limite debería devolver 1, devolvió %d", len(lim))
	}
}

func contains(list []Observation, id string) bool {
	for _, o := range list {
		if o.ID == id {
			return true
		}
	}
	return false
}