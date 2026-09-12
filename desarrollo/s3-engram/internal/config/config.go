// Package config lee la configuración de s3 (Engram) desde variables de entorno.
// Variables (spec S3, punto 26): SUPABASE_URL, SUPABASE_ANON_KEY, DATABASE_URL,
// GITHUB_TOKEN, CONTROL_TOKEN, PROXY_ENABLED, PROXY_URL, PORT, CORS_ALLOWED_ORIGINS.
package config

import (
	"os"
	"strings"
)

// Config reúne toda la configuración de s3.
type Config struct {
	Port             string
	DatabaseURL      string
	SupabaseURL      string
	SupabaseAnonKey  string
	GitHubToken      string
	GitHubOwner      string
	GitHubRepo       string
	ControlToken     string
	AllowedOrigins   []string
	ProxyEnabled     bool
	ProxyURL         string
	ProxyAssignments string // URL de Supabase para leer proxy asignado (opcional)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load construye la Config desde el entorno.
func Load() Config {
	return Config{
		Port:             env("PORT", "3000"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		SupabaseURL:      os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey:  os.Getenv("SUPABASE_ANON_KEY"),
		GitHubToken:      os.Getenv("GITHUB_TOKEN"),
		GitHubOwner:      env("GITHUB_OWNER", "1000carlos-prog"),
		GitHubRepo:       env("GITHUB_REPO", "EMGRAN"),
		ControlToken:     os.Getenv("CONTROL_TOKEN"),
		AllowedOrigins:   splitList(os.Getenv("CORS_ALLOWED_ORIGINS")),
		ProxyEnabled:     isTruthy(os.Getenv("PROXY_ENABLED")),
		ProxyURL:         os.Getenv("PROXY_URL"),
		ProxyAssignments: os.Getenv("PROXY_ASSIGNMENTS_URL"),
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}