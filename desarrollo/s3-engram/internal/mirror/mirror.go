// Package mirror replica cada observación en el repo GitHub EMGRAN
// (spec S3: cada guardado se escribe en Supabase y en el espejo físico).
// Usa la Contents API de GitHub (PUT /repos/{owner}/{repo}/contents/{path}):
// los archivos viven en observaciones/<id>.md con frontmatter + contenido.
package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"s3-engram/internal/store"
)

// Mirror replica observaciones en GitHub.
type Mirror struct {
	Token string
	Owner string
	Repo  string
	cli   *http.Client
	// baseURL es la raíz de la API; el test lo reemplaza por un servidor fake.
	baseURL string
}

// New construye el mirror. Token vacío ⇒ mirror desactivado (no-op).
func New(token, owner, repo string) *Mirror {
	if owner == "" {
		owner = "1000carlos-prog"
	}
	if repo == "" {
		repo = "EMGRAN"
	}
	return &Mirror{
		Token:   token,
		Owner:   owner,
		Repo:    repo,
		cli:     &http.Client{Timeout: 15 * time.Second},
		baseURL: "https://api.github.com",
	}
}

// Sync espeja una observación: crea el archivo si no existe o lo sobrescribe
// (la Contents API exige el sha del archivo para actualizar). Devuelve nil si
// el mirror está desactivado o si el PUT terminó bien.
func (m *Mirror) Sync(ctx context.Context, o *store.Observation) error {
	if m.Token == "" {
		return nil // mirror desactivado: solo Supabase
	}
	path := "observaciones/" + o.ID + ".md"
	body := renderMD(o)

	sha := ""
	if existing, err := m.getFile(ctx, path); err == nil {
		sha = existing
	} else if httpErr, ok := err.(*httpStatusError); !ok || httpErr.Status != http.StatusNotFound {
		return fmt.Errorf("mirror: consultando %s: %w", path, err)
	}

	msg := fmt.Sprintf("sync [%s] %s", o.Type, o.Title)
	if err := m.putFile(ctx, path, sha, body, msg); err != nil {
		return fmt.Errorf("mirror: escribiendo %s: %w", path, err)
	}
	log.Printf("s3: espejo GitHub OK -> %s/%s/%s", m.Owner, m.Repo, path)
	return nil
}

// ─── GitHub Contents API ─────────────────────────────────────────────────────

type contentsResp struct {
	SHA string `json:"sha"`
}

type httpStatusError struct {
	Status int
	Msg    string
}

func (e *httpStatusError) Error() string { return e.Msg }

// getFile devuelve el sha del archivo, o un error. 404 ⇒ err con Status 404.
func (m *Mirror) getFile(ctx context.Context, path string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", m.baseURL, m.Owner, m.Repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := m.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", &httpStatusError{Status: http.StatusNotFound, Msg: "archivo no existe"}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &httpStatusError{Status: resp.StatusCode, Msg: string(b)}
	}
	var out contentsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// putFile crea (sha vacío) o actualiza el archivo en el repo.
func (m *Mirror) putFile(ctx context.Context, path, sha, body, msg string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", m.baseURL, m.Owner, m.Repo, path)
	payload := map[string]any{
		"message": msg,
		"content": base64.StdEncoding.EncodeToString([]byte(body)),
	}
	if sha != "" {
		payload["sha"] = sha
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpStatusError{Status: resp.StatusCode, Msg: string(b)}
	}
	return nil
}

// ─── Renderizado del archivo ─────────────────────────────────────────────────

func renderMD(o *store.Observation) string {
	ctx := ""
	if o.Context != nil {
		ctx = *o.Context
	}
	topic := ""
	if o.TopicKey != nil {
		topic = *o.TopicKey
	}
	repo := ""
	if o.RepoPath != nil {
		repo = *o.RepoPath
	}
	tags := strings.Join(o.Etiquetas, ", ")

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", o.ID)
	fmt.Fprintf(&b, "title: %q\n", o.Title)
	fmt.Fprintf(&b, "tipo: %s\n", o.Type)
	if ctx != "" {
		fmt.Fprintf(&b, "contexto: %q\n", ctx)
	}
	fmt.Fprintf(&b, "etiquetas: [%s]\n", tags)
	fmt.Fprintf(&b, "autor: %s\n", o.Autor)
	fmt.Fprintf(&b, "project: %s\n", o.Project)
	fmt.Fprintf(&b, "scope: %s\n", o.Scope)
	if topic != "" {
		fmt.Fprintf(&b, "topic_key: %q\n", topic)
	}
	if repo != "" {
		fmt.Fprintf(&b, "repo_path: %s\n", repo)
	}
	fmt.Fprintf(&b, "created_at: %s\n", o.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "updated_at: %s\n", o.UpdatedAt.Format(time.RFC3339))
	b.WriteString("---\n\n")
	b.WriteString(o.Content)
	b.WriteString("\n")
	return b.String()
}