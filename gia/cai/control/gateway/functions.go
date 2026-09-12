package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// ========== OPENAI TOOL STRUCTURES ==========

type Tool struct {
	Type     string   `json:"type"` // "function"
	Function Function `json:"function"`
}

type Function struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ========== BUILD PROMPT WITH TOOLS ==========

func BuildPromptWithTools(messages []Message, tools []Tool) string {
	var prompt strings.Builder

	// 1. System messages
	for _, msg := range messages {
		if msg.Role == "system" {
			prompt.WriteString(msg.Content + "\n\n")
		}
	}

	// 2. Tool instructions
	if len(tools) > 0 {
		prompt.WriteString("Tienes acceso a las siguientes herramientas:\n\n")
		for _, tool := range tools {
			prompt.WriteString(fmt.Sprintf("### %s\n%s\n", tool.Function.Name, tool.Function.Description))
			if params, ok := tool.Function.Parameters["properties"].(map[string]interface{}); ok {
				prompt.WriteString("Parámetros:\n")
				for key, value := range params {
					if paramObj, ok := value.(map[string]interface{}); ok {
						typeStr, _ := paramObj["type"].(string)
						descStr, _ := paramObj["description"].(string)
						prompt.WriteString(fmt.Sprintf("  - %s (%s): %s\n", key, typeStr, descStr))
					}
				}
			}
			prompt.WriteString("\n")
		}

		prompt.WriteString(`Para usar una herramienta, responde EXACTAMENTE en este formato JSON (sin texto adicional antes o después):
{"tool_call": {"name": "nombre_herramienta", "arguments": {"param1": "valor1"}}}

Si no necesitas usar una herramienta, responde normalmente.
`)
	}

	// 3. Conversation messages
	for _, msg := range messages {
		if msg.Role == "user" || msg.Role == "assistant" {
			prompt.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
		}
	}

	return prompt.String()
}

// ========== DETECT TOOL CALL ==========

func DetectToolCall(response string) *ToolCall {
	trimmed := strings.TrimSpace(response)
	log.Printf("[DetectToolCall] response=%q", trimmed[:min(200, len(trimmed))])

	start := strings.Index(trimmed, `{"tool_call"`)
	if start == -1 {
		// Try alternate format: {"tool_call":{"name":...
		start = strings.Index(trimmed, `{"tool_call":`)
	}
	if start == -1 {
		log.Printf("[DetectToolCall] no tool_call found")
		return nil
	}

	depth := 0
	end := -1
	for i := start; i < len(trimmed); i++ {
		if trimmed[i] == '{' {
			depth++
		} else if trimmed[i] == '}' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}

	if end == -1 {
		return nil
	}

	jsonStr := trimmed[start:end]

	var wrapper struct {
		ToolCall struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		} `json:"tool_call"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return nil
	}

	if wrapper.ToolCall.Name == "" {
		return nil
	}

	argsJSON, _ := json.Marshal(wrapper.ToolCall.Arguments)

	return &ToolCall{
		ID:   fmt.Sprintf("call_%d", time.Now().UnixNano()),
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Name:      wrapper.ToolCall.Name,
			Arguments: string(argsJSON),
		},
	}
}

// ========== REGISTERED TOOLS ==========

var registeredToolDefs = []Tool{
	{
		Type: "function",
		Function: Function{
			Name:        "get_system_status",
			Description: "Obtiene el estado actual del sistema de proxies y servicios",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	},
	{
		Type: "function",
		Function: Function{
			Name:        "list_active_proxies",
			Description: "Lista los proxies actualmente activos y disponibles",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	},
	{
		Type: "function",
		Function: Function{
			Name:        "rotate_proxy",
			Description: "Cambia al siguiente proxy disponible en la rotación",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	},
}

var registeredToolFns = map[string]func(map[string]interface{}) string{
	"get_system_status":   getSystemStatus,
	"list_active_proxies": listActiveProxies,
	"rotate_proxy":        rotateProxyTool,
}

func getSystemStatus(args map[string]interface{}) string {
	if proxyManager == nil {
		return `{"status":"error","message":"Proxy manager not initialized"}`
	}
	status := proxyManager.GetStatus()
	proxies := proxyManager.GetProxies()

	activeCount := 0
	for _, p := range proxies {
		if p.IsActive {
			activeCount++
		}
	}

	return fmt.Sprintf(`{"status":"ok","proxies_total":%d,"proxies_active":%d,"failover_count":%d,"auth":true}`,
		len(proxies), activeCount, status.FailoverCount)
}

func listActiveProxies(args map[string]interface{}) string {
	if proxyManager == nil {
		return "[]"
	}
	proxies := proxyManager.GetProxies()

	var result []string
	for _, p := range proxies {
		if p.IsActive {
			result = append(result, fmt.Sprintf("%s:%s (%s, %s)", p.Host, p.Port, p.Protocol, p.IPType))
		}
	}

	if len(result) == 0 {
		return "[]"
	}
	return "[" + strings.Join(result, ", ") + "]"
}

func rotateProxyTool(args map[string]interface{}) string {
	if proxyManager == nil {
		return "Proxy manager not initialized"
	}
	proxies := proxyManager.GetProxies()
	for _, p := range proxies {
		if p.IsActive {
			return fmt.Sprintf("Active proxy: %s:%s (%s)", p.Host, p.Port, p.IPType)
		}
	}
	return "No active proxies available"
}

// ExecuteToolCall executes a registered tool and returns the result
func ExecuteToolCall(tc *ToolCall) string {
	fn, ok := registeredToolFns[tc.Function.Name]
	if !ok {
		return fmt.Sprintf(`{"error":"Unknown tool: %s"}`, tc.Function.Name)
	}

	var args map[string]interface{}
	if tc.Function.Arguments != "" {
		json.Unmarshal([]byte(tc.Function.Arguments), &args)
	}

	return fn(args)
}
