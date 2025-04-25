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

	//"github.com/rafabd1/Constructo/internal/terminal"
	"github.com/rafabd1/Constructo/pkg/events"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	// Removido: geminiModelName        = "gemini-1.5-flash"
)

// --- Estrutura para Resposta JSON Esperada (Plain Text - Achatada) ---
type ParsedLLMResponse struct {
	Type    string `json:"type"`               // "response", "command", "internal_command", "signal"
	Message string `json:"message"`            // Mandatory for response, optional otherwise
	Command string `json:"command,omitempty"`  // Mandatory if type is command/internal_command
	Signal  string `json:"signal,omitempty"`   // Mandatory if type is signal (e.g., "SIGINT")
	TaskID  string `json:"task_id,omitempty"` // Mandatory if type is signal
	// Risk           int    `json:"risk,omitempty"`          // Example field from user prompt, add if needed
	// Confirmation   bool   `json:"confirmation,omitempty"`  // Example field
}

// Agent manages the main interaction loop, terminal, and LLM communication.
type Agent struct {
	//termController terminal.Controller
	cmdRegistry    *commands.Registry
	genaiClient    *genai.Client
	chatSession    *genai.ChatSession
	execManager    task.ExecutionManager

	// Referência ao programa Bubble Tea para enviar mensagens de volta
	program *tea.Program
	
	// Contexto principal do agente para operações em background
	ctx context.Context
	mu            sync.Mutex
	stopChan      chan struct{}
	cancelContext context.CancelFunc

	lastSubmittedTaskID string
	lastActionFailure   error
	contextLogger       *log.Logger // Logger for agent context history
}

// GetExecutionManager retorna a instância do gerenciador de execução.
// Necessário para injetar dependência em comandos internos.
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
	apiKey := cfg.LLM.APIKey
	modelName := cfg.LLM.ModelName
	// Exigir que o nome do modelo esteja na configuração
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
		// Tenta fechar o cliente genai se a inicialização do manager falhar
		_ = client.Close()
		return nil, fmt.Errorf("failed to start execution manager: %w", err)
	}

	// --- Initialize Context Logger ---
	logFilePath := "constructo_context.log"
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		// Log the error but continue, context logging is auxiliary
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
		contextLogger: contextLogger, // Assign the new logger
		program:       nil,           // Initialize program as nil
	}, nil
}

// Stop signals the agent to shut down gracefully.
func (a *Agent) Stop() {
	log.Println("Agent Stop requested.")
	// Fechar o stopChan pode sinalizar outras goroutines internas, se houver.
	// A lógica de parada real (fechar cliente, manager) pode precisar ser coordenada.
	close(a.stopChan)
	// Chamar cancelContext pode ser necessário para interromper operações em andamento
	if a.cancelContext != nil {
		a.cancelContext()
	}
}

// parseUserInput verifica se a linha é um comando interno ou log.
// Retorna (isInternal, isLog).
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

// executeInternalCommand executa um comando interno e retorna sua saída e erro.
func (a *Agent) executeInternalCommand(ctx context.Context, commandLine string) (output string, err error) {
	parts := strings.Fields(commandLine)
	if len(parts) == 0 {
		return "", fmt.Errorf("internal command cannot be empty")
	}
	commandName := strings.TrimPrefix(parts[0], "/")
	args := parts[1:]

	cmd, exists := a.cmdRegistry.Get(commandName)
	if !exists {
		// Retorna a mensagem de erro como output
		return fmt.Sprintf("Unknown command: /%s. Type /help for available commands.", commandName), nil 
	}

	outputBuf := new(bytes.Buffer)
	execErr := cmd.Execute(ctx, args, outputBuf) // Comandos internos agora escrevem no buffer
	
	// Retorna a saída capturada e o erro da execução
	return outputBuf.String(), execErr 
}

