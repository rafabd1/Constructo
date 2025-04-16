package terminal

import (
	"fmt"
	"io"
	"sync"
)

// Monitor reads from a Pty output and distributes it.
type Monitor struct {
	pty    Pty       // The pseudo-terminal to monitor
	output io.Writer // Where to write the output (can be changed)
	mu     sync.Mutex
	done   chan struct{} // Channel to signal stopping
}

// NewMonitor creates a new terminal monitor.
func NewMonitor(pty Pty, output io.Writer) *Monitor {
	return &Monitor{
		pty:    pty,
		output: output,
		done:   make(chan struct{}),
	}
}

// SetOutput changes the destination writer for the monitored output.
// This method is safe for concurrent use.
func (m *Monitor) SetOutput(output io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.output = output
}

// Start begins monitoring the Pty output in a separate goroutine.
func (m *Monitor) Start() {
	go m.run()
}

// Stop signals the monitor to stop reading.
func (m *Monitor) Stop() {
	close(m.done)
}

// run is the main loop for reading from the Pty and writing to output.
func (m *Monitor) run() {
	buf := make([]byte, 32*1024) // Example buffer size
	for {
		select {
		case <-m.done:
			return // Stop requested
		default:
			n, err := m.pty.Read(buf) // Read from PTY
			if n > 0 {
				m.mu.Lock() // Lock before accessing/writing to output
				output := m.output
				m.mu.Unlock()

				if output != nil { // Check if output is set
					if _, writeErr := output.Write(buf[:n]); writeErr != nil {
						// Handle output write error (logging recommended)
						// Consider if we should stop monitoring on write error
						fmt.Printf("Monitor write error: %v\n", writeErr) // Replace with logger
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					// Handle read error (logging recommended)
					fmt.Printf("Monitor read error: %v\n", err) // Replace with logger
				}
				return // Exit loop on error or EOF
			}
		}
	}
}