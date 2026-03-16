package kimi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ai "github.com/readmedotmd/kmagent.md/adapter"
	"github.com/readmedotmd/kmagent.md/internal/kimi"
)

// DefaultContextWindow is the default context window size in tokens
const DefaultContextWindow = 200_000

// maxQueueSize is the maximum number of messages that can be queued
const maxQueueSize = 100

// Sentinel errors
var (
	ErrAdapterRunning    = errors.New("adapter already running")
	ErrAdapterNotRunning = errors.New("adapter not running")
	ErrQueueFull         = errors.New("message queue is full")
)

// Compile-time interface checks
var (
	_ ai.Adapter              = (*KimiAdapter)(nil)
	_ ai.SessionProvider      = (*KimiAdapter)(nil)
	_ ai.SessionManager       = (*KimiAdapter)(nil)
	_ ai.HistoryProvider      = (*KimiAdapter)(nil)
	_ ai.HistoryClearer       = (*KimiAdapter)(nil)
	_ ai.PermissionResponder  = (*KimiAdapter)(nil)
	_ ai.ExternalToolRegistry = (*KimiAdapter)(nil)
)

// trackedMessage records a message for history
type trackedMessage struct {
	role    string
	content string
}

// externalToolRecord stores a tool and its handler
type externalToolRecord struct {
	tool    ai.ExternalTool
	handler ai.ToolHandler
}

type KimiAdapter struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	status ai.AdapterStatus
	events chan ai.StreamEvent
	done   chan struct{}
	config ai.AdapterConfig

	// SDK client
	client    kimi.Client
	sessionID string
	workDir   string

	// Queue and per-run cancellation
	running   bool
	runCancel context.CancelFunc
	queue     []ai.Message

	// Conversation tracking
	history         []trackedMessage
	estimatedTokens int
	contextWindow   int

	// External tools
	tools map[string]externalToolRecord

	// Status callbacks
	statusCallbacks []func(ai.AdapterStatus)
}

// NewKimiAdapter creates a new Kimi adapter
func NewKimiAdapter() *KimiAdapter {
	return &KimiAdapter{
		status:        ai.StatusIdle,
		tools:         make(map[string]externalToolRecord),
		contextWindow: DefaultContextWindow,
	}
}

// Start initializes the adapter
func (k *KimiAdapter) Start(ctx context.Context, cfg ai.AdapterConfig) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.status == ai.StatusRunning {
		return ErrAdapterRunning
	}

	k.config = cfg
	k.events = make(chan ai.StreamEvent, 64)
	k.done = make(chan struct{})
	k.workDir = cfg.WorkDir

	// Build client options
	opts := []kimi.Option{
		kimi.WithWorkDir(cfg.WorkDir),
	}

	if cfg.Model != "" {
		opts = append(opts, kimi.WithModel(cfg.Model))
	}

	if cfg.SessionID != "" {
		opts = append(opts, kimi.WithSessionID(cfg.SessionID))
		k.sessionID = cfg.SessionID
	}

	// Map permission mode to yolo mode
	if cfg.PermissionMode == ai.PermissionAcceptAll {
		opts = append(opts, kimi.WithYoloMode(true))
	}

	// Set up environment
	env := make(map[string]string)
	for k, v := range cfg.Env {
		env[k] = v
	}
	if cfg.MaxThinkingTokens > 0 {
		env["KIMI_MAX_THINKING_TOKENS"] = fmt.Sprintf("%d", cfg.MaxThinkingTokens)
	}
	opts = append(opts, kimi.WithEnv(env))

	client := kimi.NewClient(opts...)
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("kimi sdk connect: %w", err)
	}
	k.client = client

	// Forward client errors
	k.wg.Add(1)
	go k.forwardClientErrors(client)

	// Process incoming messages
	k.wg.Add(1)
	go k.processIncomingMessages(client)

	// Register external tools if provided
	for _, tool := range cfg.ExternalTools {
		k.tools[tool.Name] = externalToolRecord{tool: tool}
	}

	if cfg.ContextWindow > 0 {
		k.contextWindow = cfg.ContextWindow
	}

	k.setStatus(ai.StatusRunning)
	return nil
}

