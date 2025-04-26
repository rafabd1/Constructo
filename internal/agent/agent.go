package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/generative-ai-go/genai"
	"github.com/pkg/errors"
	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/config"
	"github.com/rafabd1/Constructo/internal/task"
	"github.com/rafabd1/Constructo/internal/types"
	"github.com/rafabd1/Constructo/pkg/events"
	"github.com/rafabd1/Constructo/pkg/utils"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Agent manages the main interaction loop, terminal, and LLM communication.
type Agent struct {
	//termController terminal.Controller
	cmdRegistry    *commands.Registry
	genaiClient    *genai.Client
	chatSession    *genai.ChatSession
	execManager    task.ExecutionManager

	// Bubble Tea program reference to send messages back
	program *tea.Program
	
	// Main agent context for background operations
	ctx context.Context
	mu            sync.Mutex
	stopChan      chan struct{}
	cancelContext context.CancelFunc

	lastSubmittedTaskID string
	lastActionFailure   error
	contextLogger       *log.Logger 
}

// GetExecutionManager returns the execution manager instance.
// Necessary to inject dependency into internal commands.
func (a *Agent) GetExecutionManager() task.ExecutionManager {
	return a.execManager
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
	// TODO: Add support for other LLMs
	apiKey := cfg.LLM.APIKey
	modelName := cfg.LLM.ModelName
	if modelName == "" {
		return nil, fmt.Errorf("LLM model name (llm.model_name) is not specified in the configuration file")
	}
	log.Printf("Using model from config: %s", modelName)
	var client *genai.Client
	if apiKey != "" {
		log.Println("Initializing Gemini client with API Key from config.")
		client, err = genai.NewClient(ctx, option.WithAPIKey(apiKey))
	} else {
		log.Println("API Key not found in config. Attempting to use default credentials (ADC).")
		var clientErr error
		client, clientErr = genai.NewClient(ctx)
		if clientErr != nil {
			return nil, fmt.Errorf("failed to create genai client with default credentials: %w", clientErr)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create genai client: %w", err)
	}

	// --- Configure Model ---
	model := client.GenerativeModel(modelName)
	if systemInstruction != "" {
		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{genai.Text(systemInstruction)},
		}
		log.Println("System instruction loaded successfully.")
	}

	// --- Start Chat Session ---
	cs := model.StartChat()

	// --- Create Execution Manager ---
	execMgr := task.NewManager()
	if err := execMgr.Start(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("failed to start execution manager: %w", err)
	}

	// --- Initialize Context Logger ---
	logFilePath := "constructo_context.log"
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Warning: Could not open context log file %s: %v", logFilePath, err)
	}
	var contextLogger *log.Logger
	if logFile != nil {
		contextLogger = log.New(logFile, "CONTEXT: ", log.LstdFlags|log.Lmsgprefix)
		log.Printf("Context logging initialized to file: %s", logFilePath)
	} else {
		// Fallback to standard logger if file fails
		contextLogger = log.New(os.Stderr, "CONTEXT_ERR: ", log.LstdFlags|log.Lmsgprefix)
		log.Println("Warning: Context logging falling back to standard error.")
	}

	// --- Create Agent Instance ---
	return &Agent{
		cmdRegistry:   registry,
		genaiClient:   client,
		chatSession:   cs,
		execManager:   execMgr,
		stopChan:      make(chan struct{}),
		ctx:           ctx,
		contextLogger: contextLogger, 
		program:       nil,           
	}, nil
}

// Stop signals the agent to shut down gracefully.
func (a *Agent) Stop() {
	log.Println("Agent Stop requested.")
	// Closing the stopChan can signal other internal goroutines, if any.
	// The actual stop logic (closing client, manager) may need to be coordinated.
	close(a.stopChan)
	// Calling cancelContext may be necessary to stop ongoing operations
	if a.cancelContext != nil {
		a.cancelContext()
	}
}

// parseUserInput checks if the line is an internal command or log.
// Returns (isInternal, isLog).
func (a *Agent) parseUserInput(line string) (bool, bool) {
	logPattern := `^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`
	matched, _ := regexp.MatchString(logPattern, line)
	// Simplificar a condição de log
	if matched || strings.HasPrefix(line, "[") {
		return false, true
	}
	if strings.HasPrefix(line, "/") {
		return true, false
	}
	return false, false
}

