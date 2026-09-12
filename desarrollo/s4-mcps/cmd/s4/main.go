// s4 arranca el servicio de MCPs bajo demanda (spec S4): API REST que lanza
// servidores MCP por stdio solo cuando se usan, los reutiliza entre peticiones
// y los detiene tras un periodo de gracia (MCP_GRACE) o al apagar el servicio.
//
// Variables de entorno: PORT, CONTROL_TOKEN, CORS_ALLOWED_ORIGINS, GITHUB_TOKEN,
// RENDER_API_KEY, SUPABASE_URL, SUPABASE_ANON_KEY, DATABASE_URL,
// PROXY_ENABLED, PROXY_URL, MCP_TIMEOUT, MCP_GRACE, MCP_<NOMBRE>_CMD/_ARGS/_ENV,
// MCP_EXTRA_<NOMBRE>_CMD (ver internal/config).
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"s4-mcps/internal/api"
	"s4-mcps/internal/config"
	"s4-mcps/internal/mcp"
	"s4-mcps/internal/mcpexec"
	"s4-mcps/internal/mcplogs"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// abrirConSchema conecta con la base y aplica db/schema.sql (con reintentos,
// como la suite E2E de s3: Postgres puede tardar en estar listo).
func abrirConSchema(dbURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, err
	}
	schema, err := os.ReadFile("db/schema.sql")
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < 10; i++ {
		if err := db.Ping(); err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		if _, err := db.Exec(string(schema)); err != nil {
			return nil, err
		}
		return db, nil
	}
	return nil, lastErr
}

func main() {
	cfg := config.Load()

	// Registro de MCPs (spec punto 9) con sobrescrituras del entorno.
	reg := mcp.NewRegistry(mcp.Defaults(cfg))
	log.Printf("MCPs configurados: %v", reg.Nombres())

	// Proxy opcional hacia s1 (HTTP_PROXY/HTTPS_PROXY para los procesos MCP).
	var baseEnv []string
	if cfg.ProxyEnabled && cfg.ProxyURL != "" {
		baseEnv = append(baseEnv,
			"HTTP_PROXY="+cfg.ProxyURL,
			"HTTPS_PROXY="+cfg.ProxyURL,
			"ALL_PROXY="+cfg.ProxyURL,
			"NO_PROXY=localhost,127.0.0.1,*.onrender.com")
		log.Printf("proxy activado: %s", cfg.ProxyURL)
	}

	exec := mcpexec.New(reg, baseEnv,
		mcpexec.WithTimeout(cfg.McpTimeout),
		mcpexec.WithGrace(cfg.McpGrace))

	// Registro opcional de ejecuciones (spec punto 19): sin base de datos el
	// servicio funciona igual; con DATABASE_URL se aplica db/schema.sql.
	var logger = mcplogs.New(nil)
	if cfg.DatabaseURL != "" {
		db, err := abrirConSchema(cfg.DatabaseURL)
		if err != nil {
			log.Printf("aviso: DATABASE_URL presente pero mcp_logs no disponible: %v", err)
		} else {
			logger = mcplogs.New(db)
			defer func() {
				db.Close()
			}()
			log.Println("mcp_logs activo")
		}
	}
	registrar := func(mcpName, herramienta string, parametros map[string]any, resultado any, estado, errMsg string) {
		logger.Registrar(context.Background(), mcpName, herramienta, parametros, resultado, estado, errMsg)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.New(cfg.ControlToken, cfg.AllowedOrigins, reg, exec, api.WithRegistro(registrar)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Cierre ordenado: al recibir SIGTERM/SIGINT se detienen los MCPs vivos
	// y se espera a las peticiones en curso (spec punto 4: sin parásitos).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("s4 escuchando en :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	stop()
	log.Println("apagando s4…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	exec.Shutdown()
	log.Println("s4 detenido")
	os.Exit(0)
}
