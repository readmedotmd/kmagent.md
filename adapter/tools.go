package ai_adapters

import (
	"context"
	"encoding/json"
)

// ExternalTool represents a custom tool that can be registered with the adapter.
// When the model calls this tool, the adapter will invoke the registered handler.
type ExternalTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema for tool parameters
}

// ToolHandler is the function signature for external tool implementations.
// The input is the JSON-marshalled tool arguments.
// The output can be any JSON-serializable type, or a string, or an error.
type ToolHandler func(ctx context.Context, input json.RawMessage) (any, error)

// ToolResult represents the result of a tool execution.
type ToolResult struct {
	IsError bool   `json:"is_error"`
	Output  string `json:"output"`
	Message string `json:"message"`
}

// ExternalToolRegistry is an optional interface for adapters that support
// runtime registration of external tools.
type ExternalToolRegistry interface {
	// RegisterTool registers a new external tool.
	// If a tool with the same name already exists, it will be replaced.
	RegisterTool(ctx context.Context, tool ExternalTool, handler ToolHandler) error

	// UnregisterTool removes a previously registered external tool.
	UnregisterTool(ctx context.Context, name string) error

	// ListTools returns all currently registered external tools.
	ListTools(ctx context.Context) ([]ExternalTool, error)

	// HasTool returns true if a tool with the given name is registered.
	HasTool(name string) bool
}

// ToolCallRequest represents a request from the model to call an external tool.
type ToolCallRequest struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolCallResponse represents the response to a tool call.
type ToolCallResponse struct {
	ToolCallID string     `json:"tool_call_id"`
	Result     ToolResult `json:"result"`
}

// CreateTextResult creates a simple text tool result.
func CreateTextResult(text string) ToolResult {
	return ToolResult{
		IsError: false,
		Output:  text,
		Message: "Success",
	}
}

// CreateErrorResult creates a tool result representing an error.
func CreateErrorResult(errMsg string) ToolResult {
	return ToolResult{
		IsError: true,
		Output:  errMsg,
		Message: "Error",
	}
}
