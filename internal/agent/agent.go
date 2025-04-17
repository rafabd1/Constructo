package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/pkg/errors"
	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/config"
	"github.com/rafabd1/Constructo/internal/terminal"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	outputDebounceDuration = 500 * time.Millisecond
	periodicUpdateInterval = 1 * time.Minute
	geminiModelName        = "gemini-2.0-flash"
)

// AgentResponse defines the expected JSON structure from the LLM.
type AgentResponse struct {
	Msg    string `json:"msg,omitempty"`    // Message to display to the user.
	Cmd    string `json:"cmd,omitempty"`    // Command to execute in the terminal.
	Signal string `json:"signal,omitempty"` // OS Signal to send (e.g., "SIGINT").
}

// Define the schema for the AgentResponse
var agentResponseSchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"msg":    {Type: genai.TypeString, Description: "Message to display to the user."},
		"cmd":    {Type: genai.TypeString, Description: "Command to execute in the terminal."},
		"signal": {Type: genai.TypeString, Description: "OS Signal to send (e.g., 'SIGINT', 'SIGTERM')."},
	},
	// Optional: Specify required fields if needed
	// Required: []string{"msg"}, // Example: msg is always required
}

// Agent manages the main interaction loop, terminal, and LLM communication.
type Agent struct {
	termController terminal.Controller
	cmdRegistry    *commands.Registry
	genaiClient    *genai.Client
	chatSession    *genai.ChatSession

	userInputChan chan string
	termOutputBuf *bytes.Buffer
	mu            sync.Mutex
	lastOutput    time.Time
	outputTimer   *time.Timer
	stopChan      chan struct{}
	cancelContext context.CancelFunc

	// Store the last user input for context
	lastUserInput string
}

// NewAgent creates a new agent instance based on the provided configuration.
func NewAgent(ctx context.Context, cfg *config.Config, registry *commands.Registry) (*Agent, error) {
	// --- Load System Instruction ---
	systemInstructionBytes, err := os.ReadFile("instructions/system_prompt.txt")
	if err != nil {
		log.Printf("Warning: Could not read system instructions file 'instructions/system_prompt.txt': %v. Proceeding without system instructions.", err)
	}
	systemInstruction := string(systemInstructionBytes)

	// --- Initialize Gemini Client using Config ---
	apiKey := cfg.LLM.APIKey
	modelName := cfg.LLM.ModelName

	var client *genai.Client

	if apiKey != "" {
		log.Println("Initializing Gemini client with API Key from config.")
		client, err = genai.NewClient(ctx, option.WithAPIKey(apiKey))
		if err != nil {
			return nil, fmt.Errorf("failed to create genai client with API key: %w", err)
		}
	} else {
		log.Println("API Key not found in config. Attempting to use default credentials (ADC).")
		client, err = genai.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create genai client with default credentials: %w", err)
		}
	}

	// --- Configure Model ---
	model := client.GenerativeModel(modelName)
	if systemInstruction != "" {
		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{genai.Text(systemInstruction)},
		}
		log.Println("System instruction loaded successfully.")
	}
	
	// Configure for Direct JSON Output
	model.ResponseMIMEType = "application/json"
	model.ResponseSchema = agentResponseSchema 
	log.Println("Model configured for direct JSON output.")

	// --- Start Chat Session ---
	cs := model.StartChat()

	// --- Create Agent Instance ---
	return &Agent{
		cmdRegistry:   registry,
		genaiClient:   client,
		chatSession:   cs,
		userInputChan: make(chan string, 1),
		termOutputBuf: &bytes.Buffer{},
		stopChan:      make(chan struct{}),
	}, nil
}

