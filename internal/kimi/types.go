package kimi

import (
	"encoding/json"
)

// Protocol version
const ProtocolVersion = "1.5"

// Message types
const (
	MessageTypeTurnBegin        = "TurnBegin"
	MessageTypeTurnEnd          = "TurnEnd"
	MessageTypeStepBegin        = "StepBegin"
	MessageTypeStepInterrupted  = "StepInterrupted"
	MessageTypeCompactionBegin  = "CompactionBegin"
	MessageTypeCompactionEnd    = "CompactionEnd"
	MessageTypeStatusUpdate     = "StatusUpdate"
	MessageTypeContentPart      = "ContentPart"
	MessageTypeToolCall         = "ToolCall"
	MessageTypeToolCallPart     = "ToolCallPart"
	MessageTypeToolResult       = "ToolResult"
	MessageTypeSubagentEvent    = "SubagentEvent"
	MessageTypeApprovalRequest  = "ApprovalRequest"
	MessageTypeApprovalResponse = "ApprovalResponse"
)

// Content part types
const (
	ContentPartTypeText     = "text"
	ContentPartTypeThink    = "think"
	ContentPartTypeImageURL = "image_url"
	ContentPartTypeAudioURL = "audio_url"
	ContentPartTypeVideoURL = "video_url"
)

// Approval responses
const (
	ApprovalRequestResponseApprove           = "approve"
	ApprovalRequestResponseApproveForSession = "approve_for_session"
	ApprovalRequestResponseReject            = "reject"
)

// Prompt result statuses
const (
	PromptResultStatusPending         = "pending"
	PromptResultStatusFinished        = "finished"
	PromptResultStatusCancelled       = "cancelled"
	PromptResultStatusMaxStepsReached = "max_steps_reached"
	PromptResultStatusUnexpectedEOF   = "unexpected_eof"
)

// MediaURL represents a URL to media content
type MediaURL struct {
	ID  string `json:"id,omitempty"`
	URL string `json:"url"`
}

// ContentPart represents a part of the message content
type ContentPart struct {
	Type      string   `json:"type"`
	Text      string   `json:"text,omitempty"`
	Think     string   `json:"think,omitempty"`
	Encrypted string   `json:"encrypted,omitempty"`
	ImageURL  MediaURL `json:"image_url,omitempty"`
	AudioURL  MediaURL `json:"audio_url,omitempty"`
	VideoURL  MediaURL `json:"video_url,omitempty"`
}

// Content represents message content
type Content struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	ContentParts []ContentPart `json:"content_parts,omitempty"`
}

// ToPromptInput converts Content to the format expected by kimi-cli's prompt method
// kimi-cli expects user_input to be either a string or a list of ContentPart
func (c Content) ToPromptInput() any {
	if c.Type == "text" {
		return c.Text
	}
	if len(c.ContentParts) > 0 {
		return c.ContentParts
	}
	return c.Text
}

// ToolCall represents a tool invocation
type ToolCall struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// ToolResult represents the result of a tool call
type ToolResult struct {
	ToolCallID  string `json:"tool_call_id"`
	ReturnValue struct {
		IsError bool            `json:"is_error"`
		Output  json.RawMessage `json:"output"`
		Message string          `json:"message"`
		Display []DisplayBlock  `json:"display"`
	} `json:"return_value"`
}

// DisplayBlockType represents the type of display block
type DisplayBlockType string

const (
	DisplayBlockTypeBrief   DisplayBlockType = "brief"
	DisplayBlockTypeDiff    DisplayBlockType = "diff"
	DisplayBlockTypeTodo    DisplayBlockType = "todo"
	DisplayBlockTypeShell   DisplayBlockType = "shell"
	DisplayBlockTypeUnknown DisplayBlockType = "unknown"
)

// TodoStatus represents the status of a todo item
type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusDone       TodoStatus = "done"
)

// TodoItem represents a single todo item
type TodoItem struct {
	Title  string     `json:"title"`
	Status TodoStatus `json:"status"`
}

// DisplayBlock represents rich output from tools
type DisplayBlock struct {
	Type     DisplayBlockType `json:"type"`
	Text     string           `json:"text,omitempty"`
	Path     string           `json:"path,omitempty"`
	OldText  string           `json:"old_text,omitempty"`
	NewText  string           `json:"new_text,omitempty"`
	Items    []TodoItem       `json:"items,omitempty"`
	Language string           `json:"language,omitempty"`
	Command  string           `json:"command,omitempty"`
	Data     json.RawMessage  `json:"data,omitempty"`
}

// TokenUsage represents token consumption
type TokenUsage struct {
	InputOther         int `json:"input_other"`
	Output             int `json:"output"`
	InputCacheRead     int `json:"input_cache_read"`
	InputCacheCreation int `json:"input_cache_creation"`
}

// StatusUpdate represents a status update event
type StatusUpdate struct {
	ContextUsage float64    `json:"context_usage,omitempty"`
	TokenUsage   TokenUsage `json:"token_usage,omitempty"`
	MessageID    string     `json:"message_id,omitempty"`
}

// ApprovalRequest represents a request for user approval
type ApprovalRequest struct {
	ID          string         `json:"id"`
	ToolCallID  string         `json:"tool_call_id"`
	Sender      string         `json:"sender"`
	Action      string         `json:"action"`
	Description string         `json:"description"`
	Display     []DisplayBlock `json:"display,omitempty"`
}

// ApprovalResponse represents a response to an approval request
type ApprovalResponse struct {
	RequestID string `json:"request_id"`
	Response  string `json:"response"`
}

// SubagentEvent represents a sub-agent event
type SubagentEvent struct {
	TaskToolCallID string          `json:"task_tool_call_id"`
	Event          json.RawMessage `json:"event"`
}

// PromptResult represents the final result of a turn
type PromptResult struct {
	Status string `json:"status"`
	Steps  int    `json:"steps,omitempty"`
}

// PromptParams represents parameters for a prompt request
// Note: UserInput should be serialized as either string or []ContentPart
type PromptParams struct {
	UserInput any `json:"user_input"`
}

// InitializeParams represents parameters for initialization
type InitializeParams struct {
	ProtocolVersion string         `json:"protocol_version"`
	Client          ClientInfo     `json:"client,omitempty"`
	ExternalTools   []ExternalTool `json:"external_tools,omitempty"`
}

// ClientInfo represents client information
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ServerInfo represents server information
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ExternalTool represents an external tool
type ExternalTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ExternalToolsResult represents the result of registering external tools
type ExternalToolsResult struct {
	Accepted []struct {
		Name string `json:"name"`
	} `json:"accepted"`
	Rejected []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// InitializeResult represents the result of initialization
type InitializeResult struct {
	ProtocolVersion string               `json:"protocol_version"`
	Server          ServerInfo           `json:"server"`
	SlashCommands   []SlashCommand       `json:"slash_commands"`
	ExternalTools   *ExternalToolsResult `json:"external_tools,omitempty"`
}

// SlashCommand represents a slash command
type SlashCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CancelParams represents parameters for a cancel request
type CancelParams struct{}

// CancelResult represents the result of a cancel request
type CancelResult struct{}

// ToolCallRequest represents a request to call an external tool
type ToolCallRequest struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCallResponse represents a response to a tool call request
type ToolCallResponse struct {
	ID     string         `json:"id"`
	Result ToolCallResult `json:"result"`
}

// ToolCallResult represents the result of a tool call
type ToolCallResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Event represents a wire protocol event
type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Request represents a wire protocol request
type Request struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Response represents a wire protocol response
type Response struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}