func (k *KimiAdapter) forwardClientErrors(client kimi.Client) {
	defer k.wg.Done()
	for err := range client.ReceiveErrors() {
		k.emit(ai.StreamEvent{
			Type:      ai.EventError,
			Error:     fmt.Errorf("transport: %w", err),
			Timestamp: time.Now(),
		})
	}
}

func (k *KimiAdapter) processIncomingMessages(client kimi.Client) {
	defer k.wg.Done()
	for event := range client.ReceiveMessages() {
		select {
		case <-k.done:
			return
		default:
		}

		k.handleEvent(event)
	}
}

func (k *KimiAdapter) handleEvent(event kimi.Event) {
	switch event.Type {
	case kimi.MessageTypeTurnBegin:
		var payload struct {
			UserInput kimi.Content `json:"user_input"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return
		}
		// Track user message
		text := contentToText(payload.UserInput)
		k.mu.Lock()
		k.history = append(k.history, trackedMessage{role: "user", content: text})
		k.estimatedTokens += estimateTokens(text)
		k.mu.Unlock()

	case kimi.MessageTypeContentPart:
		var part kimi.ContentPart
		if err := json.Unmarshal(event.Payload, &part); err != nil {
			return
		}
		k.handleContentPart(part)

	case kimi.MessageTypeToolCall:
		var toolCall kimi.ToolCall
		if err := json.Unmarshal(event.Payload, &toolCall); err != nil {
			return
		}
		k.emit(ai.StreamEvent{
			Type:       ai.EventToolUse,
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Function.Name,
			ToolInput:  toolCall.Function.Arguments,
			ToolStatus: ai.ToolRunning,
			Timestamp:  time.Now(),
		})

		// Check if this is an external tool
		k.mu.Lock()
		toolRecord, isExternal := k.tools[toolCall.Function.Name]
		k.mu.Unlock()

		if isExternal && toolRecord.handler != nil {
			k.handleExternalToolCall(toolCall, toolRecord)
		}

	case kimi.MessageTypeToolResult:
		var result kimi.ToolResult
		if err := json.Unmarshal(event.Payload, &result); err != nil {
			return
		}
		status := ai.ToolComplete
		if result.ReturnValue.IsError {
			status = ai.ToolFailed
		}
		k.emit(ai.StreamEvent{
			Type:       ai.EventToolResult,
			ToolCallID: result.ToolCallID,
			ToolOutput: result.ReturnValue.Output,
			ToolStatus: status,
			Timestamp:  time.Now(),
		})

		// Handle display blocks if present
		for _, block := range result.ReturnValue.Display {
			k.emit(ai.StreamEvent{
				Type:         ai.EventDisplayBlock,
				DisplayBlock: convertDisplayBlock(block),
				Timestamp:    time.Now(),
			})
		}

	case kimi.MessageTypeStatusUpdate:
		var status kimi.StatusUpdate
		if err := json.Unmarshal(event.Payload, &status); err != nil {
			return
		}
		k.emit(ai.StreamEvent{
			Type: ai.EventCostUpdate,
			Usage: &ai.TokenUsage{
				InputTokens:  status.TokenUsage.InputOther + status.TokenUsage.InputCacheRead,
				OutputTokens: status.TokenUsage.Output,
				CacheRead:    status.TokenUsage.InputCacheRead,
				CacheWrite:   status.TokenUsage.InputCacheCreation,
			},
			Timestamp: time.Now(),
		})

	case kimi.MessageTypeApprovalRequest:
		var req kimi.ApprovalRequest
		if err := json.Unmarshal(event.Payload, &req); err != nil {
			return
		}
		k.emit(ai.StreamEvent{
			Type: ai.EventPermissionRequest,
			Permission: &ai.PermissionRequest{
				ToolCallID:  req.ToolCallID,
				ToolName:    req.Action,
				Description: req.Description,
			},
			Timestamp: time.Now(),
		})

	case kimi.MessageTypeCompactionBegin:
		k.emit(ai.StreamEvent{
			Type:      ai.EventCompactionBegin,
			Timestamp: time.Now(),
			Compaction: &ai.CompactionInfo{
				Reason: "context_limit",
			},
		})

	case kimi.MessageTypeCompactionEnd:
		k.emit(ai.StreamEvent{
			Type:      ai.EventCompactionEnd,
			Timestamp: time.Now(),
			Compaction: &ai.CompactionInfo{
				Reason: "context_limit",
			},
		})

	case kimi.MessageTypeStepBegin:
		var step struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(event.Payload, &step); err != nil {
			return
		}
		k.emit(ai.StreamEvent{
			Type:      ai.EventStepBegin,
			Timestamp: time.Now(),
			Step: &ai.StepInfo{
				StepNumber: step.N,
				TotalSteps: -1,
			},
		})

	case kimi.MessageTypeTurnEnd:
		var result struct {
			Result kimi.PromptResult `json:"result"`
		}
		if err := json.Unmarshal(event.Payload, &result); err != nil {
			return
		}
		k.emit(ai.StreamEvent{
			Type:      ai.EventStepEnd,
			Timestamp: time.Now(),
			Step: &ai.StepInfo{
				StepNumber: 1,
				TotalSteps: 1,
			},
		})
		k.emit(ai.StreamEvent{
			Type:      ai.EventDone,
			Timestamp: time.Now(),
		})

	case kimi.MessageTypeSubagentEvent:
		var sub kimi.SubagentEvent
		if err := json.Unmarshal(event.Payload, &sub); err != nil {
			return
		}
		k.emit(ai.StreamEvent{
			Type:      ai.EventSubAgent,
			Timestamp: time.Now(),
			SubAgent: &ai.SubAgentEvent{
				AgentID: sub.TaskToolCallID,
				Status:  ai.SubAgentStarted,
			},
		})
	}
}

func (k *KimiAdapter) handleContentPart(part kimi.ContentPart) {
	switch part.Type {
	case kimi.ContentPartTypeText:
		k.emit(ai.StreamEvent{
			Type:      ai.EventToken,
			Token:     part.Text,
			Timestamp: time.Now(),
		})
	case kimi.ContentPartTypeThink:
		k.emit(ai.StreamEvent{
			Type:      ai.EventThinking,
			Thinking:  part.Think,
			Timestamp: time.Now(),
		})
	case kimi.ContentPartTypeImageURL:
		// Images are handled differently in Kimi - they're URLs
		log.Printf("kimi: received image URL: %s", part.ImageURL.URL)
	case kimi.ContentPartTypeAudioURL:
		log.Printf("kimi: received audio URL: %s", part.AudioURL.URL)
	case kimi.ContentPartTypeVideoURL:
		log.Printf("kimi: received video URL: %s", part.VideoURL.URL)
	}
}

func (k *KimiAdapter) handleExternalToolCall(toolCall kimi.ToolCall, toolRecord externalToolRecord) {
	k.emit(ai.StreamEvent{
		Type:       ai.EventExternalToolCall,
		Timestamp:  time.Now(),
		ToolCallID: toolCall.ID,
		ToolName:   toolCall.Function.Name,
	})

	// Execute the handler
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := toolRecord.handler(ctx, []byte(toolCall.Function.Arguments))
		if err != nil {
			// Send error result
			k.client.SendToolResult(ctx, toolCall.ID, fmt.Sprintf("Error: %v", err), true)
			return
		}

		// Convert result to string
		var output string
		switch v := result.(type) {
		case string:
			output = v
		default:
			data, _ := json.Marshal(v)
			output = string(data)
		}

		k.client.SendToolResult(ctx, toolCall.ID, output, false)
	}()
}

func (k *KimiAdapter) emit(event ai.StreamEvent) bool {
	select {
	case <-k.done:
		return false
	case k.events <- event:
		return true
	}
}

func (k *KimiAdapter) setStatus(status ai.AdapterStatus) {
	k.status = status
	for _, fn := range k.statusCallbacks {
		fn(status)
	}
}

// Send sends a message to the adapter
func (k *KimiAdapter) Send(ctx context.Context, msg ai.Message, opts ...ai.SendOption) error {
	k.mu.Lock()
	if k.status != ai.StatusRunning {
		k.mu.Unlock()
		return ErrAdapterNotRunning
	}

	if k.running {
		if len(k.queue) >= maxQueueSize {
			k.mu.Unlock()
			return ErrQueueFull
		}
		k.queue = append(k.queue, msg)
		log.Printf("kimi: queued message (queue_len=%d)", len(k.queue))
		k.mu.Unlock()
		return nil
	}

	k.running = true
	runCtx, cancel := context.WithCancel(ctx)
	k.runCancel = cancel
	k.wg.Add(1)
	k.mu.Unlock()

	go k.runLoop(runCtx, msg)
	return nil
}

func (k *KimiAdapter) runLoop(ctx context.Context, msg ai.Message) {
	defer k.wg.Done()
	defer func() {
		k.mu.Lock()
		k.running = false
		k.runCancel = nil
		k.mu.Unlock()
	}()

	for {
		k.runKimi(ctx, msg)

		k.mu.Lock()
		if len(k.queue) == 0 {
			k.mu.Unlock()
			return
		}
		msg = combineMessages(k.queue)
		k.queue = nil
		if k.runCancel != nil {
			k.runCancel()
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		k.runCancel = cancel2
		ctx = ctx2
		k.mu.Unlock()

		log.Printf("kimi: processing queued message")
	}
}

func (k *KimiAdapter) runKimi(ctx context.Context, msg ai.Message) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Convert message to Kimi content
	content := messageToContent(msg)

	if err := k.client.Prompt(ctx, content); err != nil {
		log.Printf("kimi: prompt error: %v", err)
		k.emit(ai.StreamEvent{
			Type:      ai.EventError,
			Error:     err,
			Timestamp: time.Now(),
		})
	}
}

// Cancel cancels the in-progress run
func (k *KimiAdapter) Cancel() error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if !k.running {
		return nil
	}

	k.queue = nil
	if k.runCancel != nil {
		k.runCancel()
	}

	// Also send cancel to the client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = k.client.Cancel(ctx)

	log.Printf("kimi: cancelled in-progress run")
	return nil
}

// Receive returns the event channel
func (k *KimiAdapter) Receive() <-chan ai.StreamEvent {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.events
}

// Stop stops the adapter
func (k *KimiAdapter) Stop() error {
	k.mu.Lock()
	if k.status != ai.StatusRunning {
		k.mu.Unlock()
		return nil
	}
	close(k.done)
	if k.runCancel != nil {
		k.runCancel()
	}
	if k.client != nil {
		k.client.Disconnect()
	}
	k.setStatus(ai.StatusStopped)
	k.mu.Unlock()

	k.wg.Wait()
	close(k.events)
	return nil
}

// Status returns the current status
func (k *KimiAdapter) Status() ai.AdapterStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.status
}

// Capabilities returns what this adapter supports
func (k *KimiAdapter) Capabilities() ai.AdapterCapabilities {
	return ai.AdapterCapabilities{
		SupportsStreaming:     true,
		SupportsImages:        true,
		SupportsFiles:         true,
		SupportsToolUse:       true,
		SupportsMCP:           false, // Kimi CLI doesn't support MCP yet
		SupportsThinking:      true,
		SupportsCancellation:  true,
		SupportsHistory:       true,
		SupportsSubAgents:     true,
		SupportsExternalTools: true,
		SupportsDisplayBlocks: true,
		MaxContextWindow:      DefaultContextWindow,
		SupportedModels:       []string{"kimi-k2-thinking", "kimi-k2-turbo", "kimi-latest"},
	}
}

// Health checks the adapter health
func (k *KimiAdapter) Health(_ context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch k.status {
	case ai.StatusRunning:
		return nil
	case ai.StatusError:
		return &ai.AdapterError{Code: ai.ErrCrashed, Message: "adapter in error state"}
	case ai.StatusStopped:
		return &ai.AdapterError{Code: ai.ErrCrashed, Message: "adapter stopped"}
	default:
		return &ai.AdapterError{Code: ai.ErrUnknown, Message: "adapter not started"}
	}
}

// SessionID returns the current session ID
func (k *KimiAdapter) SessionID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sessionID
}

// ListSessions lists all sessions for the workspace
func (k *KimiAdapter) ListSessions(ctx context.Context) ([]ai.SessionInfo, error) {
	// Kimi stores sessions in ~/.kimi/sessions/{workDirHash}/
	sessionsDir := filepath.Join(os.Getenv("HOME"), ".kimi", "sessions")

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []ai.SessionInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Read session metadata
		metaPath := filepath.Join(sessionsDir, entry.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}

		var meta struct {
			WorkDir   string `json:"work_dir"`
			UpdatedAt int64  `json:"updated_at"`
			Brief     string `json:"brief"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}

		sessions = append(sessions, ai.SessionInfo{
			ID:        entry.Name(),
			WorkDir:   meta.WorkDir,
			UpdatedAt: meta.UpdatedAt,
			Brief:     meta.Brief,
		})
	}

	return sessions, nil
}

