package ai_adapters

import "time"

// Role represents the sender of a message
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// ContentType identifies the kind of content in a block.
type ContentType string

const (
	ContentText       ContentType = "text"
	ContentCode       ContentType = "code"
	ContentImage      ContentType = "image"
	ContentFile       ContentType = "file"
	ContentToolUse    ContentType = "tool_use"
	ContentToolResult ContentType = "tool_result"
)

// ToolCall describes a single tool invocation embedded in a message.
type ToolCall struct {
	ID     string
	Name   string
	Input  any
	Output any
	Status ToolStatusValue
}

// ContentBlock is one part of a multi-part message.
type ContentBlock struct {
	Type     ContentType
	Text     string
	Language string // for ContentCode
	Data     []byte // for binary content (images, files)
	MimeType string
	ToolCall *ToolCall
}

// Message represents a single message in a conversation.
//
// Security: Do not store secrets (API keys, tokens, passwords) in Metadata.
// Metadata may be persisted, logged, or transmitted to external systems by
// adapter implementations.
type Message struct {
	ID        string
	Role      Role
	Content   []ContentBlock
	Timestamp time.Time
	Metadata  map[string]string
}

// TextContent is a convenience constructor for a simple text message.
func TextContent(text string) []ContentBlock {
	return []ContentBlock{{Type: ContentText, Text: text}}
}

// Conversation represents a series of messages with a specific adapter.
type Conversation struct {
	ID        string
	Messages  []Message
	Adapter   string
	CreatedAt time.Time
	UpdatedAt time.Time
	Title     string
	Metadata  map[string]string
}
