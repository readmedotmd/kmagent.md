package kimi

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// ClientCapabilities represents capabilities declared by the Wire client during initialization
type ClientCapabilities struct {
	SupportsQuestion  bool `json:"supports_question,omitempty"`
	SupportsPlanMode  bool `json:"supports_plan_mode,omitempty"`
}

// Client provides bidirectional streaming communication with Kimi CLI
type Client interface {
	Connect(ctx context.Context) error
	Disconnect() error
	Prompt(ctx context.Context, content Content) error
	// RespondApproval responds to an approval request from the server
	// The requestID should be the ID from the ApprovalRequest event
	RespondApproval(ctx context.Context, requestID string, response string) error
	// RespondQuestion responds to a question request from the server
	// The requestID should be the ID from the QuestionRequest event
	// Answers is a map from question text to selected option label(s)
	RespondQuestion(ctx context.Context, requestID string, answers map[string]string) error
	Cancel(ctx context.Context) error
	// SendToolResult sends the result of an external tool call
	// The toolCallID should be the ID from the ToolCallRequest event
	SendToolResult(ctx context.Context, toolCallID string, content string, isError bool) error
	ReceiveMessages() <-chan Event
	ReceiveErrors() <-chan error
}

// clientImpl implements the Client interface
type clientImpl struct {
	mu        sync.RWMutex
	transport *Transport
	options   *Options
	connected bool
}

// NewClient creates a new Client
func NewClient(opts ...Option) Client {
	options := NewOptions(opts...)
	return &clientImpl{options: options}
}

// Connect establishes a connection to the Kimi CLI and sends initialize request
func (c *clientImpl) Connect(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Find CLI
	cliPath := "kimi"
	if _, err := exec.LookPath(cliPath); err != nil {
		return fmt.Errorf("kimi CLI not found in PATH: %w", err)
	}

	c.transport = NewTransport(cliPath, c.options)

	if err := c.transport.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect transport: %w", err)
	}

	c.connected = true

	// Send initialize request to kimi-cli
	// This is required before any other operation
	initParams := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Client: ClientInfo{
			Name:    "kmagent.md",
			Version: "0.0.1",
		},
	}
	
	if err := c.transport.SendMessage(ctx, "initialize", initParams); err != nil {
		c.transport.Close()
		c.connected = false
		c.transport = nil
		return fmt.Errorf("failed to send initialize request: %w", err)
	}

	return nil
}

// Disconnect closes the connection
func (c *clientImpl) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.transport != nil && c.connected {
		if err := c.transport.Close(); err != nil {
			return fmt.Errorf("failed to close transport: %w", err)
		}
	}
	c.connected = false
	c.transport = nil
	return nil
}

// Prompt sends a prompt to the CLI
// Note: kimi-cli expects user_input to be either a string or a list of ContentPart
func (c *clientImpl) Prompt(ctx context.Context, content Content) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	params := PromptParams{
		UserInput: content.ToPromptInput(),
	}

	return c.transport.SendMessage(ctx, "prompt", params)
}

// RespondApproval responds to an approval request
// The requestID is the ID from the ApprovalRequest event (which matches the JSON-RPC request id)
// The response should be "approve", "approve_for_session", or "reject"
func (c *clientImpl) RespondApproval(ctx context.Context, requestID string, response string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	// Send as a JSON-RPC response (not a notification)
	// The server sends ApprovalRequest as a JSON-RPC request with method="request"
	// and expects us to reply with a JSON-RPC response containing the result
	result := map[string]string{
		"request_id": requestID,
		"response":   response,
	}

	return c.transport.SendResponse(ctx, requestID, result)
}

// RespondQuestion responds to a question request
// The requestID is the ID from the QuestionRequest event (which matches the JSON-RPC request id)
// Answers is a map from question text to selected option label(s)
func (c *clientImpl) RespondQuestion(ctx context.Context, requestID string, answers map[string]string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	// Send as a JSON-RPC response (not a notification)
	// The server sends QuestionRequest as a JSON-RPC request with method="request"
	// and expects us to reply with a JSON-RPC response containing the result
	result := map[string]any{
		"request_id": requestID,
		"answers":    answers,
	}

	return c.transport.SendResponse(ctx, requestID, result)
}

// Cancel cancels the current turn
func (c *clientImpl) Cancel(ctx context.Context) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	// Cancel is a request (expects a response), not a notification
	return c.transport.SendMessage(ctx, "cancel", struct{}{})
}

// SendToolResult sends the result of an external tool call
// The toolCallID is the ID from the ToolCallRequest event (which matches the JSON-RPC request id)
func (c *clientImpl) SendToolResult(ctx context.Context, toolCallID string, content string, isError bool) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	// Tool result is sent as a JSON-RPC response (not a notification)
	// The server sends ToolCallRequest as a JSON-RPC request with method="request"
	// and expects us to reply with a JSON-RPC response containing the result
	result := map[string]any{
		"id": toolCallID,
		"result": map[string]any{
			"content":  content,
			"is_error": isError,
		},
	}

	return c.transport.SendResponse(ctx, toolCallID, result)
}

// ReceiveMessages returns the message channel
func (c *clientImpl) ReceiveMessages() <-chan Event {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		ch := make(chan Event)
		close(ch)
		return ch
	}
	return c.transport.ReceiveMessages()
}

// ReceiveErrors returns the error channel
func (c *clientImpl) ReceiveErrors() <-chan error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		ch := make(chan error)
		close(ch)
		return ch
	}
	return c.transport.ReceiveErrors()
}
