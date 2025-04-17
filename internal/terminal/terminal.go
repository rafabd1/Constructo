package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	// "os/exec" // Keep commented out for now, only os.Signal is used
	"strings"
	"sync"

	// Explicitly name the imported package
	pty "github.com/aymanbagabas/go-pty"
)

// Controller defines the interface for managing and interacting with the underlying pseudo-terminal.
type Controller interface {
	// Start initializes and starts the pseudo-terminal with a specific shell command.
	Start(ctx context.Context, shellCmd string, args ...string) error
	// Stop terminates the pseudo-terminal and associated processes.
	Stop() error
	// Resize(rows, cols uint16) error // TODO: Re-add Resize functionality if needed and feasible
	// Write sends data (user input) to the pseudo-terminal's input.
	// Close closes the writer, which typically signals the end of input to the PTY.
	io.WriteCloser // The pty.Pty interface includes io.WriteCloser
	// SendSignal sends an OS signal to the underlying process group in the PTY.
	SendSignal(sig os.Signal) error
	// SetOutput sets the destination writer for the terminal's output.
	SetOutput(output io.Writer)

	// TODO: Add method(s) to subscribe to or retrieve the monitored output stream
	// TODO: Consider adding methods for direct process interaction (e.g., SendSignal)
}

// PtyController implements the Controller interface using a pseudo-terminal.
type PtyController struct {
	pty     pty.Pty      // Use the interface type directly
	cmd     *pty.Cmd      // Store the wrapper command from the library
	monitor *Monitor      // Reads output from pty
	mu      sync.RWMutex  // Protects access to internal state
	output  io.Writer     // Destination for monitored output (can be nil)
	stopped bool
}

// NewPtyController creates a new pseudo-terminal controller.
func NewPtyController() *PtyController {
	return &PtyController{}
}

// SetOutput sets the destination writer for terminal output.
// If the terminal is already started, it updates the monitor's output.
// It is safe for concurrent use.
func (c *PtyController) SetOutput(output io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.output = output
	// If monitor exists, update its output writer as well
	if c.monitor != nil {
		c.monitor.SetOutput(output)
	}
}

// Start initializes and starts the pseudo-terminal with the given shell command.
// If shellCmd is empty, it attempts to use the default system shell.
func (c *PtyController) Start(ctx context.Context, shellCmd string, args ...string) error {
	c.mu.Lock()
	// Capture output under lock before unlocking
	currentOutput := c.output
	// Unlock early to avoid holding lock during pty.Start
	c.mu.Unlock()

	// Re-lock for state check and modification (briefly)
	c.mu.Lock()
	if c.cmd != nil || c.pty != nil {
		c.mu.Unlock()
		return fmt.Errorf("terminal controller already started")
	}
	c.mu.Unlock() // Unlock before potentially long-running operations

	// Determine shell command if not provided
	if shellCmd == "" {
		shellCmd = defaultShell()
		if shellCmd == "" {
			return fmt.Errorf("could not determine default shell")
		}
	}

	// Create the pty instance first
	newPty, err := pty.New()
	if err != nil {
		return fmt.Errorf("failed to create pty: %w", err)
	}

	// Create the command associated with the pty
	cmdWrapper := newPty.CommandContext(ctx, shellCmd, args...)

	// Start the command
	if err = cmdWrapper.Start(); err != nil {
		_ = newPty.Close() // Clean up PTY if command fails to start
		return fmt.Errorf("failed to start command in pty: %w", err)
	}

	// Re-acquire lock to update controller state safely
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if another Start raced and succeeded (unlikely but possible)
	if c.cmd != nil || c.pty != nil {
		_ = newPty.Close()
		if cmdWrapper.Process != nil {
			_ = cmdWrapper.Process.Kill()
		}
		return fmt.Errorf("terminal controller started concurrently")
	}

	c.cmd = cmdWrapper
	c.pty = newPty
	c.stopped = false

	// --- Monitor Setup ---
	// Pass the pty.Pty interface directly to the monitor
	c.monitor = NewMonitor(c.pty, currentOutput)
	c.monitor.Start()

	// Goroutine to wait for the command to exit
	go c.waitCmd()

	return nil
}

