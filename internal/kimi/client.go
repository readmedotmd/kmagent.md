package kimi

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// Client provides bidirectional streaming communication with Kimi CLI
type Client interface {
	Connect(ctx context.Context) error
	Disconnect() error
	Prompt(ctx context.Context, content Content) error
	RespondApproval(ctx context.Context, requestID string, response string) error
	Cancel(ctx context.Context) error
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

// Connect establishes a connection to the Kimi CLI
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
func (c *clientImpl) Prompt(ctx context.Context, content Content) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	params := PromptParams{
		UserInput: content,
	}

	return c.transport.SendMessage(ctx, "prompt", params)
}

// RespondApproval responds to an approval request
func (c *clientImpl) RespondApproval(ctx context.Context, requestID string, response string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	approval := ApprovalResponse{
		RequestID: requestID,
		Response:  response,
	}

	return c.transport.SendNotification(ctx, "approval/response", approval)
}

// Cancel cancels the current turn
func (c *clientImpl) Cancel(ctx context.Context) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	return c.transport.SendNotification(ctx, "cancel", CancelParams{})
}

// SendToolResult sends the result of an external tool call
func (c *clientImpl) SendToolResult(ctx context.Context, toolCallID string, content string, isError bool) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return fmt.Errorf("client not connected")
	}

	result := ToolCallResponse{
		ID: toolCallID,
		Result: ToolCallResult{
			Content: content,
			IsError: isError,
		},
	}

	return c.transport.SendNotification(ctx, "tool/result", result)
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