// --- Função Auxiliar para Parsing Robusto ---
func extractJsonBlock(rawResponse string) (string, error) {
	// Tentativa 1: Remover espaços em branco e verificar se é JSON válido diretamente
	trimmed := strings.TrimSpace(rawResponse)
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) || 
	   (strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		if json.Valid([]byte(trimmed)) {
			return trimmed, nil
		}
	}

	// Tentativa 2: Encontrar o primeiro '{' e o último '}'
	firstBrace := strings.Index(rawResponse, "{")
	lastBrace := strings.LastIndex(rawResponse, "}")
	if firstBrace != -1 && lastBrace != -1 && lastBrace > firstBrace {
		extracted := rawResponse[firstBrace : lastBrace+1]
		if json.Valid([]byte(extracted)) {
			log.Println("[Debug] Extracted JSON block using first/last brace.")
			return extracted, nil
		}
	}

	// Tentativa 3: Regex mais simples (pode ser frágil)
	// jsonRegex := regexp.MustCompile(`(?s){.*}`) // Regex simples
	// match := jsonRegex.FindString(rawResponse)
	// if match != "" && json.Valid([]byte(match)) {
	// 	log.Println("[Debug] Extracted JSON block using simple regex.")
	// 	return match, nil
	// }

	return "", fmt.Errorf("could not extract valid JSON block from LLM response")
}