// Run starts the main agent loop.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	a.cancelContext = cancel
	defer cancel()
	// Close genai client on exit
	defer func() {
		if err := a.genaiClient.Close(); err != nil {
			log.Printf("Error closing genai client: %v", err)
		}
	}()

	// 1. Initialize Terminal Controller
	a.termController = terminal.NewPtyController()

	// 2. Setup Writer for terminal output
	// Output goes ONLY to the agent buffer (outputWriter) via the monitor.
	// The agent will decide what to print to the user's os.Stdout.
	a.termOutputBuf.Reset() // Explicitly reset buffer before use
	outputWriter := NewSynchronizedBufferWriter(a.termOutputBuf, &a.mu, a.handleTerminalOutput)
	a.termController.SetOutput(outputWriter) // Set output directly to our buffer writer

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
			// fmt.Printf("[Debug] Received line from userInputChan: %q\n", line)
			if a.handleUserInput(ctx, line) {
				// User input handled internally or triggered agent turn
			} else {
				// Re-enable direct write for non-command input
				_, err := a.termController.Write([]byte(line + "\n"))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error writing to terminal: %v\n", err)
				}
				// fmt.Println("[Debug] Direct write to terminal disabled for hang test.") // Removed
			}

		case <-a.getOutputTimerChannel(): // Debounced output trigger
			fmt.Println("[Debug] Output timer fired")
			a.processAgentTurn(ctx, "terminal_output")

		case <-periodicTicker.C: // Periodic check
			a.mu.Lock()
			hasOutput := a.termOutputBuf.Len() > 0
			// lastOutputTime := a.lastOutput // Use this if needed
			a.mu.Unlock()
			if hasOutput /* && time.Since(lastOutputTime) >= periodicUpdateInterval */ {
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
			a.Stop()
			return
		}
		line = strings.TrimSpace(line)
		if line != "" {
			// fmt.Printf("[Debug] readUserInput sending line: %q\n", line)
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
	// fmt.Printf("[Debug] handleUserInput called with line: %q\n", line)
	if strings.HasPrefix(line, "/") {
		parts := strings.Fields(line)
		commandName := strings.TrimPrefix(parts[0], "/")
		args := parts[1:]

		cmd, exists := a.cmdRegistry.Get(commandName)
		if !exists {
			fmt.Printf("Unknown command: %s\n", commandName)
			return true // Handled (by showing error)
		}

		fmt.Printf("[Executing Internal Command: /%s]\n", commandName)
		err := cmd.Execute(ctx, args)
		if err != nil {
			fmt.Printf("Error executing command /%s: %v\n", commandName, err)
		}
		return true // Command execution handled
	}

	// Regular user input: always trigger agent turn
	// fmt.Println("[Debug] handleUserInput triggering processAgentTurn for normal input.") // Removed debug log
	a.mu.Lock()
	a.lastUserInput = line
	a.mu.Unlock()
	a.processAgentTurn(ctx, "user_input")
	return true // Agent turn was triggered
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
		// Stop the existing timer. If Stop returns false, the timer has already fired.
		// We don't need to drain the channel here; the select loop handles consumption.
		_ = a.outputTimer.Stop() // We ignore the return value for simplicity
		a.outputTimer.Reset(outputDebounceDuration) // Reset the timer
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
	// Remove temporary simplification log
	// fmt.Printf("[Debug] processAgentTurn called with trigger: %s\n", trigger)

	a.mu.Lock()
	// Stop output timer if it's running, as we are processing now.
	// Don't nil it out, just stop it.
	if a.outputTimer != nil {
		if !a.outputTimer.Stop() {}
	}

	// Get raw terminal output directly
	terminalOutput := a.termOutputBuf.String()
	a.termOutputBuf.Reset()
	currentUserInput := ""
	if trigger == "user_input" {
		currentUserInput = a.lastUserInput
		a.lastUserInput = ""
	}
	// lastCmdSent := a.lastAgentCmd // Removed
	a.mu.Unlock()

	fmt.Printf("[Agent Turn Triggered by: %s]\n", trigger)

	// Remove cleaning logic
	/* // --- Clean Terminal Output --- 
	terminalOutputClean := terminalOutputRaw
	if lastCmdSent != "" {
		lines := strings.SplitN(terminalOutputRaw, "\n", 2)
		firstLineTrimmed := strings.TrimSpace(lines[0])
		if strings.Contains(firstLineTrimmed, lastCmdSent) { 
			if len(lines) > 1 {
				terminalOutputClean = strings.TrimSpace(lines[1])
			} else {
				terminalOutputClean = ""
			}
			log.Printf("[Debug] Cleaned command echo '%s' from terminal output.", lastCmdSent)
		}
	}
	// --- End Cleaning --- */

	// 2. Construct LLM Message Part(s) - Use raw terminalOutput
	var promptParts []genai.Part
	if trigger == "user_input" && currentUserInput != "" {
		promptParts = append(promptParts, genai.Text(fmt.Sprintf("User Request: %s", currentUserInput)))
	} else {
		if terminalOutput == "" { // Use raw output for check
			promptParts = append(promptParts, genai.Text(fmt.Sprintf("Event Trigger: %s (No new terminal output)", trigger)))
		} else {
		    promptParts = append(promptParts, genai.Text(fmt.Sprintf("Event Trigger: %s", trigger)))
		}
	}
	// Always add raw terminal output context if it exists
	if terminalOutput != "" {
		promptParts = append(promptParts, genai.Text(fmt.Sprintf("\n\n[Terminal Output Since Last Turn:]\n%s", terminalOutput)))
	}

	// --- DEBUG LOG for prompt --- (Keep for now)
	log.Printf("--- Sending Prompt Parts to LLM ---")
	for i, part := range promptParts {
		if txt, ok := part.(genai.Text); ok {
			log.Printf("Part %d: %s", i, string(txt))
		} else {
			log.Printf("Part %d: Non-text part (%T)", i, part)
		}
	}
	log.Printf("--- End Prompt Parts ---")
	// --- END DEBUG LOG ---

	if len(promptParts) == 0 {
	    log.Println("[Debug] Skipping agent turn: No effective prompt parts to send.")
	    return
	}

	// 3. Call LLM
	fmt.Println("[Calling Gemini...] ")
	resp, err := a.chatSession.SendMessage(ctx, promptParts...)
	if err != nil {
		log.Printf("Error calling Gemini: %v", err)

		// Attempt to get more details from googleapi.Error
		var googleAPIErr *googleapi.Error
		if errors.As(err, &googleAPIErr) {
			log.Printf("Google API Error Details: Code=%d, Message=%s, Body=%s",
				googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
			fmt.Fprintf(os.Stderr, "Error communicating with LLM (Code %d): %s\nDetails: %s\n",
				googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
		} else {
			// Fallback for non-googleapi errors
			fmt.Fprintf(os.Stderr, "Error communicating with LLM: %v\n", err)
		}
		return
	}

	// 4. Parse LLM Response (expect direct JSON in Text part)
	var agentResp AgentResponse
	processed := false

	if resp == nil {
		log.Println("Warning: Received nil response from LLM.")
	} else if len(resp.Candidates) == 0 {
		log.Println("Warning: Received response with no candidates from LLM.")
		if resp.PromptFeedback != nil {
			log.Printf("Prompt Feedback: BlockReason=%v, SafetyRatings=%+v", resp.PromptFeedback.BlockReason, resp.PromptFeedback.SafetyRatings)
		}
	} else if resp.Candidates[0].Content == nil {
		log.Println("Warning: Received candidate with nil content from LLM.")
		candidate := resp.Candidates[0]
		log.Printf("Candidate FinishReason: %v, SafetyRatings: %+v, CitationMetadata: %+v",
			candidate.FinishReason, candidate.SafetyRatings, candidate.CitationMetadata)
	} else if len(resp.Candidates[0].Content.Parts) == 0 {
		log.Println("Warning: Received candidate content with no parts from LLM.")
		candidate := resp.Candidates[0]
		log.Printf("Candidate FinishReason: %v, SafetyRatings: %+v, CitationMetadata: %+v",
			candidate.FinishReason, candidate.SafetyRatings, candidate.CitationMetadata)
	} else {
		// Expect the JSON content directly in the first Text part
		part := resp.Candidates[0].Content.Parts[0]
		if txt, ok := part.(genai.Text); ok {
			jsonData := []byte(txt)
			if err := json.Unmarshal(jsonData, &agentResp); err != nil {
				log.Printf("Error unmarshaling direct JSON response: %v\nJSON received: %s", err, string(jsonData))
			} else {
				processed = true
				fmt.Printf("[LLM JSON Response Parsed: %+v]\n", agentResp)
			}
		} else {
			log.Printf("Warning: Expected genai.Text part for JSON response, but got %T", part)
		}
	}
	// --- END RESPONSE PROCESSING --- 

	if !processed {
		log.Println("Warning: Could not process LLM response content.")
		if agentResp.Msg == "" { 
		    agentResp.Msg = "[Agent Error: Could not process response content]"
		}
	}

	// 5. Execute Action
	if agentResp.Msg != "" {
		fmt.Printf("[Agent]: %s\n", agentResp.Msg)
	}

	if agentResp.Cmd != "" {
		fmt.Printf("[Sending Command to Terminal: %s]\n", agentResp.Cmd)
		_, err := a.termController.Write([]byte(agentResp.Cmd + "\n"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing command to terminal: %v\n", err)
		}
	} else if agentResp.Signal != "" {
		sig := mapSignal(agentResp.Signal)
		if sig != nil {
			fmt.Printf("[Sending Signal to Terminal: %v (%s)]\n", sig, agentResp.Signal)
			err := a.termController.SendSignal(sig)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error sending signal: %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Unknown signal in LLM response: %s\n", agentResp.Signal)
		}
	}

	// 6. Update history is handled automatically by chatSession.SendMessage

}

// mapSignal converts a signal name string to an os.Signal.
// TODO: Make this more comprehensive.
func mapSignal(signalName string) os.Signal {
	switch strings.ToUpper(signalName) {
	case "SIGINT":
		return os.Interrupt // syscall.SIGINT
	case "SIGTERM":
		return os.Kill // syscall.SIGTERM
	case "SIGKILL":
		return os.Kill // syscall.SIGKILL
	// Add other common signals as needed (SIGTSTP, SIGHUP, etc.)
	default:
		return nil
	}
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
	// Write to buffer under lock
	n, err = s.buf.Write(p)
	// Capture callback function pointer under lock
	onWriteCallback := s.onWrite
	s.mu.Unlock() // <<< RELEASE LOCK HERE

	// Call callback AFTER lock is released
	if err == nil && n > 0 && onWriteCallback != nil {
		onWriteCallback()
	}
	return
}

// Package agent contains the core logic of the AI agent.