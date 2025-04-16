package terminal

import (
	"os"
)

// Pty represents an active pseudo-terminal connection.
// It abstracts the underlying OS-specific PTY/ConPTY implementation.
type Pty interface {
	// Read reads data from the pseudo-terminal's output.
	Read(p []byte) (n int, err error)
	// Write writes data to the pseudo-terminal's input.
	Write(p []byte) (n int, err error)
	// Close closes the pseudo-terminal connection.
	Close() error
	// Resize changes the size of the pseudo-terminal.
	Resize(rows, cols uint16) error
	// Fd returns the file descriptor of the master side of the PTY.
	// This might be needed for certain operations (e.g., ioctl on Unix).
	Fd() uintptr
	// Tty returns the file associated with the PTY master.
	// Use this for Read/Write operations.
	Tty() *os.File

	// StartProcess starts the given command within the pseudo-terminal.
	StartProcess(name string, arg ...string) (*os.Process, error)
}

// pty handles pseudo-terminal (PTY/ConPTY) specific logic.