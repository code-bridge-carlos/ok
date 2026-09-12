package mcpexec

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"s4-mcps/internal/mcp"
)

var mockBin string

func TestMain(m *testing.M) {
	// Compila el MCP simulado una sola vez para todos los tests.
	dir, err := os.MkdirTemp("", "s4mock")
	if err != nil {
		panic(err)
	}
	mockBin = dir + "/mockmcp"
	cmd := exec.Command("go", "build", "-o", mockBin, "../../cmd/mockmcp")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		panic("no se pudo compilar el mock MCP: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func pidFilePath(name string) string {
	return os.TempDir() + "/s4mock-" + name + ".pid"
}

// pidsLanzados devuelve los PIDs que el mock registró (uno por lanzamiento).
func pidsLanzados(t *testing.T, name string) []int {
	t.Helper()
	data, err := os.ReadFile(pidFilePath(name))
	if err != nil {
		return nil
	}
	var out []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func newTestExecutor(t *testing.T, defs ...mcp.Def) *Executor {
	t.Helper()
	e := New(mcp.NewRegistry(defs), []string{"MOCK_AUX=1"},
		WithTimeout(3*time.Second), WithGrace(250*time.Millisecond))
	t.Cleanup(e.Shutdown)
	return e
}

func mockDef(name string, env ...string) mcp.Def {
	os.Remove(pidFilePath(name)) // pidfile limpio por test (O_APPEND en el mock)
	full := append([]string{"MOCK_PIDFILE=" + pidFilePath(name)}, env...)
	return mcp.Def{Nombre: name, Comando: mockBin, Env: full}
}

// TestEjecutarHerramienta: flujo feliz completo (handshake + tools/call).
func TestEjecutarHerramienta(t *testing.T) {
	e := newTestExecutor(t, mockDef("fake"))
	res, err := e.Execute(context.Background(), "fake", "buscar", map[string]any{"q": "golang"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatal("isError inesperado")
	}
	texto := ""
	if len(res.Content) == 1 {
		texto = res.Content[0].Text
	}
	if !strings.Contains(texto, "resultado de buscar") || !strings.Contains(texto, "golang") {
		t.Fatalf("contenido inesperado: %+v", res.Content)
	}
	// Tras la gracia, el proceso debe morir (punto 30: sin procesos parásitos).
	time.Sleep(600 * time.Millisecond)
	if pid := pidsLanzados(t, "fake"); len(pid) == 1 && alive(pid[0]) {
		t.Fatal("el proceso MCP siguió vivo tras la gracia")
	}
}

// TestReusoSesion: dos peticiones concurrentes comparten UN solo proceso.
func TestReusoSesion(t *testing.T) {
	e := newTestExecutor(t, mockDef("compartido"))
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Execute(context.Background(), "compartido", "buscar", nil); err != nil {
				t.Errorf("Execute concurrente: %v", err)
			}
		}()
	}
	wg.Wait()
	pids := pidsLanzados(t, "compartido")
	if len(pids) != 1 {
		t.Fatalf("se esperaba 1 lanzamiento del MCP, hubo %d (%v)", len(pids), pids)
	}
	// Sin peticiones pendientes, el proceso termina tras la gracia (punto 8).
	time.Sleep(600 * time.Millisecond)
	if alive(pids[0]) {
		t.Fatal("sesión compartida quedó viva sin peticiones")
	}
}

// TestListTools: el MCP responde tools/list con sus herramientas.
func TestListTools(t *testing.T) {
	e := newTestExecutor(t, mockDef("fake"))
	tools, cmd, err := e.ListTools(context.Background(), "fake")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if cmd == "" {
		t.Fatal("comando vacío")
	}
	if len(tools) != 2 || tools[0].Name != "buscar" {
		t.Fatalf("tools inesperadas: %+v", tools)
	}
}

// TestMCPErrorHerramienta: isError=true pasa en el resultado (punto 11).
func TestMCPErrorHerramienta(t *testing.T) {
	e := newTestExecutor(t, mockDef("falso", "MOCK_FAIL_TOOL=1"))
	res, err := e.Execute(context.Background(), "falso", "seRompe", nil)
	if err != nil {
		t.Fatalf("isError de herramienta no debería fallar la sesión: %v", err)
	}
	if !res.IsError {
		t.Fatal("se esperaba isError=true")
	}
}

// TestMCPCrash: el proceso muere durante la llamada → error + limpieza.
// El mock se cae a los 60ms; con un sleep de 300ms la respuesta nunca llega.
func TestMCPCrash(t *testing.T) {
	e := newTestExecutor(t, mockDef("inestable", "MOCK_CRASH=1", "MOCK_SLEEP_MS=300"))
	if _, err := e.Execute(context.Background(), "inestable", "buscar", nil); err == nil {
		t.Fatal("se esperaba error con MCP que se cae")
	}
	time.Sleep(500 * time.Millisecond)
	for _, pid := range pidsLanzados(t, "inestable") {
		if alive(pid) {
			t.Fatalf("proceso del MCP caído aún presente (%d)", pid)
		}
	}
}

// TestMCPDesconocido: nombre no registrado → error claro (punto 8).
func TestMCPDesconocido(t *testing.T) {
	e := newTestExecutor(t, mockDef("fake"))
	if _, err := e.Execute(context.Background(), "noexiste", "buscar", nil); err == nil {
		t.Fatal("se esperaba error con MCP desconocido")
	}
}

// TestTimeout: la herramienta tarda más que el tope → error (MCP_TIMEOUT).
func TestTimeout(t *testing.T) {
	e := newTestExecutor(t, mockDef("lento", "MOCK_SLEEP_MS=2000"))
	e.timeout = 500 * time.Millisecond // tope menor que el sleep del mock
	start := time.Now()
	if _, err := e.Execute(context.Background(), "lento", "buscar", nil); err == nil {
		t.Fatal("se esperaba timeout")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout tardó demasiado: %v", time.Since(start))
	}
}
