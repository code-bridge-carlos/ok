// mockmcp es un servidor MCP (stdio) simulado para los tests de s4.
// Cumple el mínimo del protocolo MCP y permite inducir fallos vía entorno:
//
//	MOCK_PIDFILE   ruta donde escribir el PID (para validar reuso de sesión)
//	MOCK_SLEEP_MS  milisegundos de espera antes de responder tools/call
//	MOCK_FAIL_TOOL si =1, tools/call devuelve isError=true
//	MOCK_CRASH     si =1, el proceso termina tras el handshake
//
// Uso (en tests): go build -o mockmcp ./cmd/mockmcp y lanzar como comando MCP.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func reply(id int, result any) error {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	return emit(msg)
}

func emit(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(data))
	return err
}

func main() {
	if pf := os.Getenv("MOCK_PIDFILE"); pf != "" {
		f, err := os.OpenFile(pf, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintln(f, os.Getpid())
			_ = f.Close()
		}
	}
	sleepMS, _ := strconv.Atoi(os.Getenv("MOCK_SLEEP_MS"))
	failTool := os.Getenv("MOCK_FAIL_TOOL") == "1"
	if os.Getenv("MOCK_CRASH") == "1" {
		// Simula un MCP que se cae solo: termina poco después del arranque.
		time.AfterFunc(60*time.Millisecond, func() { os.Exit(3) })
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		var msg rpcMessage
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.ID == nil {
			continue // notificación: ignorar
		}
		switch msg.Method {
		case "initialize":
			_ = reply(*msg.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mockmcp", "version": "1.0"},
			})
		case "tools/list":
			_ = reply(*msg.ID, map[string]any{
				"tools": []map[string]any{
					{"name": "buscar", "description": "búsqueda simulada", "inputSchema": map[string]any{"type": "object"}},
					{"name": "saludar", "description": "saludo", "inputSchema": map[string]any{"type": "object"}},
				},
			})
		case "tools/call":
			if sleepMS > 0 {
				time.Sleep(time.Duration(sleepMS) * time.Millisecond)
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			if failTool {
				_ = reply(*msg.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "fallo simulado de la herramienta"}},
					"isError": true,
				})
				continue
			}
			argTexto, _ := json.Marshal(params.Arguments)
			_ = reply(*msg.ID, map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": "resultado de " + params.Name + " con args " + string(argTexto),
				}},
				"isError": false,
			})
		case "ping":
			_ = reply(*msg.ID, map[string]any{})
		}
	}
}
