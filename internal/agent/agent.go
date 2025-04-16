package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/terminal"
	// Placeholder for LLM client, parser, etc.
	// "github.com/rafabd1/Constructo/pkg/llm"
	// "github.com/rafabd1/Constructo/internal/config"
)

const (
	outputDebounceDuration = 500 * time.Millisecond
	periodicUpdateInterval = 1 * time.Minute
)

// Agent manages the main interaction loop, terminal, and LLM communication.
type Agent struct {
	termController terminal.Controller
	cmdRegistry    *commands.Registry // Assuming commands are registered elsewhere
	// llmClient      llm.Client // Placeholder
	// parser         Parser     // Placeholder for LLM response parser

	userInputChan chan string      // Channel for user input lines
	termOutputBuf *bytes.Buffer    // Buffer to capture terminal output for the agent
	mu            sync.Mutex       // Protects agent state, especially termOutputBuf access
	lastOutput    time.Time        // Time of last output received from terminal
	outputTimer   *time.Timer      // Timer for debouncing output triggers
	stopChan      chan struct{}    // Channel to signal agent shutdown
	cancelContext context.CancelFunc // Context cancellation for background tasks
}

// NewAgent creates a new agent instance.
func NewAgent(registry *commands.Registry /* llmClient llm.Client, parser Parser */) *Agent {
	return &Agent{
		cmdRegistry:   registry,
		// llmClient:     llmClient,
		// parser:        parser,
		userInputChan: make(chan string, 1), // Buffered slightly
		termOutputBuf: &bytes.Buffer{},
		stopChan:      make(chan struct{}),
	}
}

// Run starts the main agent loop.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	a.cancelContext = cancel
	defer cancel()

	// 1. Initialize Terminal Controller
	a.termController = terminal.NewPtyController()

	// 2. Setup MultiWriter for terminal output
	// Output goes to user (os.Stdout) and agent buffer (a.termOutputBuf)
	// We need a thread-safe way to access the buffer.
	outputWriter := NewSynchronizedBufferWriter(a.termOutputBuf, &a.mu, a.handleTerminalOutput)
	multiOut := io.MultiWriter(os.Stdout, outputWriter)
	a.termController.SetOutput(multiOut)

	// 3. Start the terminal (using default shell for now)
	if err := a.termController.Start(ctx, "", ""); err != nil {
		return fmt.Errorf("failed to start terminal controller: %w", err)
	}
	defer a.termController.Stop()

	// 4. Start reading user input in a separate goroutine
	go a.readUserInput()

	// 5. Start periodic update ticker
	periodicTicker := time.NewTicker(periodicUpdateInterval)
	defer periodicTicker.Stop()

	fmt.Println("Constructo Agent Started. Type your requests or /help.")

	// 6. Main select loop
	for {
		select {
		case <-ctx.Done():
			fmt.Println("Agent context cancelled. Shutting down...")
			return ctx.Err()

		case <-a.stopChan:
			fmt.Println("Agent shutdown requested.")
			return nil

		case line := <-a.userInputChan:
			if a.handleUserInput(ctx, line) {
				// If user input triggers LLM or internal command, process immediately
				// (handleUserInput calls processAgentTurn if needed)
			} else {
				// Otherwise, send directly to terminal PTY
				_, err := a.termController.Write([]byte(line + "\n"))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error writing to terminal: %v\n", err)
					// Consider how to handle this error (e.g., inform user/agent)
				}
			}

		case <-a.getOutputTimerChannel(): // Debounced output trigger
			fmt.Println("[Debug] Output timer fired")
			a.processAgentTurn(ctx, "terminal_output")

		case <-periodicTicker.C: // Periodic check
			// Check if there's unread output and enough time has passed
			a.mu.Lock()
			hasOutput := a.termOutputBuf.Len() > 0
			lastOutputTime := a.lastOutput
			a.mu.Unlock()
			if hasOutput && time.Since(lastOutputTime) >= periodicUpdateInterval {
				fmt.Println("[Debug] Periodic timer triggered agent turn")
				a.processAgentTurn(ctx, "periodic_update")
			}
		}
	}
}

// Stop signals the agent to shut down gracefully.
func (a *Agent) Stop() {
	close(a.stopChan)
	// Optionally call a.cancelContext() if immediate stop is needed
}

// readUserInput continuously reads lines from stdin.
func (a *Agent) readUserInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ") // Simple prompt
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Error reading user input: %v\n", err)
			}
			// Signal stop or handle EOF?
			a.Stop()
			return
		}
		line = strings.TrimSpace(line)
		if line != "" {
			select {
			case a.userInputChan <- line:
			case <-a.stopChan:
				return
			}
		}
	}
}

