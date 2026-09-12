package main

import (
	"log"
	"net/http"

	"s3-engram/internal/api"
	"s3-engram/internal/config"
	"s3-engram/internal/mirror"
	"s3-engram/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("s3: no se pudo conectar a la base de datos: %v", err)
	}
	defer st.Close()

	// Espejo GitHub (spec S3): activo si hay GITHUB_TOKEN, si no es no-op.
	m := mirror.New(cfg.GitHubToken, cfg.GitHubOwner, cfg.GitHubRepo)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: api.New(st, cfg, m),
	}

	log.Printf("s3-engram (Engram API) escuchando en :%s", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("s3: servidor terminó con error: %v", err)
	}
}