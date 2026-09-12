// Package config lee la configuración de s4 (MCPs bajo demanda) del entorno.
// Variables (spec S4, puntos 22-24, 6): PORT, CONTROL_TOKEN, SUPABASE_URL,
// SUPABASE_ANON_KEY, DATABASE_URL, GITHUB_TOKEN, RENDER_API_KEY,
// CORS_ALLOWED_ORIGINS, PROXY_ENABLED, PROXY_URL, MCP_TIMEOUT, MCP_GRACE.
package config

import (
	"os"
	"strings"
	"time"
)

// Config reúne toda la configuración de s4.
type Config struct {
	Port            string
	DatabaseURL     string
	SupabaseURL     string
	SupabaseAnonKey string
	GitHubToken     string
	RenderApiKey    string
	ControlToken    string
	AllowedOrigins  []string
	ProxyEnabled    bool
	ProxyURL        string
	McpTimeout      time.Duration // tiempo máximo de una ejecución de MCP
	McpGrace        time.Duration // espera antes de matar un MCP sin peticiones
	McpOverrides    map[string]Overrides
}

// Overrides permite redefinir o añadir MCPs vía entorno (útil en pruebas y
// para ajustar versiones sin tocar código): MCP_<NOMBRE>_CMD / _ARGS / _ENV.
type Overrides struct {
	Comando string
	Args    []string
	Env     []string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load construye la Config desde el entorno.
func Load() Config {
	timeout, _ := time.ParseDuration(env("MCP_TIMEOUT", "90s"))
	grace, _ := time.ParseDuration(env("MCP_GRACE", "5s"))
	return Config{
		Port:            env("PORT", "3000"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		SupabaseURL:     os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey: os.Getenv("SUPABASE_ANON_KEY"),
		GitHubToken:     os.Getenv("GITHUB_TOKEN"),
		RenderApiKey:    os.Getenv("RENDER_API_KEY"),
		ControlToken:    os.Getenv("CONTROL_TOKEN"),
		AllowedOrigins:  splitList(os.Getenv("CORS_ALLOWED_ORIGINS")),
		ProxyEnabled:    isTruthy(os.Getenv("PROXY_ENABLED")),
		ProxyURL:        os.Getenv("PROXY_URL"),
		McpTimeout:      timeout,
		McpGrace:        grace,
		McpOverrides:    readOverrides(),
	}
}

// readOverrides parsea MCP_<NOMBRE>_CMD / _ARGS / _ENV y MCP_EXTRA_<NOMBRE>_CMD.
func readOverrides() map[string]Overrides {
	out := map[string]Overrides{}
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "MCP_") {
			continue
		}
		rest := strings.TrimPrefix(name, "MCP_")
		idx := strings.LastIndex(rest, "_")
		if idx <= 0 {
			continue
		}
		mcpName := strings.ToLower(rest[:idx])
		field := rest[idx+1:]
		o := out[mcpName]
		switch field {
		case "CMD":
			o.Comando = value
		case "ARGS":
			o.Args = strings.Fields(value)
		case "ENV":
			o.Env = splitPairs(value)
		}
		if o.Comando != "" || o.Args != nil || o.Env != nil {
			out[mcpName] = o
		}
	}
	return out
}

func splitPairs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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