// ResumeSession resumes a previous session
func (k *KimiAdapter) ResumeSession(ctx context.Context, sessionID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.status != ai.StatusRunning {
		return ErrAdapterNotRunning
	}

	k.sessionID = sessionID
	// The next Prompt will use this session ID
	return nil
}

// DeleteSession deletes a session
func (k *KimiAdapter) DeleteSession(ctx context.Context, sessionID string) error {
	sessionsDir := filepath.Join(os.Getenv("HOME"), ".kimi", "sessions")
	sessionDir := filepath.Join(sessionsDir, sessionID)

	if err := os.RemoveAll(sessionDir); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	return nil
}

// GetHistory returns the conversation history
func (k *KimiAdapter) GetHistory(ctx context.Context) ([]ai.Message, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var msgs []ai.Message
	for _, tm := range k.history {
		role := ai.RoleUser
		if tm.role == "assistant" {
			role = ai.RoleAssistant
		}
		msgs = append(msgs, ai.Message{
			Role:      role,
			Content:   ai.TextContent(tm.content),
			Timestamp: time.Now(),
		})
	}
	return msgs, nil
}

// ClearHistory clears the conversation history
func (k *KimiAdapter) ClearHistory(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.history = nil
	k.estimatedTokens = 0
	return nil
}

// RespondPermission responds to a permission request
func (k *KimiAdapter) RespondPermission(ctx context.Context, toolCallID string, response ai.ApprovalResponse) error {
	var kimiResponse string
	switch response {
	case ai.ApprovalResponseApprove:
		kimiResponse = kimi.ApprovalRequestResponseApprove
	case ai.ApprovalResponseApproveForSession:
		kimiResponse = kimi.ApprovalRequestResponseApproveForSession
	case ai.ApprovalResponseReject:
		kimiResponse = kimi.ApprovalRequestResponseReject
	default:
		kimiResponse = kimi.ApprovalRequestResponseApprove
	}

	return k.client.RespondApproval(ctx, toolCallID, kimiResponse)
}

