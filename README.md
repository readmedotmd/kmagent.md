# kmagent.md

A Go adapter that wraps **Kimi CLI** behind the standard **[agent.adapter.md](https://github.com/readmedotmd/agent.adapter.md)** interface.

## Features

- **Streaming Events**: Token-by-token events via `<-chan StreamEvent`
- **Tool Use**: Full tool call lifecycle with ID correlation
- **Extended Thinking**: Thinking blocks streamed as `EventThinking` events
- **Multi-modal**: Support for images, audio, and video URLs
- **External Tools**: Register custom Go functions as tools
- **Sessions**: Full session lifecycle management (list, resume, delete)
- **Context Compaction**: Automatic context management with visibility events
- **Permission Flow**: Approve, reject, or approve-for-session
- **Display Blocks**: Rich tool output (diffs, todos, shell commands)

## Installation

```bash
go get github.com/readmedotmd/kmagent.md
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"

    "github.com/readmedotmd/kmagent.md"
    ai "github.com/readmedotmd/kmagent.md/adapter"
)

func main() {
    ctx := context.Background()
    adapter := kimi.NewKimiAdapter()

    err := adapter.Start(ctx, ai.AdapterConfig{
        Name:           "my-kimi-agent",
        WorkDir:        ".",
        Model:          "kimi-k2-thinking",
        PermissionMode: ai.PermissionDefault,
    })
    if err != nil {
        panic(err)
    }
    defer adapter.Stop()

    // Send a message
    adapter.Send(ctx, ai.Message{
        Role:    ai.RoleUser,
        Content: ai.TextContent("What files are in this directory?"),
    })

    // Stream the response
    for ev := range adapter.Receive() {
        switch ev.Type {
        case ai.EventToken:
            fmt.Print(ev.Token)
        case ai.EventToolUse:
            fmt.Printf("\n[tool: %s]\n", ev.ToolName)
        case ai.EventThinking:
            fmt.Printf("\n[thinking: %s]\n", ev.Thinking)
        case ai.EventDone:
            fmt.Println()
            return
        case ai.EventError:
            fmt.Printf("\nerror: %v\n", ev.Error)
            return
        }
    }
}
```

## External Tools

Register custom Go functions as tools:

```go
// Define your tool
weatherTool := ai.ExternalTool{
    Name:        "get_weather",
    Description: "Get current weather for a location",
    Parameters: json.RawMessage(`{
        "type": "object",
        "properties": {
            "location": {"type": "string", "description": "City name"},
            "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]}
        },
        "required": ["location"]
    }`),
}

// Define the handler
weatherHandler := func(ctx context.Context, input json.RawMessage) (any, error) {
    var args struct {
        Location string `json:"location"`
        Unit     string `json:"unit"`
    }
    if err := json.Unmarshal(input, &args); err != nil {
        return nil, err
    }
    
    // Your implementation here
    return fmt.Sprintf("Weather in %s: 22°C Sunny", args.Location), nil
}

// Register with the adapter
if registry, ok := adapter.(ai.ExternalToolRegistry); ok {
    registry.RegisterTool(ctx, weatherTool, weatherHandler)
}
```

## Session Management

```go
// List sessions
if manager, ok := adapter.(ai.SessionManager); ok {
    sessions, _ := manager.ListSessions(ctx)
    for _, s := range sessions {
        fmt.Printf("Session: %s (updated: %d)\n", s.ID, s.UpdatedAt)
    }
    
    // Resume a session
    manager.ResumeSession(ctx, "session-id")
    
    // Delete a session
    manager.DeleteSession(ctx, "session-id")
}
```

## Permission Handling

```go
for ev := range adapter.Receive() {
    if ev.Type == ai.EventPermissionRequest {
        // Show user what the agent wants to do
        fmt.Printf("Approve %s: %s?\n", ev.Permission.ToolName, ev.Permission.Description)
        
        // Respond
        if responder, ok := adapter.(ai.PermissionResponder); ok {
            // Option 1: Approve once
            responder.RespondPermission(ctx, ev.Permission.ToolCallID, ai.ApprovalResponseApprove)
            
            // Option 2: Approve for entire session
            // responder.RespondPermission(ctx, ev.Permission.ToolCallID, ai.ApprovalResponseApproveForSession)
            
            // Option 3: Reject
            // responder.RespondPermission(ctx, ev.Permission.ToolCallID, ai.ApprovalResponseReject)
        }
    }
}
```

## Architecture

```
┌─────────────────────────────────────────────────┐
│                  Your Application                │
│                                                  │
│   adapter.Start()  adapter.Send()  adapter.Receive()
└──────────┬──────────────┬──────────────┬─────────┘
           │              │              │
    ┌──────▼──────────────▼──────────────▼─────────┐
│              KimiAdapter                        │
│   queue ─── runLoop ─── event emission          │
│   session tracking ─── external tools           │
└──────────────────┬─────────────────────────────┘
                   │
    ┌──────────────────▼─────────────────────────┐
│           internal/kimi                       │
│   Client ─── Transport ─── Wire Protocol      │
│   JSON-RPC over stdin/stdout                  │
└──────────────────┬────────────────────────────┘
                   │
    ┌──────────────────▼─────────────────────────┐
│            Kimi CLI                            │
│   kimi --wire                                  │
└────────────────────────────────────────────────┘
```

## License

MIT