// waitCmd waits for the command process to exit and handles cleanup.
func (c *PtyController) waitCmd() {
	// Use the Wait method of the *pty.Cmd wrapper
	if c.cmd == nil {
		log.Println("[WARN] waitCmd called with nil cmd wrapper")
		return
	}
	waitErr := c.cmd.Wait() // Wait on the *pty.Cmd wrapper
	if waitErr != nil {
		// Log the reason why the command exited (can be normal exit, signal, error)
		log.Printf("[INFO] Terminal command process exited: %v", waitErr)
	} else {
		log.Println("[INFO] Terminal command process exited cleanly.")
	}

	c.mu.Lock()
	if !c.stopped {
		log.Println("[INFO] waitCmd initiating cleanup due to unexpected process exit.")
		c.stopped = true
		if c.monitor != nil {
			c.monitor.Stop()
		}
		if c.pty != nil {
			_ = c.pty.Close()
		}
		// TODO: Maybe signal agent loop that terminal stopped?
	}
	c.mu.Unlock()
}

// Stop terminates the pseudo-terminal and associated processes.
func (c *PtyController) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return nil // Already stopped
	}
	c.stopped = true

	var firstErr error

	if c.monitor != nil {
		c.monitor.Stop()
	}

	// Close the PTY. This should signal the process.
	if c.pty != nil {
		if err := c.pty.Close(); err != nil {
			firstErr = fmt.Errorf("failed to close pty: %w", err)
		}
		c.pty = nil
	}

	// Attempt to kill the underlying process via the wrapper's Process field.
	if c.cmd != nil && c.cmd.Process != nil {
		// Check if process already exited (non-blocking check if possible, os-specific)
		// For simplicity, we just try to kill.
		if err := c.cmd.Process.Kill(); err != nil {
			// Ignore "process already finished" errors.
			if !errors.Is(err, os.ErrProcessDone) && !strings.Contains(err.Error(), "process already finished") {
				killErr := fmt.Errorf("failed to kill process: %w", err)
				if firstErr == nil {
					firstErr = killErr
				} else {
					// Log the subsequent error if desired
					fmt.Printf("Additional error during stop: %v\n", killErr) // Replace with proper logging
				}
			}
		}
		c.cmd = nil
	}

	return firstErr
}

// Resize informs the pseudo-terminal about a change in the terminal window size.
// Resize functionality removed temporarily
// func (c *PtyController) Resize(rows, cols uint16) error {
// 	c.mu.RLock()
// 	currentPty := c.pty
// 	c.mu.RUnlock()
//
// 	if currentPty == nil {
// 		return fmt.Errorf("terminal not started")
// 	}
// 	// TODO: Investigate how to get *os.File or use pty.Setsize correctly
// 	// return currentPty.SetWinSize(int(rows), int(cols)) // This method doesn't exist on the interface
// 	return fmt.Errorf("Resize not implemented with current PTY library setup")
// }

// Write sends data (user input) to the pseudo-terminal's input.
func (c *PtyController) Write(p []byte) (n int, err error) {
	c.mu.RLock()
	currentPty := c.pty
	c.mu.RUnlock()
	if currentPty == nil {
		return 0, fmt.Errorf("terminal not started")
	}
	// Call Write directly on the interface value
	return currentPty.Write(p)
}

// Close closes the writer side of the PTY.
// This typically signals EOF to the process running in the PTY.
func (c *PtyController) Close() error {
	c.mu.RLock()
	currentPty := c.pty
	c.mu.RUnlock()

	if currentPty == nil {
		return fmt.Errorf("terminal not started")
	}
	// Call Close directly on the interface value
	return currentPty.Close()
}

// SendSignal sends an OS signal to the process running in the PTY.
// Note: On Unix-like systems, this typically sends the signal to the entire process group
// associated with the PTY, which is usually the desired behavior for signals like SIGINT.
func (c *PtyController) SendSignal(sig os.Signal) error {
	c.mu.RLock()
	cmdWrapper := c.cmd
	c.mu.RUnlock()

	if cmdWrapper == nil || cmdWrapper.Process == nil {
		return fmt.Errorf("terminal process not running or accessible")
	}

	// Signal the underlying process via the wrapper's Process field
	if err := cmdWrapper.Process.Signal(sig); err != nil {
		// Ignore "process already finished" error when sending signal
		if errors.Is(err, os.ErrProcessDone) || strings.Contains(err.Error(), "process already finished") {
			return nil
		}
		return fmt.Errorf("failed to send signal %v: %w", sig, err)
	}
	return nil
}

// Package terminal manages interaction with the terminal.