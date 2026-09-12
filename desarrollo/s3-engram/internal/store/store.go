// Package store da acceso a PostgreSQL (Supabase) para las observaciones de
// Engram (spec S3, puntos 7-12 y 22-24).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // driver PostgreSQL
)

// Observation es una observación de Engram (tabla engram_observations, spec punto 22).
type Observation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Type      string    `json:"tipo"`
	Context   *string   `json:"contexto,omitempty"`
	Etiquetas []string  `json:"etiquetas"`
	Autor     string    `json:"autor"`
	Project   string    `json:"project"`
	Scope     string    `json:"scope"`
	TopicKey  *string   `json:"topic_key,omitempty"`
	RepoPath  *string   `json:"repo_path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const observationColumns = `id, title, content, type, context, etiquetas, autor,
	project, scope, topic_key, repo_path, created_at, updated_at`

// Store es el acceso a la base de datos de s3.
type Store struct {
	db *sql.DB
}

// New abre la conexión y verifica que responda.
func New(databaseURL string) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("DATABASE_URL vacío: s3 requiere conexión a Supabase")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("abriendo conexión: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping a la base de datos: %w", err)
	}
	return &Store{db: db}, nil
}

// Close cierra la conexión.
func (s *Store) Close() error { return s.db.Close() }

// Insert guarda una observación nueva y devuelve la fila persistida.
func (s *Store) Insert(ctx context.Context, o *Observation) (*Observation, error) {
	if strings.TrimSpace(o.Title) == "" {
		return nil, errors.New("title requerido")
	}
	if strings.TrimSpace(o.Content) == "" {
		return nil, errors.New("content requerido")
	}
	normalize(o)

	const q = `INSERT INTO engram_observations
		(title, content, type, context, etiquetas, autor, project, scope, topic_key, repo_path)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id, created_at, updated_at`

	row := s.db.QueryRowContext(ctx, q,
		o.Title, o.Content, o.Type, o.Context, o.Etiquetas,
		o.Autor, o.Project, o.Scope, o.TopicKey, o.RepoPath,
	)
	if err := row.Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, fmt.Errorf("insertando observación: %w", err)
	}
	return o, nil
}

// Update actualiza una observación existente. Devuelve (nil, nil) si el id no existe.
func (s *Store) Update(ctx context.Context, id string, o *Observation) (*Observation, error) {
	if strings.TrimSpace(o.Title) == "" {
		return nil, errors.New("title requerido")
	}
	if strings.TrimSpace(o.Content) == "" {
		return nil, errors.New("content requerido")
	}
	normalize(o)

	const q = `UPDATE engram_observations SET
		title=$2, content=$3, type=$4, context=$5, etiquetas=$6,
		autor=$7, project=$8, scope=$9, topic_key=$10, repo_path=$11,
		updated_at=now()
		WHERE id=$1
		RETURNING ` + observationColumns

	out := *o
	row := s.db.QueryRowContext(ctx, q,
		id, o.Title, o.Content, o.Type, o.Context, o.Etiquetas,
		o.Autor, o.Project, o.Scope, o.TopicKey, o.RepoPath,
	)
	if err := scanInto(&out, row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("actualizando observación: %w", err)
	}
	return &out, nil
}

// Get devuelve una observación por id. Devuelve (nil, nil) si no existe.
func (s *Store) Get(ctx context.Context, id string) (*Observation, error) {
	const q = `SELECT ` + observationColumns + ` FROM engram_observations WHERE id=$1`

	var o Observation
	if err := scanInto(&o, s.db.QueryRowContext(ctx, q, id)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("obteniendo observación: %w", err)
	}
	return &o, nil
}

// SearchCriteria filtra las búsquedas (spec puntos 10 y 13).
type SearchCriteria struct {
	Q        string // palabras clave en title/content/contexto (ILIKE)
	Type     string // filtro por tipo exacto
	Etiqueta string // filtro por etiqueta (membership)
	Desde    string // created_at >= (fecha o RFC3339)
	Hasta    string // created_at <= (fecha o RFC3339)
	Limite   int    // por defecto 50, máximo 100
}

// Search devuelve observaciones que cumplen los criterios, más recientes primero.
func (s *Store) Search(ctx context.Context, c SearchCriteria) ([]Observation, error) {
	if c.Limite <= 0 {
		c.Limite = 50
	}
	if c.Limite > 100 {
		c.Limite = 100
	}

	var conds []string
	var args []any
	n := 1
	// add construye una condición: cada '?' del cond se sustituye por $n, $n+1, ...
	// y vals deben coincidir uno a uno con los '?'.
	add := func(cond string, vals ...any) {
		var sb strings.Builder
		last := 0
		for i := 0; i < len(cond); i++ {
			if cond[i] == '?' {
				sb.WriteString(cond[last:i])
				sb.WriteString(fmt.Sprintf("$%d", n))
				n++
				last = i + 1
			}
		}
		sb.WriteString(cond[last:])
		conds = append(conds, sb.String())
		args = append(args, vals...)
	}

	if c.Q != "" {
		like := "%" + c.Q + "%"
		add(`(title ILIKE ? OR content ILIKE ? OR COALESCE(context,'') ILIKE ?)`,
			like, like, like)
	}
	if c.Type != "" {
		add(`type = ?`, c.Type)
	}
	if c.Etiqueta != "" {
		add(`? = ANY(etiquetas)`, c.Etiqueta)
	}
	if c.Desde != "" {
		add(`created_at >= ?`, c.Desde)
	}
	if c.Hasta != "" {
		add(`created_at <= ?`, c.Hasta)
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	q := `SELECT ` + observationColumns + ` FROM engram_observations` + where +
		` ORDER BY created_at DESC LIMIT ` + fmt.Sprintf("$%d", n)
	args = append(args, c.Limite)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("buscando observaciones: %w", err)
	}
	defer rows.Close()

	var out []Observation
	for rows.Next() {
		var o Observation
		if err := scanInto(&o, rows); err != nil {
			return nil, fmt.Errorf("leyendo fila de búsqueda: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterando búsqueda: %w", err)
	}
	return out, nil
}

// normalize aplica los valores por defecto de una observación.
func normalize(o *Observation) {
	if o.Type == "" {
		o.Type = "manual"
	}
	if o.Autor == "" {
		o.Autor = "s2"
	}
	if o.Project == "" {
		o.Project = "ok"
	}
	if o.Scope == "" {
		o.Scope = "project"
	}
	if o.Etiquetas == nil {
		o.Etiquetas = []string{}
	}
}

// scanner abstrae *sql.Row y *sql.Rows para scanInto.
type scanner interface {
	Scan(dest ...any) error
}

func scanInto(o *Observation, row scanner) error {
	return row.Scan(
		&o.ID, &o.Title, &o.Content, &o.Type, &o.Context, &o.Etiquetas, &o.Autor,
		&o.Project, &o.Scope, &o.TopicKey, &o.RepoPath, &o.CreatedAt, &o.UpdatedAt,
	)
}