// handleUserInput checks for internal commands or prepares for LLM.
// Returns true if the input was handled internally or triggers an agent turn.
func (a *Agent) handleUserInput(ctx context.Context, line string) bool {
	if strings.HasPrefix(line, "/") {
		parts := strings.Fields(line)
		commandName := strings.TrimPrefix(parts[0], "/")
		args := parts[1:]

		cmd, exists := a.cmdRegistry.Get(commandName)
		if !exists {
			fmt.Printf("Unknown command: %s\n", commandName)
			return true // Handled (by showing error)
		}

		// Execute command (needs context with dependencies)
		// TODO: Inject dependencies (LLM, Memory, etc.) into context
		fmt.Printf("[Executing Internal Command: /%s]\n", commandName)
		err := cmd.Execute(ctx, args)
		if err != nil {
			fmt.Printf("Error executing command /%s: %v\n", commandName, err)
		}
		return true // Command execution handled
	}

	// Regular user input, trigger LLM processing
	a.processAgentTurn(ctx, "user_input")
	return true
}

// handleTerminalOutput is called by SynchronizedBufferWriter when new output arrives.
func (a *Agent) handleTerminalOutput() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.lastOutput = time.Now()
	// Reset or start the debounce timer
	if a.outputTimer == nil {
		a.outputTimer = time.NewTimer(outputDebounceDuration)
	} else {
		if !a.outputTimer.Stop() {
			// Drain channel if Stop returns false (timer already fired)
			// This might require reading from the channel in getOutputTimerChannel
			// For simplicity, we assume Stop works or the timer fires eventually.
		}
		a.outputTimer.Reset(outputDebounceDuration)
	}
}

// getOutputTimerChannel returns the timer channel, handling nil timer.
func (a *Agent) getOutputTimerChannel() <-chan time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.outputTimer == nil {
		// Return a channel that never receives if timer is not set
		// (avoids nil channel read in select)
		return nil
	}
	return a.outputTimer.C
}

// processAgentTurn collects context, calls LLM, and handles the response.
func (a *Agent) processAgentTurn(ctx context.Context, trigger string) {
	a.mu.Lock()
	// Stop output timer if it's running, as we are processing now
	if a.outputTimer != nil {
		if !a.outputTimer.Stop() {
			// Drain?
		}
		a.outputTimer = nil // Prevent immediate re-trigger
	}

	// 1. Get terminal output from buffer
	terminalOutput := a.termOutputBuf.String()
	a.termOutputBuf.Reset() // Clear buffer after reading
	a.mu.Unlock()

	fmt.Printf("[Agent Turn Triggered by: %s]\n", trigger)
	if terminalOutput != "" {
		fmt.Printf("[Terminal Output Context:]\n%s\n[/Terminal Output Context]\n", terminalOutput)
	}

	// 2. Construct Prompt (including user input if trigger=="user_input", history, terminalOutput, etc.)
	// prompt := constructPrompt(userInput, terminalOutput, history...)
	fmt.Println("[Simulating LLM Call...] ")

	// 3. Call LLM (Placeholder)
	// llmResponse, err := a.llmClient.Generate(ctx, prompt)
	// Handle error
	llmResponse := "{\"cmd\": \"echo Processed trigger: " + trigger + "\"}" // Simulate echo command
	fmt.Printf("[Simulated LLM Response: %s]\n", llmResponse)

	// 4. Parse LLM Response (Placeholder)
	// action := a.parser.Parse(llmResponse)
	parsedCmd := "echo Processed trigger: " + trigger // Simulate parsing
	parsedSignal := ""

	// 5. Execute Action
	if parsedCmd != "" {
		fmt.Printf("[Sending Command to Terminal: %s]\n", parsedCmd)
		_, err := a.termController.Write([]byte(parsedCmd + "\n"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing command to terminal: %v\n", err)
		}
	} else if parsedSignal != "" {
		// Map signal name (e.g., "SIGINT") to os.Signal
		// sig := mapSignal(parsedSignal)
		// if sig != nil {
		// 	 fmt.Printf("[Sending Signal to Terminal: %v]\n", sig)
		// 	 err := a.termController.SendSignal(sig)
		// 	 if err != nil {
		// 		 fmt.Fprintf(os.Stderr, "Error sending signal: %v\n", err)
		// 	 }
		// } else {
		// 	 fmt.Fprintf(os.Stderr, "Unknown signal in LLM response: %s\n", parsedSignal)
		// }
	}

	// 6. Update history, state, etc.
}

// --- Helper for synchronized buffer writing ---

// SynchronizedBufferWriter wraps a bytes.Buffer with a mutex and a callback.
type SynchronizedBufferWriter struct {
	buf      *bytes.Buffer
	mu       *sync.Mutex
	onWrite func() // Callback function triggered after a successful write
}

// NewSynchronizedBufferWriter creates a new synchronized writer.
func NewSynchronizedBufferWriter(buf *bytes.Buffer, mu *sync.Mutex, onWrite func()) *SynchronizedBufferWriter {
	return &SynchronizedBufferWriter{
		buf:     buf,
		mu:      mu,
		onWrite: onWrite,
	}
}

// Write implements io.Writer.
func (s *SynchronizedBufferWriter) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err = s.buf.Write(p)
	if err == nil && n > 0 && s.onWrite != nil {
		// Call the callback outside the lock if it might block or lock itself,
		// but for simply triggering a timer, inside is fine.
		s.onWrite()
	}
	return
}

// Package agent contains the core logic of the AI agent.