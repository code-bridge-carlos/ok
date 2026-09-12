// Package mcplogs guarda el registro opcional de ejecuciones de MCPs en la
// tabla mcp_logs (spec S4, punto 19). Es best-effort: si no hay base de datos
// configurada, las llamadas son no-op y NUNCA rompen la petición en curso
// (misma filosofía que el espejo en s3).
package mcplogs

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Logger persiste ejecuciones de MCPs. Instancias nil o sin base son seguras.
type Logger struct {
	db *sql.DB
}

// New construye el Logger sobre una conexión ya abierta.
func New(db *sql.DB) *Logger { return &Logger{db: db} }

// Registrar inserta una fila en mcp_logs (estado "ok" o "error").
func (l *Logger) Registrar(ctx context.Context, mcp, herramienta string, parametros, resultado any, estado, errMsg string) {
	if l == nil || l.db == nil {
		return
	}
	pj, _ := json.Marshal(parametros)
	rj, _ := json.Marshal(resultado)
	if rj == nil {
		rj = []byte("null")
	}

	actx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := l.db.ExecContext(actx,
		`INSERT INTO mcp_logs (mcp, herramienta, parametros, resultado, estado, error)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		mcp, herramienta, string(pj), string(rj), estado, errMsg); err != nil {
		log.Printf("mcp_logs: no se pudo registrar (%s/%s): %v", mcp, herramienta, err)
	}
}

// Close cierra la conexión subyacente (nil-safe).
func (l *Logger) Close() {
	if l != nil && l.db != nil {
		_ = l.db.Close()
	}
}