// executeInternalCommand executes an internal command and returns its output and error.
func (a *Agent) executeInternalCommand(ctx context.Context, commandLine string) (output string, err error) {
	parts := strings.Fields(commandLine)
	if len(parts) == 0 {
		return "", fmt.Errorf("internal command cannot be empty")
	}
	commandName := strings.TrimPrefix(parts[0], "/")
	args := parts[1:]

	cmd, exists := a.cmdRegistry.Get(commandName)
	if !exists {
		return fmt.Sprintf("Unknown command: /%s. Type /help for available commands.", commandName), nil 
	}

	outputBuf := new(bytes.Buffer)
	execErr := cmd.Execute(ctx, args, outputBuf)
	
	return outputBuf.String(), execErr 
}

// processAgentTurn collects context, calls LLM, and handles the response.
// internalResult is the result of an internal command executed in the previous turn (requested by LLM)
func (a *Agent) processAgentTurn(ctx context.Context, trigger string, userInput string, completedTask *task.Task, internalResultOutput *string) {
	a.contextLogger.Printf("--- Agent Turn Start (Trigger: %s) ---", trigger)

	// --- UTF-8 Validation of user input ---
	if userInput != "" {
		if !utf8.ValidString(userInput) {
			log.Printf("Warning: User input contains invalid UTF-8 sequence: %q", userInput)
			a.contextLogger.Printf("[Input Sanitize] Original User Input (Invalid UTF-8): %q", userInput)
			var sanitized strings.Builder
			for _, r := range userInput {
				if r == utf8.RuneError {
					sanitized.WriteRune('\uFFFD') 
				} else {
					sanitized.WriteRune(r)
				}
			}
			userInput = sanitized.String()
			log.Printf("Sanitized user input: %q", userInput)
			a.contextLogger.Printf("[Input Sanitize] Sanitized User Input: %q", userInput)
		} else {
			a.contextLogger.Printf("[Input] User Request: %q", userInput)
		}
	}

	// Log completed task info if present
	if completedTask != nil {
		a.contextLogger.Printf("[Input] Completed Task ID: %s, Command: %q, Status: %s, ExitCode: %d",
			completedTask.ID, completedTask.CommandString, completedTask.Status, completedTask.ExitCode)
		output := completedTask.OutputBuffer.String()
		outputSummary := output
		maxLen := 500
		if len(output) > maxLen { outputSummary = output[:maxLen] + "...(truncated)" }
		a.contextLogger.Printf("[Input] Task Output (Truncated): %q", outputSummary)
		if completedTask.Error != nil {
			a.contextLogger.Printf("[Input] Task Error: %v", completedTask.Error)
		} else {
			a.contextLogger.Printf("[Input] Task Error: None")
		}
	}

	// Log internal command result if present
	if internalResultOutput != nil {
		output := *internalResultOutput
		outputSummary := output
		maxLen := 500 
		if len(output) > maxLen { outputSummary = output[:maxLen] + "...(truncated)" }
		a.contextLogger.Printf("[Input] Internal Command Result (Truncated): %q", outputSummary)
	}
	// --- End of UTF-8 Validation ---

	// 1. Construir o prompt para o LLM
	var currentTurnParts []genai.Part
	contextInfo := bytes.NewBufferString("")

	a.mu.Lock()
	lastFailure := a.lastActionFailure
	a.lastActionFailure = nil
	a.mu.Unlock()
	if lastFailure != nil {
		failureText := fmt.Sprintf("[Context] Previous Action Failed: %v", lastFailure)
		fmt.Fprintln(contextInfo, failureText) // Add to buffer
		a.contextLogger.Printf("%s", failureText) // Log it
	}

	// Add internal command result *this turn* (if any) to buffer
	if internalResultOutput != nil {
		fmt.Fprintf(contextInfo, "[Internal Command Result]\n")
		output := *internalResultOutput
		if len(output) > 1000 { output = output[:1000] + "\n... (output truncated)" }
		fmt.Fprintf(contextInfo, "%s", output)
		fmt.Fprintf(contextInfo, "[End Internal Command Result]\n")
		// Log again that it was added to the context buffer
		outputSummary := *internalResultOutput
		maxLen := 500
		if len(outputSummary) > maxLen { outputSummary = outputSummary[:maxLen] + "...(truncated)" }
		a.contextLogger.Printf("[Context] Added Internal Command Result (Truncated): %q", outputSummary)
	}

	// Add running tasks summary to buffer
	runningSummary := a.execManager.GetRunningTasksSummary()
	if runningSummary != "" {
		summaryText := fmt.Sprintf("[Context] Running Tasks Summary:\n%s", runningSummary)
		fmt.Fprintln(contextInfo, ""+runningSummary+"") // Add to buffer
		a.contextLogger.Printf("%s", summaryText) // Log it
	}

	// Add Trigger to buffer
	fmt.Fprintf(contextInfo, "Trigger: %s\n", trigger)
	a.contextLogger.Printf("[Context] Added Trigger: %s", trigger)

	// Add User Request or Task Result Analysis to buffer/parts
	if userInput != "" {
		fmt.Fprintf(contextInfo, "User Request: %s\n", userInput)
		a.contextLogger.Printf("[Context] Added User Request: %q", userInput)
	} else if completedTask != nil {
		// Start turn by completing a task
		taskResultText := bytes.NewBufferString("\n[Task Result Analysis Required]\n")
		fmt.Fprintf(taskResultText, "Task ID: %s\n", completedTask.ID)
		fmt.Fprintf(taskResultText, "Command: `%s`\n", completedTask.CommandString)
		fmt.Fprintf(taskResultText, "Status: %s (Exit Code: %d)\n", completedTask.Status, completedTask.ExitCode)
		if completedTask.Error != nil {
			fmt.Fprintf(taskResultText, "Execution Error: %v\n", completedTask.Error)
		} else {
			fmt.Fprintf(taskResultText, "Execution Error: None\n")
		}

		output := completedTask.OutputBuffer.String()
		outputSummary := output
		maxLen := 1500
		if len(output) > maxLen {
			outputSummary = output[:maxLen] + "\n... (output truncated)"
		}
		fmt.Fprintf(taskResultText, "Output (truncated if long):\n```\n%s\n```\n", outputSummary)
		fmt.Fprintf(taskResultText, "[End Task Result]\n")
		fmt.Fprintf(taskResultText, "Instruction: Analyze the task result above. If the task failed or the output indicates an unexpected problem, report the issue. If the task succeeded and completed the original user request, you can simply acknowledge it. If the task succeeded but further steps are needed based on the output, propose the next command or ask the user for clarification.\n")

		taskResultTextString := taskResultText.String()
		currentTurnParts = append(currentTurnParts, genai.Text(taskResultTextString))
		a.contextLogger.Printf("[Context] Added Task Result Analysis Block:\n%s", taskResultTextString) 
		log.Printf("--- Sending Task Result to LLM (with analysis instruction) ---")
		log.Printf("--- End Task Result --- Gemini") 
	} else if internalResultOutput == nil && userInput == "" { 
		log.Println("Warning: processAgentTurn called without user input or task/internal result.")
		a.contextLogger.Printf("--- Agent Turn Skipped (No Input) ---")
		return
	}

	builtContext := contextInfo.String()
	// Only add contextInfo buffer if it contains more than just the trigger line and wasn't handled by task result
	if builtContext != "" && completedTask == nil {
		// Check if contextInfo contains more than just the trigger and potentially whitespace/newlines
		lines := strings.Split(strings.TrimSpace(builtContext), "\n")
		meaningfulContent := false
		for _, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "Trigger:") && strings.TrimSpace(line) != "" {
				meaningfulContent = true
				break
			}
		}

		if meaningfulContent {
			// We already logged the pieces added to contextInfo, so just log adding the combined buffer
			a.contextLogger.Printf("[Context] Added Combined Context Buffer")
			currentTurnParts = append(currentTurnParts, genai.Text(builtContext))
		}
	}

	if len(currentTurnParts) == 0 {
		log.Println("[Debug] Skipping agent turn: No effective prompt parts to send.")
		a.contextLogger.Printf("--- Agent Turn Skipped (No Parts) ---")
		return
	}

	// 2. Call LLM
	a.sendToTUI(events.AgentOutputMsg{Content: "[Calling Gemini...]"}) 
	a.contextLogger.Printf("--- Sending to LLM (%d parts) ---", len(currentTurnParts))
	for i, part := range currentTurnParts {
		if txt, ok := part.(genai.Text); ok {
			partText := string(txt)
			maxLogLen := 1000 // Limit log size per part
			if len(partText) > maxLogLen { partText = partText[:maxLogLen] + "...(truncated)"}
			a.contextLogger.Printf("[Part %d] Text (Truncated): %q", i+1, partText)
		} else {
			a.contextLogger.Printf("[Part %d] Type: %T", i+1, part)
		}
	}
	resp, err := a.chatSession.SendMessage(ctx, currentTurnParts...)
	if err != nil {
		errMsg := fmt.Sprintf("Error calling LLM: %v", err)
		log.Printf("%s", errMsg)
		a.contextLogger.Printf("[Error] LLM Call Failed: %v", err)
		var googleAPIErr *googleapi.Error
		if errors.As(err, &googleAPIErr) {
			errMsg = fmt.Sprintf("%s\nDetails: Code=%d, Message=%s, Body=%s", errMsg, googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
			a.contextLogger.Printf("[Error] LLM API Error Details: Code=%d, Message=%s, Body=%s", googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
		}
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: %s", errMsg)}) // Enviar erro para TUI
		a.contextLogger.Printf("--- Agent Turn End (LLM Error) ---")
		return
	}

	// 3. Parse LLM Response
	var parsedResp types.ParsedLLMResponse
	processed := false
	rawResponseText := ""

	if resp != nil && len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil && len(resp.Candidates[0].Content.Parts) > 0 {
		if txt, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
			rawResponseText = string(txt)
			a.contextLogger.Printf("--- LLM Raw Response ---")
			a.contextLogger.Printf("%s", rawResponseText)
			a.contextLogger.Printf("--- End LLM Raw Response ---")

			jsonBlock, err := utils.ExtractJsonBlock(rawResponseText)
			if err != nil {
				log.Printf("Error extracting JSON from LLM response: %v", err)
				a.contextLogger.Printf("[Error] Failed to extract JSON block from raw response: %v", err)
			} else {
				a.contextLogger.Printf("[Debug] Extracted JSON block: %s", jsonBlock)
				if err := json.Unmarshal([]byte(jsonBlock), &parsedResp); err == nil {
					if parsedResp.Type == "" { 
						log.Printf("Warning: Parsed JSON missing mandatory field 'type'. JSON: %s", jsonBlock)
						a.contextLogger.Printf("[Warning] Parsed JSON missing mandatory field 'type'. JSON: %s", jsonBlock)
					} else {
						a.contextLogger.Printf("[Debug] Successfully unmarshalled JSON: Type=%s", parsedResp.Type)
						processed = true
					}
				} else {
					log.Printf("Error unmarshaling extracted JSON block: %v\nJSON Block:\n%s", err, jsonBlock)
					a.contextLogger.Printf("[Error] Failed to unmarshal extracted JSON block: %v. JSON Block: %s", err, jsonBlock)
				}
			}
		} else {
			log.Printf("Warning: Expected genai.Text part in LLM response, but got %T", resp.Candidates[0].Content.Parts[0])
			a.contextLogger.Printf("[Warning] Expected genai.Text part in LLM response, got %T", resp.Candidates[0].Content.Parts[0])
		}
	} else {
		log.Printf("Warning: Could not process LLM response structure. Resp: %+v", resp)
		a.contextLogger.Printf("[Warning] Could not process LLM response structure.")
		if resp != nil && resp.PromptFeedback != nil {
			log.Printf("Prompt Feedback: BlockReason=%v, SafetyRatings=%+v", resp.PromptFeedback.BlockReason, resp.PromptFeedback.SafetyRatings)
			a.contextLogger.Printf("[Debug] Prompt Feedback: BlockReason=%v, SafetyRatings=%+v", resp.PromptFeedback.BlockReason, resp.PromptFeedback.SafetyRatings)
		}
		if resp != nil && len(resp.Candidates) > 0 {
			log.Printf("Candidate[0] FinishReason: %v, SafetyRatings: %+v", resp.Candidates[0].FinishReason, resp.Candidates[0].SafetyRatings)
			a.contextLogger.Printf("[Debug] Candidate[0] FinishReason: %v, SafetyRatings: %+v", resp.Candidates[0].FinishReason, resp.Candidates[0].SafetyRatings)
		}
	}

	if !processed {
		log.Println("Warning: Failed to process LLM response into valid JSON structure.")
		a.contextLogger.Printf("[Warning] Failed to process LLM response into valid JSON structure. Raw text: %q", rawResponseText)
		// Generate a standard error message if parsing fails and there is no message
		if parsedResp.Message == "" {
			errMsg := fmt.Sprintf("[Agent Error: Could not understand the response format. Raw: %s]", rawResponseText)
			if len(errMsg) > 500 { // Truncate raw response in error
				errMsg = errMsg[:500] + "...]"
			}
			parsedResp.Message = errMsg
		}
		parsedResp.Type = "response"
		a.contextLogger.Printf("[Debug] Forced response type to 'response' due to processing failure.")
	}

	// 4. Execute Action
	if parsedResp.Message != "" {
		a.sendToTUI(events.AgentOutputMsg{Content: parsedResp.Message})
		// Log action only if it's not just a simple response message
		if parsedResp.Type != "response" {
			a.contextLogger.Printf("[Action] Sent Message to TUI: %q", parsedResp.Message)
		} else {
			a.contextLogger.Printf("[Response] Sent Message to TUI: %q", parsedResp.Message)
		}
	}


	switch parsedResp.Type {
	case "command":
		if parsedResp.Command == "" { 
			errMsg := "[Agent Error]: LLM response type is command but command field is missing or empty."
			log.Println(errMsg)
			a.contextLogger.Printf("[Error] %s", errMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to run command but didn't specify which."})
			break
		}
		cmdToRun := parsedResp.Command 
		a.contextLogger.Printf("[Action] Submitting External Command: %q", cmdToRun)
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Submitting Task: %s]", cmdToRun)})
		isInteractive := false // TODO: Implement decideIfInteractive(cmdToRun)
		taskID, err := a.execManager.SubmitTask(ctx, cmdToRun, isInteractive)
		if err != nil {
			failureMsg := fmt.Errorf("SubmitTask failed for command '%s': %w", cmdToRun, err)
			log.Printf("%v", failureMsg)
			a.contextLogger.Printf("[Error] SubmitTask Failed: %v", failureMsg) // Log to context log
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: Failed to start command: %s]", cmdToRun)}) // Para TUI
			a.mu.Lock()
			a.lastActionFailure = failureMsg
			a.mu.Unlock()
		} else {
			a.contextLogger.Printf("[Action] Task Submitted: ID=%s, Command=%q", taskID, cmdToRun)
			a.mu.Lock()
			a.lastSubmittedTaskID = taskID
			a.mu.Unlock()
		}
	case "internal_command":
		if parsedResp.Command == "" {
			errMsg := "[Agent Error]: LLM response type is internal_command but command field is missing or empty."
			log.Println(errMsg)
			a.contextLogger.Printf("[Error] %s", errMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to run internal command but didn't specify which."})
			break
		}
		cmdToRun := parsedResp.Command
		a.contextLogger.Printf("[Action] Executing Internal Command: %q", cmdToRun)
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Executing Internal Command: %s]", cmdToRun)})
		output, err := a.executeInternalCommand(ctx, cmdToRun)

		if err != nil {
			if errors.Is(err, commands.ErrExitRequested) {
				a.contextLogger.Printf("[Action] Internal command requested exit. Output: %q", output)
				a.sendToTUI(events.AgentOutputMsg{Content: output})
				a.sendToTUI(events.ExitTUIMsg{})
				a.contextLogger.Printf("--- Agent Turn End (Exit Requested) ---")
				return
			}
			log.Printf("Error executing internal command requested by LLM: %v", err)
			a.contextLogger.Printf("[Error] Internal command execution failed: %v. Output: %q", err, output)
		} else {
			a.contextLogger.Printf("[Action] Internal command executed successfully. Output: %q", output)
		}

		log.Printf("Internal command executed, immediately calling agent turn with its result.")
		a.contextLogger.Printf("[Flow] Triggering next agent turn with internal command result.")
		// NOTE: Passing the context log the output *before* the next turn starts logging it as input
		a.processAgentTurn(ctx, "internal_command_result", "", nil, &output)
		// No need for Agent Turn End log here, the recursive call will handle its own end
		return // Prevent double end logging

	case "signal":
		if parsedResp.Signal == "" || parsedResp.TaskID == "" { // Check flattened fields
			errMsg := "[Agent Error]: LLM response type is signal but signal or task_id field is missing or empty."
			log.Println(errMsg)
			a.contextLogger.Printf("[Error] %s JSON: %+v", errMsg, parsedResp) // Log the whole struct
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to send signal but didn't specify signal or target."})
			break
		}
		signalName := parsedResp.Signal  
		targetTaskID := parsedResp.TaskID
		sig := utils.MapSignal(signalName)
		if sig == nil {
			errMsg := fmt.Sprintf("[Agent Error]: Unknown signal '%s'.", signalName)
			log.Println(errMsg)
			a.contextLogger.Printf("[Error] %s", errMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: errMsg})
			break
		}
		a.contextLogger.Printf("[Action] Sending Signal %v (%s) to Task %s", sig, signalName, targetTaskID)
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Sending Signal %s to Task %s]", signalName, targetTaskID)})
		err := a.execManager.SendSignalToTask(targetTaskID, sig)
		if err != nil {
			failureMsg := fmt.Errorf("SendSignal(%v) failed for task '%s': %w", sig, targetTaskID, err)
			log.Printf("%v", failureMsg)
			a.contextLogger.Printf("[Error] SendSignal Failed: %v", failureMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: Failed to send signal %s to task %s. %v]", signalName, targetTaskID, err)})
			a.mu.Lock()
			a.lastActionFailure = failureMsg
			a.mu.Unlock()
		} else {
			a.contextLogger.Printf("[Action] Signal %s sent successfully to task %s", signalName, targetTaskID)
		}
	case "response":
		a.contextLogger.Printf("[Action] LLM provided a direct response (no command/signal).")
		//break is redundant here (warning)
	default:
		unknownTypeMsg := fmt.Sprintf("[Agent Error]: Received unknown response type from LLM: %s", parsedResp.Type)
		log.Printf("%s", unknownTypeMsg)
		a.contextLogger.Printf("[Error] Unknown response type: %s. Full Parsed: %+v", parsedResp.Type, parsedResp)
	}
	a.contextLogger.Printf("--- Agent Turn End ---")
}