// RegisterTool registers an external tool
func (k *KimiAdapter) RegisterTool(ctx context.Context, tool ai.ExternalTool, handler ai.ToolHandler) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.status != ai.StatusRunning {
		return ErrAdapterNotRunning
	}

	k.tools[tool.Name] = externalToolRecord{
		tool:    tool,
		handler: handler,
	}

	// Re-initialize with new tools
	// This would require re-connecting to the CLI with updated external tools
	log.Printf("kimi: registered external tool %s", tool.Name)
	return nil
}

// UnregisterTool unregisters an external tool
func (k *KimiAdapter) UnregisterTool(ctx context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	delete(k.tools, name)
	log.Printf("kimi: unregistered external tool %s", name)
	return nil
}

// ListTools lists all registered external tools
func (k *KimiAdapter) ListTools(ctx context.Context) ([]ai.ExternalTool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var tools []ai.ExternalTool
	for _, record := range k.tools {
		tools = append(tools, record.tool)
	}
	return tools, nil
}

// HasTool returns true if a tool is registered
func (k *KimiAdapter) HasTool(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.tools[name]
	return ok
}

// OnStatusChange registers a status change callback
func (k *KimiAdapter) OnStatusChange(fn func(ai.AdapterStatus)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.statusCallbacks = append(k.statusCallbacks, fn)
}

