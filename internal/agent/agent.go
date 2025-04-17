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
	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/terminal"
	"google.golang.org/api/option"
	// Placeholder for other imports
	// "github.com/rafabd1/Constructo/internal/config"
)

const (
	outputDebounceDuration = 500 * time.Millisecond
	periodicUpdateInterval = 1 * time.Minute
	geminiModelName        = "gemini-1.5-flash"
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

// NewAgent creates a new agent instance.
func NewAgent(ctx context.Context, registry *commands.Registry) (*Agent, error) {
	// --- Load System Instruction ---
	systemInstructionBytes, err := os.ReadFile("instructions/system_prompt.txt")
	if err != nil {
		log.Printf("Warning: Could not read system instructions file 'instructions/system_prompt.txt': %v. Proceeding without system instructions.", err)
		// Continue without system instructions, or return error if it's critical
		// return nil, fmt.Errorf("failed to load system instructions: %w", err)
	}
	systemInstruction := string(systemInstructionBytes)

	// --- Initialize Gemini Client using API Key ---
	apiKey := "x" // TODO: Load from config or env more securely
	if apiKey == "" {
		return nil, fmt.Errorf("API Key not found. Please set the API_KEY environment variable or configuration")
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create genai client: %w", err)
	}

	// --- Configure Model ---
	model := client.GenerativeModel(geminiModelName)
	// Set System Instruction if loaded successfully
	if systemInstruction != "" {
		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{genai.Text(systemInstruction)},
		}
		log.Println("System instruction loaded successfully.")
	}

	model.Tools = []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "agent_action",
			Description: "Specify the next action for the agent: a message, a command, or a signal.",
			Parameters:  agentResponseSchema,
		}},
	}}
	model.ToolConfig = &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{
			// Mode: genai.FunctionCallingModeAuto, // Omit Mode, let it default to Auto
			AllowedFunctionNames: []string{"agent_action"},
		},
	}

	// --- Start Chat Session ---
	cs := model.StartChat()
	// TODO: Load initial history if available

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

	// 2. Setup MultiWriter for terminal output
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
				// User input handled internally or triggered agent turn
			} else {
				// Send directly to terminal PTY
				_, err := a.termController.Write([]byte(line + "\n"))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error writing to terminal: %v\n", err)
				}
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

		fmt.Printf("[Executing Internal Command: /%s]\n", commandName)
		err := cmd.Execute(ctx, args)
		if err != nil {
			fmt.Printf("Error executing command /%s: %v\n", commandName, err)
		}
		return true // Command execution handled
	}

	// Store user input and trigger LLM processing
	a.mu.Lock()
	a.lastUserInput = line
	a.mu.Unlock()
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
	// Stop output timer
	if a.outputTimer != nil {
		if !a.outputTimer.Stop() {
			// Drain?
		}
		a.outputTimer = nil
	}

	// Get context
	terminalOutput := a.termOutputBuf.String()
	a.termOutputBuf.Reset()
	currentUserInput := "" // Get last user input if trigger is user_input
	if trigger == "user_input" {
		currentUserInput = a.lastUserInput
		a.lastUserInput = "" // Clear after consuming
	}
	a.mu.Unlock()

	fmt.Printf("[Agent Turn Triggered by: %s]\n", trigger)

	// 2. Construct LLM Message Part(s)
	var promptParts []genai.Part
	if trigger == "user_input" {
		promptParts = append(promptParts, genai.Text(fmt.Sprintf("User Request: %s", currentUserInput)))
	} else {
		promptParts = append(promptParts, genai.Text(fmt.Sprintf("Event Trigger: %s", trigger)))
	}
	if terminalOutput != "" {
		promptParts = append(promptParts, genai.Text(fmt.Sprintf("\n\n[Recent Terminal Output:]\n%s", terminalOutput)))
	}

	// 3. Call LLM
	fmt.Println("[Calling Gemini...] ")
	resp, err := a.chatSession.SendMessage(ctx, promptParts...)
	if err != nil {
		log.Printf("Error calling Gemini: %v", err)
		// TODO: How to recover or inform user?
		// Maybe display error message via AgentResponse.Msg?
		fmt.Fprintf(os.Stderr, "Error communicating with LLM: %v\n", err)
		return
	}

	// 4. Parse LLM Response (extract function call)
	var agentResp AgentResponse
	processed := false
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		part := resp.Candidates[0].Content.Parts[0]
		if fc, ok := part.(genai.FunctionCall); ok {
			if fc.Name == "agent_action" {
				jsonData, err := json.Marshal(fc.Args)
				if err != nil {
					log.Printf("Error marshaling function call args: %v", err)
				} else {
					if err := json.Unmarshal(jsonData, &agentResp); err != nil {
						log.Printf("Error unmarshaling agent response JSON: %v", err)
						// Attempt to extract text part as fallback message?
					} else {
						processed = true
						fmt.Printf("[LLM Response Parsed: %+v]\n", agentResp)
					}
				}
			}
		} else if txt, ok := part.(genai.Text); ok {
			// If no function call, treat the text as a simple message
			agentResp.Msg = string(txt)
			processed = true
			fmt.Printf("[LLM Text Response: %s]\n", agentResp.Msg)
		}
	}

	if !processed {
		log.Println("Warning: Could not process LLM response.")
		agentResp.Msg = "[Agent Error: Could not process response]"
		// Potentially add original response text here if available
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
		// Map signal name to os.Signal
		sig := mapSignal(agentResp.Signal) // Need to implement mapSignal
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