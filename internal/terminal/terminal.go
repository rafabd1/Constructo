package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	io.WriteCloser
	// SendSignal sends an OS signal to the underlying process group in the PTY.
	SendSignal(sig os.Signal) error
	// SetOutput sets the destination writer for the terminal's output.
	SetOutput(output io.Writer)

	// TODO: Add method(s) to subscribe to or retrieve the monitored output stream
	// TODO: Consider adding methods for direct process interaction (e.g., SendSignal)
}

// PtyController implements the Controller interface using a pseudo-terminal.
type PtyController struct {
	ptyFile *os.File      // Master side of the PTY
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
	if c.cmd != nil || c.ptyFile != nil {
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

	// Create the command
	cmd := exec.CommandContext(ctx, shellCmd, args...)

	// Start the command within a PTY.
	var ptyF *os.File
	var err error
	ptyF, err = pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start pty: %w", err)
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
	ptyWrapper := &osFilePtyWrapper{file: c.ptyFile}
	// Initialize and start the monitor with the currently set output
	c.monitor = NewMonitor(ptyWrapper, currentOutput)
	c.monitor.Start()

	// Goroutine to wait for the command to exit
	go c.waitCmd()

	return nil
}

// waitCmd waits for the command process to exit and handles cleanup.
func (c *PtyController) waitCmd() {
	// Wait for the command to finish execution
	// This error is expected when the process exits normally or is killed.
	_ = c.cmd.Wait()

	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		// If the command exits unexpectedly, ensure PTY and monitor are stopped.
		if c.monitor != nil {
			c.monitor.Stop()
		}
		if c.ptyFile != nil {
			_ = c.ptyFile.Close() // Best effort close
		}
		// TODO: Signal that the terminal has stopped unexpectedly?
	}
	c.mu.Unlock()
}

// osFilePtyWrapper adapts an *os.File (from creack/pty) to our Pty interface.
// This is a temporary measure; a more robust implementation might be needed.
type osFilePtyWrapper struct {
	file *os.File
}

func (w *osFilePtyWrapper) Read(p []byte) (n int, err error) {
	return w.file.Read(p)
}

func (w *osFilePtyWrapper) Write(p []byte) (n int, err error) {
	return w.file.Write(p)
}

func (w *osFilePtyWrapper) Close() error {
	return w.file.Close()
}

func (w *osFilePtyWrapper) Resize(rows, cols uint16) error {
	ws := &pty.Winsize{Rows: rows, Cols: cols}
	return pty.Setsize(w.file, ws)
}

func (w *osFilePtyWrapper) Fd() uintptr {
	return w.file.Fd()
}

func (w *osFilePtyWrapper) Tty() *os.File {
	return w.file
}

// StartProcess is not applicable for this simple wrapper as the process is started externally.
func (w *osFilePtyWrapper) StartProcess(name string, arg ...string) (*os.Process, error) {
	return nil, fmt.Errorf("StartProcess not implemented on osFilePtyWrapper")
}

// --- Implement other Controller methods (Stop, Resize, Write, Close) --- TODO

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

	// Close the PTY file first. This often signals the child process to terminate.
	if c.ptyFile != nil {
		if err := c.ptyFile.Close(); err != nil {
			firstErr = fmt.Errorf("failed to close pty: %w", err)
		}
		c.ptyFile = nil
	}

	// Attempt to kill the process directly if it hasn't exited.
	if c.cmd != nil && c.cmd.Process != nil {
		// Check if process already exited (non-blocking check if possible, os-specific)
		// For simplicity, we just try to kill.
		if err := c.cmd.Process.Kill(); err != nil {
			// Ignore "process already finished" errors.
			if !errors.Is(err, os.ErrProcessDone) {
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
	ptyF := c.ptyFile // Capture under lock
	c.mu.RUnlock()

	if ptyF == nil {
		return fmt.Errorf("terminal not started")
	}
	// Use the helper function from the pty library with the correct struct
	ws := &pty.Winsize{Rows: rows, Cols: cols}
	return pty.Setsize(ptyF, ws)
}

// Write sends data (user input) to the pseudo-terminal's input.
func (c *PtyController) Write(p []byte) (n int, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ptyFile == nil {
		return 0, fmt.Errorf("terminal not started")
	}
	return c.ptyFile.Write(p)
}

// Close closes the writer side of the PTY.
// This typically signals EOF to the process running in the PTY.
func (c *PtyController) Close() error {
	c.mu.RLock()
	pty := c.ptyFile // Capture the file pointer under lock
	c.mu.RUnlock()

	if pty == nil {
		return fmt.Errorf("terminal not started")
	}
	// Closing the master side of the PTY acts as closing the input stream
	// for the child process.
	return pty.Close()
}

// SendSignal sends an OS signal to the process running in the PTY.
// Note: On Unix-like systems, this typically sends the signal to the entire process group
// associated with the PTY, which is usually the desired behavior for signals like SIGINT.
func (c *PtyController) SendSignal(sig os.Signal) error {
	c.mu.RLock()
	cmdProc := c.cmd // Capture under lock
	c.mu.RUnlock()

	if cmdProc == nil || cmdProc.Process == nil {
		return fmt.Errorf("terminal process not running")
	}

	if err := cmdProc.Process.Signal(sig); err != nil {
		// Ignore "process already finished" error when sending signal
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("failed to send signal %v: %w", sig, err)
	}
	return nil
}

// Package terminal manages interaction with the terminal.