// processAgentTurn collects context, calls LLM, and handles the response.
// internalResult é o resultado de um comando interno executado NO TURNO ANTERIOR (solicitado pelo LLM)
func (a *Agent) processAgentTurn(ctx context.Context, trigger string, userInput string, completedTask *task.Task, internalResultOutput *string) {
	a.contextLogger.Printf("--- Agent Turn Start (Trigger: %s) ---", trigger)

	// --- Validação UTF-8 da entrada do usuário ---
	if userInput != "" {
		if !utf8.ValidString(userInput) {
			log.Printf("Warning: User input contains invalid UTF-8 sequence: %q", userInput)
			a.contextLogger.Printf("[Input Sanitize] Original User Input (Invalid UTF-8): %q", userInput)
			var sanitized strings.Builder
			for _, r := range userInput {
				if r == utf8.RuneError { // Check against the constant
					// Write the standard Unicode replacement character rune
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
		maxLen := 500 // Shorter limit for context log
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
		maxLen := 500 // Shorter limit for context log
		if len(output) > maxLen { outputSummary = output[:maxLen] + "...(truncated)" }
		a.contextLogger.Printf("[Input] Internal Command Result (Truncated): %q", outputSummary)
	}
	// --- Fim da Validação UTF-8 ---

	// 1. Construir o prompt para o LLM
	var currentTurnParts []genai.Part
	contextInfo := bytes.NewBufferString("")

	// Incluir falha da ação anterior
	a.mu.Lock()
	lastFailure := a.lastActionFailure
	a.lastActionFailure = nil
	a.mu.Unlock()
	if lastFailure != nil {
		failureText := fmt.Sprintf("[Context] Previous Action Failed: %v", lastFailure)
		fmt.Fprintln(contextInfo, failureText) // Add to buffer
		a.contextLogger.Printf("%s", failureText) // Log it
	}

	// Adiciona resultado do comando interno *deste turno* (se houver) ao buffer
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

	// Adiciona resumo das tarefas em execução ao buffer
	runningSummary := a.execManager.GetRunningTasksSummary()
	if runningSummary != "" {
		summaryText := fmt.Sprintf("[Context] Running Tasks Summary:\n%s", runningSummary)
		fmt.Fprintln(contextInfo, ""+runningSummary+"") // Add to buffer
		a.contextLogger.Printf("%s", summaryText) // Log it
	}

	// Adiciona Trigger ao buffer
	fmt.Fprintf(contextInfo, "Trigger: %s\n", trigger)
	a.contextLogger.Printf("[Context] Added Trigger: %s", trigger)

	// Adiciona User Request ou Task Result Analysis ao buffer/parts
	if userInput != "" {
		fmt.Fprintf(contextInfo, "User Request: %s\n", userInput)
		a.contextLogger.Printf("[Context] Added User Request: %q", userInput)
	} else if completedTask != nil {
		// Turno iniciado pela conclusão de uma tarefa
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

		taskResultTextString := taskResultText.String() // Capture the constructed text
		currentTurnParts = append(currentTurnParts, genai.Text(taskResultTextString))
		a.contextLogger.Printf("[Context] Added Task Result Analysis Block:\n%s", taskResultTextString) // Log the full block added
		log.Printf("--- Sending Task Result to LLM (with analysis instruction) ---")
		// log.Printf("%s", taskResultText.String()) // Already logged by contextLogger
		log.Printf("--- End Task Result --- Gemini") // Use Gemini instead of Gêmeos
	} else if internalResultOutput == nil && userInput == "" { // Modificado: só aviso se não houver NADA
		log.Println("Warning: processAgentTurn called without user input or task/internal result.")
		a.contextLogger.Printf("--- Agent Turn Skipped (No Input) ---")
		return
	}

	// Adiciona o contexto construído como genai.Text, se não foi tratado pelo Task Result
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
		// Remover log de debug de skip
		log.Println("[Debug] Skipping agent turn: No effective prompt parts to send.")
		a.contextLogger.Printf("--- Agent Turn Skipped (No Parts) ---")
		return
	}

	// 2. Call LLM
	a.sendToTUI(events.AgentOutputMsg{Content: "[Calling Gemini...]"}) // Enviar status para TUI
	// Log parts before sending
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
		a.contextLogger.Printf("[Error] LLM Call Failed: %v", err) // Log error to context log too
		// Tentar extrair detalhes do erro da API
		var googleAPIErr *googleapi.Error
		if errors.As(err, &googleAPIErr) {
			errMsg = fmt.Sprintf("%s\nDetails: Code=%d, Message=%s, Body=%s", errMsg, googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
			a.contextLogger.Printf("[Error] LLM API Error Details: Code=%d, Message=%s, Body=%s", googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
		}
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: %s", errMsg)}) // Enviar erro para TUI
		a.contextLogger.Printf("--- Agent Turn End (LLM Error) ---")
		return
	}

	// 3. Parse LLM Response (AGORA COMO TEXTO -> JSON)
	var parsedResp ParsedLLMResponse
	processed := false
	rawResponseText := ""

	if resp != nil && len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil && len(resp.Candidates[0].Content.Parts) > 0 {
		// Assumir que a resposta está na primeira parte e é texto
		if txt, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
			rawResponseText = string(txt)
			// ---> LOG RAW RESPONSE <---
			a.contextLogger.Printf("--- LLM Raw Response ---")
			a.contextLogger.Printf("%s", rawResponseText) // Log the full raw response
			a.contextLogger.Printf("--- End LLM Raw Response ---")
			// ---> END LOG RAW RESPONSE <---

			jsonBlock, err := extractJsonBlock(rawResponseText)
			if err != nil {
				log.Printf("Error extracting JSON from LLM response: %v", err)
				a.contextLogger.Printf("[Error] Failed to extract JSON block from raw response: %v", err)
			} else {
				a.contextLogger.Printf("[Debug] Extracted JSON block: %s", jsonBlock)
				if err := json.Unmarshal([]byte(jsonBlock), &parsedResp); err == nil {
					// Validação básica da estrutura parseada
					if parsedResp.Type == "" || parsedResp.Message == "" {
						log.Printf("Warning: Parsed JSON missing mandatory fields 'type' or 'message'. JSON: %s", jsonBlock)
						a.contextLogger.Printf("[Warning] Parsed JSON missing mandatory fields 'type' or 'message'. JSON: %s", jsonBlock)
						// Still mark as processed if basic structure is okay, rely on later checks
						processed = true // Assume basic structure is ok for now
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
		// Logar feedback se houver
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
		// Gera uma mensagem de erro padrão se o parse falhar e não houver mensagem
		if parsedResp.Message == "" {
			errMsg := fmt.Sprintf("[Agent Error: Could not understand the response format. Raw: %s]", rawResponseText)
			if len(errMsg) > 500 { // Truncar raw response no erro
				errMsg = errMsg[:500] + "...]"
			}
			parsedResp.Message = errMsg
		}
		parsedResp.Type = "response" // Força tipo response em caso de erro de parse
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
		if parsedResp.Command == "" { // Check flattened field
			errMsg := "[Agent Error]: LLM response type is command but command field is missing or empty."
			log.Println(errMsg)
			a.contextLogger.Printf("[Error] %s", errMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to run command but didn't specify which."})
			break
		}
		cmdToRun := parsedResp.Command // Use flattened field
		a.contextLogger.Printf("[Action] Submitting External Command: %q", cmdToRun)
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Submitting Task: %s]", cmdToRun)})
		isInteractive := false // TODO: Implement decideIfInteractive(cmdToRun)
		taskID, err := a.execManager.SubmitTask(ctx, cmdToRun, isInteractive)
		if err != nil {
			failureMsg := fmt.Errorf("SubmitTask failed for command '%s': %w", cmdToRun, err)
			log.Printf("%v", failureMsg) // Log interno
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
		if parsedResp.Command == "" { // Check flattened field
			errMsg := "[Agent Error]: LLM response type is internal_command but command field is missing or empty."
			log.Println(errMsg)
			a.contextLogger.Printf("[Error] %s", errMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to run internal command but didn't specify which."})
			break
		}
		cmdToRun := parsedResp.Command // Use flattened field
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
			// O erro é incluído na output que vai para a chamada recursiva
		} else {
			a.contextLogger.Printf("[Action] Internal command executed successfully. Output: %q", output)
		}

		// Chama processAgentTurn imediatamente com o resultado
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
		signalName := parsedResp.Signal   // Use flattened field
		targetTaskID := parsedResp.TaskID // Use flattened field
		sig := mapSignal(signalName)
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
		// Mensagem já enviada no início do passo 4 e logada.
		a.contextLogger.Printf("[Action] LLM provided a direct response (no command/signal).")
		break // Ensure we break here
	default:
		unknownTypeMsg := fmt.Sprintf("[Agent Error]: Received unknown response type from LLM: %s", parsedResp.Type)
		log.Printf("%s", unknownTypeMsg)
		a.contextLogger.Printf("[Error] Unknown response type: %s. Full Parsed: %+v", parsedResp.Type, parsedResp)
		//a.sendToTUI(AgentOutputMsg{Content: unknownTypeMsg}) // Avoid flooding TUI with internal errors if possible
	}
	a.contextLogger.Printf("--- Agent Turn End ---")
}


// mapSignal converts a signal name string to an os.Signal.
func mapSignal(signalName string) os.Signal {
	switch strings.ToUpper(signalName) {
	case "SIGINT":
		return os.Interrupt
	case "SIGTERM":
		return os.Kill // Mapeia para SIGKILL em Go no Unix
	case "SIGKILL":
		return os.Kill
	default:
		return nil
	}
}

// SynchronizedBufferWriter não é mais necessário com o ExecutionManager

// TODO: Implementar decideIfInteractive(cmd string) bool
// Uma heurística simples pode verificar se o comando é `bash`, `python`, `ssh`, etc.
// ou se contém subcomandos como `read`.

// Package agent contains the core logic of the AI agent.

// ProcessUserInput recebe input da TUI e inicia o processamento em uma goroutine.
func (a *Agent) ProcessUserInput(input string) {
	log.Printf("Agent received user input: %s", input)
	
	// Verifica se é um comando interno do usuário
	isInternal, isLog := a.parseUserInput(input)
	if isLog {
		return // Ignorar logs
	}

	if isInternal {
		// Executar comando interno diretamente e enviar saída para TUI
		go func() {
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Executing Internal Command: %s]", input)})
			output, err := a.executeInternalCommand(a.ctx, input)
			if err != nil {
				if errors.Is(err, commands.ErrExitRequested) {
					a.sendToTUI(events.AgentOutputMsg{Content: output})
					a.sendToTUI(events.ExitTUIMsg{})
					return 
				}
				// Envia erro e output para TUI
				errorMsg := fmt.Sprintf("Error executing command: %v", err)
				a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: %s\nOutput:\n%s", errorMsg, output)})
			} else {
				// Envia output para TUI
				a.sendToTUI(events.AgentOutputMsg{Content: output})
			}
		}()
	} else {
		// É input para o LLM, processar como antes
		go func() {
			a.processAgentTurn(a.ctx, "user_input", input, nil, nil)
		}()
	}
}

// ProcessTaskCompletion é chamado pela TUI quando uma tarefa externa termina.
func (a *Agent) ProcessTaskCompletion(taskInfo *task.Task) {
	if taskInfo == nil {
		log.Println("[Agent ProcessTaskCompletion]: Received nil taskInfo.")
		return
	}
	log.Printf("[Agent ProcessTaskCompletion]: Received completion for task %s (Status: %s)", taskInfo.ID, taskInfo.Status)
	// Executa em goroutine para não bloquear a TUI
	go func() {
		a.processAgentTurn(a.ctx, "task_completed", "", taskInfo, nil)
	}()
}

// SetProgram permite que a TUI injete a referência ao programa Bubble Tea.
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

// sendToTUI agora usa program.Send.
func (a *Agent) sendToTUI(msg tea.Msg) {
	a.mu.Lock() // Usar Lock normal
	p := a.program 
	a.mu.Unlock() // Usar Unlock normal

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