// ProcessUserInput receives user input from the TUI and starts processing in a goroutine.
func (a *Agent) ProcessUserInput(input string) {
	log.Printf("Agent received user input: %s", input)
	
	isInternal, isLog := a.parseUserInput(input)
	if isLog {
		return 
	}

	if isInternal {
		go func() {
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Executing Internal Command: %s]", input)})
			output, err := a.executeInternalCommand(a.ctx, input)
			if err != nil {
				if errors.Is(err, commands.ErrExitRequested) {
					a.sendToTUI(events.AgentOutputMsg{Content: output})
					a.sendToTUI(events.ExitTUIMsg{})
					return 
				}
				errorMsg := fmt.Sprintf("Error executing command: %v", err)
				a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: %s\nOutput:\n%s", errorMsg, output)})
			} else {
				a.sendToTUI(events.AgentOutputMsg{Content: output})
			}
		}()
	} else {
		go func() {
			a.processAgentTurn(a.ctx, "user_input", input, nil, nil)
		}()
	}
}

// ProcessTaskCompletion is called by the TUI when an external task completes.
func (a *Agent) ProcessTaskCompletion(taskInfo *task.Task) {
	if taskInfo == nil {
		log.Println("[Agent ProcessTaskCompletion]: Received nil taskInfo.")
		return
	}
	log.Printf("[Agent ProcessTaskCompletion]: Received completion for task %s (Status: %s)", taskInfo.ID, taskInfo.Status)
	// Execute in a goroutine to avoid blocking the TUI
	go func() {
		a.processAgentTurn(a.ctx, "task_completed", "", taskInfo, nil)
	}()
}


// SetProgram allows the TUI to inject the Bubble Tea program reference.
func (a *Agent) SetProgram(p *tea.Program) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.program != nil {
		log.Println("Warning: Trying to set tea.Program on Agent multiple times.")
		return
	}
	a.program = p
	log.Println("[Agent]: tea.Program reference set.")
}

// sendToTUI sends a message to the TUI via the Bubble Tea program.
func (a *Agent) sendToTUI(msg tea.Msg) {
	a.mu.Lock() 
	p := a.program 
	a.mu.Unlock() 

	if p == nil {
		log.Println("Warning: Agent's tea.Program reference is nil. Cannot send message:", msg)
		return
	}
	log.Printf("[Agent->TUI Send Attempt (via Program.Send)]: %+v", msg)
	go func() { 
		p.Send(msg)
		log.Printf("[Agent->TUI Send Success (via Program.Send)]")
	}()
}