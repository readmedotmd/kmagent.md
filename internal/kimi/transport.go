package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	channelBufferSize      = 100
	terminationTimeoutSecs = 5
	maxScanTokenSize       = 1024 * 1024 // 1MB
)

// Transport handles communication with the Kimi CLI
type Transport struct {
	cmd     *exec.Cmd
	cliPath string
	options *Options

	connected bool
	mu        sync.RWMutex
	writeMu   sync.Mutex

	workersWg sync.WaitGroup

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *os.File

	msgChan chan Event
	errChan chan error

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Request ID generator (atomic counter)
	nextID atomic.Int64

	// readyCh is closed when the first stdout message is received
	readyCh chan struct{}
	// startErr captures stderr output if the process crashes during startup
	startErr atomic.Value // stores string
}

// NewTransport creates a new transport
func NewTransport(cliPath string, options *Options) *Transport {
	return &Transport{
		cliPath: cliPath,
		options: options,
	}
}

// Connect establishes a connection to the Kimi CLI
func (t *Transport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.connected {
		return fmt.Errorf("transport already connected")
	}

	args := []string{"--wire"}
	if t.options != nil {
		if t.options.WorkDir != "" {
			args = append(args, "--work-dir", t.options.WorkDir)
		}
		if t.options.Model != "" {
			args = append(args, "--model", t.options.Model)
		}
		if t.options.Thinking {
			args = append(args, "--thinking")
		}
		if t.options.YoloMode {
			args = append(args, "--yolo")
		}
		if t.options.SessionID != "" {
			args = append(args, "--session-id", t.options.SessionID)
		}
	}

	t.cmd = exec.CommandContext(ctx, t.cliPath, args...)

	// Environment
	env := os.Environ()
	env = append(env, "KIMI_ENTRYPOINT=adapter")
	if t.options != nil && t.options.Env != nil {
		for k, v := range t.options.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	t.cmd.Env = env

	// Working directory
	if t.options != nil && t.options.WorkDir != "" {
		t.cmd.Dir = t.options.WorkDir
	}

	// I/O pipes
	var err error
	t.stdin, err = t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	t.stdout, err = t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Stderr handling
	stderrFile, err := os.CreateTemp("", "kimi_stderr_*.log")
	if err != nil {
		return fmt.Errorf("failed to create stderr file: %w", err)
	}
	if err := stderrFile.Chmod(0o600); err != nil {
		_ = stderrFile.Close()
		_ = os.Remove(stderrFile.Name())
		return fmt.Errorf("failed to set stderr file permissions: %w", err)
	}
	t.stderr = stderrFile
	t.cmd.Stderr = t.stderr

	if err := t.cmd.Start(); err != nil {
		t.cleanup()
		return fmt.Errorf("failed to start Kimi CLI: %w", err)
	}

	t.ctx, t.cancel = context.WithCancel(ctx)
	t.msgChan = make(chan Event, channelBufferSize)
	t.errChan = make(chan error, channelBufferSize)

	t.readyCh = make(chan struct{})

	t.workersWg.Add(2)
	t.wg.Add(3)
	go t.handleStdout()
	go t.monitorProcess()
	go func() {
		defer t.wg.Done()
		t.workersWg.Wait()
		close(t.msgChan)
		close(t.errChan)
	}()

	t.connected = true
	return nil
}

// generateRequestID generates a unique string ID for JSON-RPC requests
func (t *Transport) generateRequestID() string {
	return fmt.Sprintf("%d", t.nextID.Add(1))
}

// SendMessage sends a JSON-RPC request to the CLI (expects a response)
// Note: kimi-cli expects id to be a STRING, not an integer
func (t *Transport) SendMessage(ctx context.Context, method string, params any) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected || t.stdin == nil {
		return fmt.Errorf("transport not connected or stdin closed")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Generate unique string ID for this request (kimi-cli requires string IDs)
	id := t.generateRequestID()

	request := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Debug logging
	if os.Getenv("KIMI_ADAPTER_DEBUG") != "" {
		log.Printf("[KIMI_ADAPTER] SEND (request): %s", string(data))
	}

	t.writeMu.Lock()
	_, err = t.stdin.Write(append(data, '\n'))
	t.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	return nil
}

// SendResponse sends a JSON-RPC response (reply to a request from the server)
func (t *Transport) SendResponse(ctx context.Context, id string, result any) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected || t.stdin == nil {
		return fmt.Errorf("transport not connected or stdin closed")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	response := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Result  any    `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}

	data, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	// Debug logging
	if os.Getenv("KIMI_ADAPTER_DEBUG") != "" {
		log.Printf("[KIMI_ADAPTER] SEND (response): %s", string(data))
	}

	t.writeMu.Lock()
	_, err = t.stdin.Write(append(data, '\n'))
	t.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to write response: %w", err)
	}

	return nil
}

// SendNotification sends a JSON-RPC notification (no response expected, no id field)
func (t *Transport) SendNotification(ctx context.Context, method string, params any) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected || t.stdin == nil {
		return fmt.Errorf("transport not connected or stdin closed")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Notifications MUST NOT have an "id" field per JSON-RPC 2.0 spec
	notification := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	// Debug logging
	if os.Getenv("KIMI_ADAPTER_DEBUG") != "" {
		log.Printf("[KIMI_ADAPTER] SEND (notification): %s", string(data))
	}

	t.writeMu.Lock()
	_, err = t.stdin.Write(append(data, '\n'))
	t.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to write notification: %w", err)
	}

	return nil
}

// ReceiveMessages returns the message channel
func (t *Transport) ReceiveMessages() <-chan Event {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected {
		ch := make(chan Event)
		close(ch)
		return ch
	}
	return t.msgChan
}

// ReceiveErrors returns the error channel
func (t *Transport) ReceiveErrors() <-chan error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected {
		ch := make(chan error)
		close(ch)
		return ch
	}
	return t.errChan
}

// Interrupt sends an interrupt signal to the CLI
func (t *Transport) Interrupt() error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected || t.cmd == nil || t.cmd.Process == nil {
		return fmt.Errorf("process not running")
	}
	return t.cmd.Process.Signal(syscall.SIGINT)
}

// Close closes the transport
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.connected {
		return nil
	}
	t.connected = false

	if t.cancel != nil {
		t.cancel()
	}

	if t.stdin != nil {
		_ = t.stdin.Close()
		t.stdin = nil
	}

	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(terminationTimeoutSecs * time.Second):
	}

	var err error
	if t.cmd != nil && t.cmd.Process != nil {
		err = t.terminateProcess()
	}

	t.cleanup()
	return err
}

func (t *Transport) handleStdout() {
	defer t.wg.Done()
	defer t.workersWg.Done()

	scanner := bufio.NewScanner(t.stdout)
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	readySignaled := false

	for scanner.Scan() {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		// Signal readiness on first real output
		if !readySignaled {
			close(t.readyCh)
			readySignaled = true
		}

		// Debug logging
		if os.Getenv("KIMI_ADAPTER_DEBUG") != "" {
			log.Printf("[KIMI_ADAPTER] RECV: %s", line)
		}

		// Parse JSON-RPC message
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
			Error   *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}

		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			// Try to parse as an event
			var event Event
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				select {
				case t.errChan <- fmt.Errorf("failed to parse message: %w", err):
				case <-t.ctx.Done():
					return
				}
				continue
			}
			select {
			case t.msgChan <- event:
			case <-t.ctx.Done():
				return
			}
			continue
		}

		// Handle events (notifications with method)
		if msg.Method != "" {
			eventType := msg.Method
			eventPayload := msg.Params
			// Wire protocol v1.5+: events/requests are wrapped in a
			// {"type":"EventName","payload":{...}} envelope with method="event"
			// or method="request". Unwrap to get the actual event type.
			if eventType == "event" || eventType == "request" {
				var envelope struct {
					Type    string          `json:"type"`
					Payload json.RawMessage `json:"payload"`
				}
				if err := json.Unmarshal(msg.Params, &envelope); err == nil && envelope.Type != "" {
					eventType = envelope.Type
					eventPayload = envelope.Payload
				}
			}
			event := Event{
				Type:    eventType,
				Payload: eventPayload,
			}
			select {
			case t.msgChan <- event:
			case <-t.ctx.Done():
				return
			}
			continue
		}

		// Handle errors
		if msg.Error != nil {
			select {
			case t.errChan <- fmt.Errorf("RPC error %d: %s", msg.Error.Code, msg.Error.Message):
			case <-t.ctx.Done():
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case t.errChan <- fmt.Errorf("stdout scanner error: %w", err):
		case <-t.ctx.Done():
		}
	}
}

// WaitReady blocks until the first stdout message is received from the CLI,
// indicating that it has started successfully. If the context expires or the
// process crashes before producing output, an error is returned with any
// stderr diagnostics that were captured.
func (t *Transport) WaitReady(ctx context.Context) error {
	select {
	case <-t.readyCh:
		return nil
	case <-ctx.Done():
		// Process may have crashed — include stderr in the error
		if diagRaw := t.startErr.Load(); diagRaw != nil {
			if diag, ok := diagRaw.(string); ok && diag != "" {
				return fmt.Errorf("kimi CLI not ready: %w\nstderr: %s", ctx.Err(), diag)
			}
		}
		return fmt.Errorf("kimi CLI not ready: %w", ctx.Err())
	}
}

// monitorProcess waits for the CLI process to exit and captures stderr output
// so that startup failures are surfaced with diagnostic information.
func (t *Transport) monitorProcess() {
	defer t.wg.Done()
	defer t.workersWg.Done()

	if t.cmd == nil {
		return
	}

	err := t.cmd.Wait()
	if err == nil {
		return
	}

	// Process crashed — read stderr for diagnostics
	diag := t.readStderr()
	if diag != "" {
		t.startErr.Store(diag)
	}

	// Push the error onto errChan so consumers see it
	errMsg := fmt.Sprintf("kimi CLI exited unexpectedly: %v", err)
	if diag != "" {
		errMsg = fmt.Sprintf("%s\nstderr: %s", errMsg, diag)
	}
	select {
	case t.errChan <- fmt.Errorf("%s", errMsg):
	case <-t.ctx.Done():
	}
}

// readStderr reads any buffered stderr output from the temporary log file.
func (t *Transport) readStderr() string {
	if t.stderr == nil {
		return ""
	}

	name := t.stderr.Name()
	data, err := os.ReadFile(name)
	if err != nil {
		return ""
	}

	s := strings.TrimSpace(string(data))
	const maxStderrLen = 4096
	if len(s) > maxStderrLen {
		s = s[:maxStderrLen] + "... (truncated)"
	}
	return s
}

func (t *Transport) terminateProcess() error {
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}

	if err := t.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if isProcessFinished(err) {
			return nil
		}
		killErr := t.cmd.Process.Kill()
		if killErr != nil && !isProcessFinished(killErr) {
			return killErr
		}
		return nil
	}

	done := make(chan error, 1)
	cmd := t.cmd
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "signal:") {
			return nil
		}
		return err
	case <-time.After(terminationTimeoutSecs * time.Second):
		if killErr := t.cmd.Process.Kill(); killErr != nil && !isProcessFinished(killErr) {
			return killErr
		}
		<-done
		return nil
	case <-t.ctx.Done():
		if killErr := t.cmd.Process.Kill(); killErr != nil && !isProcessFinished(killErr) {
			return killErr
		}
		<-done
		return nil
	}
}

func (t *Transport) cleanup() {
	if t.stdout != nil {
		_ = t.stdout.Close()
		t.stdout = nil
	}
	if t.stderr != nil {
		_ = t.stderr.Close()
		_ = os.Remove(t.stderr.Name())
		t.stderr = nil
	}
	t.cmd = nil
}

func isProcessFinished(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "process already finished") ||
		strings.Contains(s, "process already released") ||
		strings.Contains(s, "no child processes") ||
		strings.Contains(s, "signal: killed")
}
