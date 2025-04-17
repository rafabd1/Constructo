package terminal

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// Monitor reads from a Pty output and distributes it.
type Monitor struct {
	pty    *os.File    // Expecting the *os.File from creack/pty
	output io.Writer   // Where to write the output (can be changed)
	mu     sync.Mutex
	done   chan struct{} // Channel to signal stopping
}

// NewMonitor creates a new terminal monitor.
func NewMonitor(ptyFile *os.File, output io.Writer) *Monitor {
	return &Monitor{
		pty:    ptyFile, // Store the *os.File
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

// run is the main loop for reading from the Pty (*os.File) and writing to output.
func (m *Monitor) run() {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-m.done:
			return // Stop requested
		default:
			n, err := m.pty.Read(buf)
			if n > 0 {
				log.Printf("[Monitor Raw Read]: %q", string(buf[:n]))
				m.mu.Lock()
				output := m.output
				m.mu.Unlock()

				if output != nil {
					if _, writeErr := output.Write(buf[:n]); writeErr != nil {
						fmt.Printf("Monitor write error: %v\n", writeErr)
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					fmt.Printf("Monitor read error: %v\n", err)
				}
				return // Exit loop on error or EOF
			}
		}
	}
}