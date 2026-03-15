package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *os.File

	msgChan chan Event
	errChan chan error

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
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

	t.wg.Add(1)
	go t.handleStdout()

	t.connected = true
	return nil
}

// SendMessage sends a message to the CLI
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

	request := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	t.writeMu.Lock()
	_, err = t.stdin.Write(append(data, '\n'))
	t.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	return nil
}

// SendNotification sends a notification (no response expected)
func (t *Transport) SendNotification(ctx context.Context, method string, params any) error {
	return t.SendMessage(ctx, method, params)
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
	defer close(t.msgChan)
	defer close(t.errChan)

	scanner := bufio.NewScanner(t.stdout)
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

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

		// Parse JSON-RPC message
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
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
			event := Event{
				Type:    msg.Method,
				Payload: msg.Params,
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
