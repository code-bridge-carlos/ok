// Package mcp define el registro de MCPs disponibles en s4 (spec S4, punto 9):
// Context7, grep.app, MCP GitHub y MCP Render. Las definiciones por defecto
// pueden sobrescribirse o ampliarse con variables de entorno (ver config).
package mcp

import (
	"sort"
	"strings"

	"s4-mcps/internal/config"
)

// Def describe cómo lanzar un servidor MCP (processo hijo stdio).
type Def struct {
	Nombre  string   // identificador usado en la API ("context7", "grepapp", ...)
	Comando string   // ejecutable (p. ej. "npx")
	Args    []string // argumentos del ejecutable
	Env     []string // pares CLAVE=VALOR adicionales para el proceso
}

// Defaults devuelve el registro base de MCPs de la spec, aplicando las
// sobrescrituras/extensiones del entorno (MCP_<NOMBRE>_CMD, MCP_EXTRA_...).
func Defaults(cfg config.Config) []Def {
	defs := []Def{
		{Nombre: "context7", Comando: "npx", Args: []string{"-y", "@upstash/context7-mcp"}},
		{Nombre: "grepapp", Comando: "npx", Args: []string{"-y", "@grep-app/mcp-server"}},
		{Nombre: "github", Comando: "npx", Args: []string{"-y", "@github/mcp-server"}},
		{Nombre: "render", Comando: "npx", Args: []string{"-y", "@renderinc/mcp-server"}},
	}

	// Tokens de los MCPs que los requieren (spec: operar con la cuenta real).
	for i := range defs {
		switch defs[i].Nombre {
		case "github":
			if cfg.GitHubToken != "" {
				defs[i].Env = append(defs[i].Env,
					"GITHUB_TOKEN="+cfg.GitHubToken,
					"GITHUB_PERSONAL_ACCESS_TOKEN="+cfg.GitHubToken)
			}
		case "render":
			if cfg.RenderApiKey != "" {
				defs[i].Env = append(defs[i].Env, "RENDER_API_KEY="+cfg.RenderApiKey)
			}
		}
	}

	// Sobrescrituras y extensiones del entorno (nombres en minúsculas).
	merged := map[string]Def{}
	for _, d := range defs {
		merged[d.Nombre] = d
	}
	for name, ov := range cfg.McpOverrides {
		d := merged[name]
		d.Nombre = name
		if ov.Comando != "" {
			d.Comando = ov.Comando
		}
		if ov.Args != nil {
			d.Args = ov.Args
		}
		if ov.Env != nil {
			d.Env = append(d.Env, ov.Env...)
		}
		merged[name] = d
	}

	out := make([]Def, 0, len(merged))
	for _, d := range merged {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nombre < out[j].Nombre })
	return out
}

// Registry permite buscar un MCP por nombre (case-insensitive).
type Registry struct {
	defs map[string]Def
}

// NewRegistry indexa las definiciones dadas.
func NewRegistry(defs []Def) *Registry {
	r := &Registry{defs: map[string]Def{}}
	for _, d := range defs {
		r.defs[strings.ToLower(d.Nombre)] = d
	}
	return r
}

// Get devuelve la definición del MCP o false si no está configurado.
func (r *Registry) Get(nombre string) (Def, bool) {
	d, ok := r.defs[strings.ToLower(nombre)]
	return d, ok
}

// Nombres lista los MCPs configurados, ordenados.
func (r *Registry) Nombres() []string {
	out := make([]string, 0, len(r.defs))
	for name := range r.defs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