// Helper functions

func combineMessages(msgs []ai.Message) ai.Message {
	if len(msgs) == 1 {
		return msgs[0]
	}
	var parts []string
	for _, m := range msgs {
		parts = append(parts, messageToText(m))
	}
	return ai.Message{
		ID:        msgs[len(msgs)-1].ID,
		Role:      ai.RoleUser,
		Content:   ai.TextContent(strings.Join(parts, "\n\n")),
		Timestamp: msgs[len(msgs)-1].Timestamp,
	}
}

func messageToText(msg ai.Message) string {
	var parts []string
	for _, block := range msg.Content {
		if block.Type == ai.ContentText && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func messageToContent(msg ai.Message) kimi.Content {
	var parts []kimi.ContentPart
	for _, block := range msg.Content {
		switch block.Type {
		case ai.ContentText:
			parts = append(parts, kimi.ContentPart{
				Type: kimi.ContentPartTypeText,
				Text: block.Text,
			})
		case ai.ContentImage:
			// Convert to data URL
			if len(block.Data) > 0 {
				mimeType := block.MimeType
				if mimeType == "" {
					mimeType = "image/png"
				}
				dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(block.Data))
				parts = append(parts, kimi.ContentPart{
					Type:     kimi.ContentPartTypeImageURL,
					ImageURL: kimi.MediaURL{URL: dataURL},
				})
			}
		case ai.ContentCode:
			parts = append(parts, kimi.ContentPart{
				Type: kimi.ContentPartTypeText,
				Text: fmt.Sprintf("```%s\n%s\n```", block.Language, block.Text),
			})
		}
	}

	if len(parts) == 0 {
		return kimi.Content{Type: "text", Text: ""}
	}
	if len(parts) == 1 && parts[0].Type == kimi.ContentPartTypeText {
		return kimi.Content{Type: "text", Text: parts[0].Text}
	}
	return kimi.Content{
		Type:         "content_parts",
		ContentParts: parts,
	}
}

func contentToText(content kimi.Content) string {
	if content.Type == "text" {
		return content.Text
	}
	var parts []string
	for _, part := range content.ContentParts {
		if part.Type == kimi.ContentPartTypeText {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "")
}

func estimateTokens(text string) int {
	return len([]rune(text)) / 4
}

func convertDisplayBlock(block kimi.DisplayBlock) *ai.DisplayBlock {
	db := &ai.DisplayBlock{
		Type:     ai.DisplayBlockType(block.Type),
		Text:     block.Text,
		Path:     block.Path,
		OldText:  block.OldText,
		NewText:  block.NewText,
		Language: block.Language,
		Command:  block.Command,
	}

	for _, item := range block.Items {
		db.Items = append(db.Items, ai.TodoItem{
			Title:  item.Title,
			Status: ai.TodoStatus(item.Status),
		})
	}

	return db
}
