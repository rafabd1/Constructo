package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Controller defines the interface for managing and interacting with the underlying pseudo-terminal.
type Controller interface {
	// Start initializes and starts the pseudo-terminal with a specific shell command.
	Start(ctx context.Context, shellCmd string, args ...string) error
	// Stop terminates the pseudo-terminal and associated processes.
	Stop() error
	// Resize informs the pseudo-terminal about a change in the terminal window size.
	Resize(rows, cols uint16) error
	// Write sends data (user input) to the pseudo-terminal's input.
	// Close closes the writer, which typically signals the end of input to the PTY.
	io.WriteCloser // The *os.File returned by pty.Start implements io.WriteCloser
	// SendSignal sends an OS signal to the underlying process group in the PTY.
	SendSignal(sig os.Signal) error
	// SetOutput sets the destination writer for the terminal's output.
	SetOutput(output io.Writer)

	// TODO: Add method(s) to subscribe to or retrieve the monitored output stream
	// TODO: Consider adding methods for direct process interaction (e.g., SendSignal)
}

// PtyController implements the Controller interface using a pseudo-terminal.
type PtyController struct {
	ptyFile *os.File      // Master side of the PTY from creack/pty
	cmd     *exec.Cmd     // The shell command process
	monitor *Monitor      // Reads output from ptyFile
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
	if c.monitor != nil {
		// Monitor now expects an *os.File, need adjustment if monitor interface changes
		// Assuming monitor can handle the output setting directly
		c.monitor.SetOutput(output)
	}
}

// Start initializes and starts the pseudo-terminal with the given shell command.
func (c *PtyController) Start(ctx context.Context, shellCmd string, args ...string) error {
	c.mu.Lock()
	currentOutput := c.output
	c.mu.Unlock()

	c.mu.Lock()
	if c.cmd != nil || c.ptyFile != nil {
		c.mu.Unlock()
		return fmt.Errorf("terminal controller already started")
	}
	c.mu.Unlock()

	// Determine shell command
	var effectiveShell string
	var effectiveArgs []string

	if shellCmd != "" { // User explicitly provided a shell
		effectiveShell = shellCmd
		effectiveArgs = args
		log.Printf("Using explicitly provided shell: %s %v", effectiveShell, effectiveArgs)
	} else {
		// Attempt to detect default shell, but prefer bash if available in container
		detectedShell := defaultShell()
		if _, err := exec.LookPath("bash"); err == nil {
			effectiveShell = "bash" // Prefer bash if installed
			log.Println("Defaulting to bash for PTY session.")
		} else if detectedShell != "" {
			effectiveShell = detectedShell
			log.Printf("Defaulting to detected shell: %s", effectiveShell)
		} else {
			return fmt.Errorf("could not determine a suitable shell (bash not found and detection failed)")
		}
		
		// Set effective args based on user input or default to a robust REPL implementation
		if len(args) > 0 {
		    effectiveArgs = args
		} else {
		    // Create a simple but robust REPL that works even in restricted PTY environments
		    repl := `while true; do printf "> "; read -r cmd; [ -z "$cmd" ] && continue; eval "$cmd" || echo "Command exited with status $?"; done`
		    effectiveArgs = []string{"-c", repl}
		    log.Printf("Starting shell %s with custom REPL loop for stability.", effectiveShell)
		}
	}

	// Create the command
	log.Printf("Starting PTY with command: %s %v", effectiveShell, effectiveArgs)
	cmd := exec.CommandContext(ctx, effectiveShell, effectiveArgs...)

	// Start the command within a PTY.
	ptyF, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start pty with shell '%s': %w", effectiveShell, err)
	}

	// Re-acquire lock to update controller state safely
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if another Start raced and succeeded (unlikely but possible)
	if c.cmd != nil || c.ptyFile != nil {
		_ = ptyF.Close() // Clean up the newly created PTY
		_ = cmd.Process.Kill() // Attempt to kill the associated process
		return fmt.Errorf("terminal controller started concurrently")
	}

	c.cmd = cmd
	c.ptyFile = ptyF
	c.stopped = false

	// --- Monitor Setup ---
	// Pass the *os.File to the monitor
	c.monitor = NewMonitor(c.ptyFile, currentOutput)
	c.monitor.Start()

	// Goroutine to wait for the command to exit
	go c.waitCmd()

	return nil
}

// waitCmd waits for the command process to exit and handles cleanup.
func (c *PtyController) waitCmd() {
	// Use the Wait method of the *exec.Cmd wrapper
	if c.cmd == nil {
		log.Println("[WARN] waitCmd called with nil cmd")
		return
	}
	waitErr := c.cmd.Wait() // Wait on the *exec.Cmd wrapper
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
		if c.ptyFile != nil {
			_ = c.ptyFile.Close()
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
	if c.ptyFile != nil {
		if err := c.ptyFile.Close(); err != nil {
			firstErr = fmt.Errorf("failed to close pty: %w", err)
		}
		c.ptyFile = nil
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
func (c *PtyController) Resize(rows, cols uint16) error {
	c.mu.RLock()
	ptyF := c.ptyFile
	c.mu.RUnlock()

	if ptyF == nil {
		return fmt.Errorf("terminal not started")
	}
	// Use the helper function from the pty library
	ws := &pty.Winsize{Rows: rows, Cols: cols}
	return pty.Setsize(ptyF, ws)
}

// Write sends data (user input) to the pseudo-terminal's input.
func (c *PtyController) Write(p []byte) (n int, err error) {
	c.mu.RLock()
	ptyF := c.ptyFile
	c.mu.RUnlock()
	if ptyF == nil {
		return 0, fmt.Errorf("terminal not started")
	}
	// Call Write directly on the interface value
	return ptyF.Write(p)
}

// Close closes the writer side of the PTY.
// This typically signals EOF to the process running in the PTY.
func (c *PtyController) Close() error {
	c.mu.RLock()
	ptyF := c.ptyFile
	c.mu.RUnlock()

	if ptyF == nil {
		return fmt.Errorf("terminal not started")
	}
	// Call Close directly on the interface value
	return ptyF.Close()
}

// SendSignal sends an OS signal to the process running in the PTY.
// Note: On Unix-like systems, this typically sends the signal to the entire process group
// associated with the PTY, which is usually the desired behavior for signals like SIGINT.
func (c *PtyController) SendSignal(sig os.Signal) error {
	c.mu.RLock()
	cmdProc := c.cmd
	c.mu.RUnlock()

	if cmdProc == nil || cmdProc.Process == nil {
		return fmt.Errorf("terminal process not running")
	}

	// Signal the underlying process via the wrapper's Process field
	if err := cmdProc.Process.Signal(sig); err != nil {
		// Ignore "process already finished" error when sending signal
		if errors.Is(err, os.ErrProcessDone) || strings.Contains(err.Error(), "process already finished") {
			return nil
		}
		return fmt.Errorf("failed to send signal %v: %w", sig, err)
	}
	return nil
}

// Package terminal manages interaction with the terminal.