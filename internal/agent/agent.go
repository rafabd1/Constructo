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
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/generative-ai-go/genai"
	"github.com/pkg/errors"
	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/config"
	"github.com/rafabd1/Constructo/internal/task"
	"github.com/rafabd1/Constructo/internal/terminal"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	// Removido: geminiModelName        = "gemini-1.5-flash"
)

// --- NOVA Estrutura para Resposta JSON Esperada (Plain Text) ---
type ParsedLLMResponse struct {
	Type           string                 `json:"type"`           // "response", "command", "internal_command", "signal"
	Message        string                 `json:"message"`        // Mandatory
	CommandDetails *CommandDetails        `json:"command_details,omitempty"`
	SignalDetails  *SignalDetails         `json:"signal_details,omitempty"`
	// Adicionar outros campos do exemplo do usuário se necessário (risk, confirmation, etc.)
}

type CommandDetails struct {
	Command string `json:"command"` // Mandatory if type is command/internal_command
}

type SignalDetails struct {
	Signal string `json:"signal"`  // Mandatory if type is signal
	TaskID string `json:"task_id"` // Mandatory if type is signal
}

// Agent manages the main interaction loop, terminal, and LLM communication.
type Agent struct {
	termController terminal.Controller
	cmdRegistry    *commands.Registry
	genaiClient    *genai.Client
	chatSession    *genai.ChatSession
	execManager    task.ExecutionManager

	userInputChan chan string
	mu            sync.Mutex
	stopChan      chan struct{}
	cancelContext context.CancelFunc

	lastSubmittedTaskID string
	lastActionFailure   error 
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

	// --- Create Agent Instance ---
	return &Agent{
		cmdRegistry:   registry,
		genaiClient:   client,
		chatSession:   cs,
		execManager:   execMgr,
		userInputChan: make(chan string, 1),
		stopChan:      make(chan struct{}),
	}, nil
}

// Run starts the main agent loop.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	a.cancelContext = cancel
	defer cancel()

	// Defer closing resources
	defer func() {
		if err := a.genaiClient.Close(); err != nil {
			log.Printf("Error closing genai client: %v", err)
		}
		if err := a.execManager.Stop(); err != nil {
			log.Printf("Error stopping execution manager: %v", err)
		}
		if a.termController != nil {
			if err := a.termController.Stop(); err != nil {
				log.Printf("Error stopping terminal controller: %v", err)
			}
		}
	}()

	// 1. Initialize *Main* Terminal Controller
	a.termController = terminal.NewPtyController()
	// SetOutput para o PTY principal - AGORA VAI PARA STDOUT para o usuário ver o REPL
	a.termController.SetOutput(os.Stdout)
	// Start the main terminal with the robust REPL
	if err := a.termController.Start(ctx, ""); err != nil { // Usa shell padrão com REPL
		return fmt.Errorf("failed to start main terminal controller: %w", err)
	}

	// 2. Start reading user input
	go a.readUserInput()

	fmt.Println("Constructo Agent Started. Type your requests or /help.")

	// 3. Main select loop
	for {
		select {
		case <-ctx.Done():
			fmt.Println("Agent context cancelled. Shutting down...")
			return ctx.Err()

		case <-a.stopChan:
			fmt.Println("Agent shutdown requested.")
			return nil

		case line := <-a.userInputChan:
			isInternal, isLog := a.parseUserInput(line)
			if isLog {
				continue
			}
			if !isInternal {
				a.processAgentTurn(ctx, "user_input", line, nil, nil)
			} else {
				// Não logar execução interna aqui, o comando interno pode logar se necessário
				// fmt.Printf("[Executing Internal Command (User): %s]\n", line)
				output, err := a.executeInternalCommand(ctx, line)
				if err != nil {
					if errors.Is(err, commands.ErrExitRequested) {
						fmt.Println(output) 
						a.Stop()
						return nil
					}
					// Imprimir erro e saída para o usuário
					fmt.Fprintf(os.Stderr, "Error executing internal command: %v\n", err)
					fmt.Print(output)
				} else {
					fmt.Print(output) // Mostra a saída diretamente
				}
			}

		case event := <-a.execManager.Events():
			// Manter este log, é útil para saber o que o manager está fazendo
			log.Printf("[TaskManager Event]: ID=%s Type=%s Status=%s Err=%v", event.TaskID, event.EventType, event.Status, event.Error)
			if event.EventType == "completed" {
				taskStatus, err := a.execManager.GetTaskStatus(event.TaskID)
				if err != nil {
					log.Printf("Error getting status for completed task %s: %v", event.TaskID, err) // Manter log de erro
					continue
				}
				a.processAgentTurn(ctx, "task_completed", "", taskStatus, nil)
			} else if event.EventType == "error" {
				log.Printf("Execution manager reported error for task %s: %v", event.TaskID, event.Error) // Manter log de erro
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
		fmt.Print("> ") // Prompt simples, agora vindo do Go, não do REPL do bash
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
			select {
			case a.userInputChan <- line:
			case <-a.stopChan:
				return
			}
		}
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
	commandName := strings.TrimPrefix(parts[0], "/")
	args := parts[1:]

	cmd, exists := a.cmdRegistry.Get(commandName)
	if !exists {
		return fmt.Sprintf("Unknown command: %s", commandName), nil // Retorna erro como output
	}

	outputBuf := new(bytes.Buffer)
	execErr := cmd.Execute(ctx, args, outputBuf)
	
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
	
	// --- Validação UTF-8 da entrada do usuário ---
	if userInput != "" && !utf8.ValidString(userInput) {
		log.Printf("Warning: User input contains invalid UTF-8 sequence: %q", userInput)
		// Tenta limpar a string substituindo caracteres inválidos.
		// Isso pode não ser perfeito, mas evita o erro da API.
		var sanitized strings.Builder
		for _, r := range userInput {
			if r == utf8.RuneError {
				sanitized.WriteRune('\uFFFD') // Unicode replacement character
			} else {
				sanitized.WriteRune(r)
			}
		}
		userInput = sanitized.String()
		log.Printf("Sanitized user input: %q", userInput)
		// Opcional: Informar o usuário sobre a sanitização?
		// fmt.Println("[Agent]: Your input contained invalid characters and was sanitized.")
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
		fmt.Fprintf(contextInfo, "[Action Failed: %v]\n\n", lastFailure)
		log.Printf("Including previous action failure in context: %v", lastFailure)
	}
	// Adiciona resultado do comando interno *deste turno* (se houver)
	if internalResultOutput != nil {
		fmt.Fprintf(contextInfo, "[Internal Command Result]\n")
		output := *internalResultOutput
		if len(output) > 1000 { output = output[:1000] + "\n... (output truncated)" } 
		fmt.Fprintf(contextInfo, "%s\n", output)
		// Erro do comando interno já está implícito na saída (gerado por executeInternalCommand)
		fmt.Fprintf(contextInfo, "[End Internal Command Result]\n\n")
	}

	// Adiciona resumo das tarefas em execução
	runningSummary := a.execManager.GetRunningTasksSummary()
	if runningSummary != "" {
		fmt.Fprintf(contextInfo, "\n%s\n", runningSummary)
	}

	fmt.Fprintf(contextInfo, "Trigger: %s\n", trigger) 

	if userInput != "" {
		fmt.Fprintf(contextInfo, "User Request: %s\n", userInput)
	} else if completedTask != nil {
		// Turno iniciado pela conclusão de uma tarefa
		// Construir uma representação estruturada do resultado da tarefa
		// Idealmente, usaríamos genai.FunctionResponse, mas vamos simular com texto estruturado por enquanto
		// para manter a compatibilidade com o chatSession simples.
		// Futuramente: Mudar para Function Calling/Tool Use real.
		
		taskResultText := bytes.NewBufferString("")
		fmt.Fprintf(taskResultText, "[Task Result - ID: %s]\n", completedTask.ID)
		fmt.Fprintf(taskResultText, "Command: %s\n", completedTask.CommandString)
		fmt.Fprintf(taskResultText, "Status: %s (Exit Code: %d)\n", completedTask.Status, completedTask.ExitCode)
		if completedTask.Error != nil {
			fmt.Fprintf(taskResultText, "Error: Execution failed\n") // Info simples para LLM
		}
		output := completedTask.OutputBuffer.String()
		if len(output) > 2000 { 
			output = output[:2000] + "\n... (output truncated)"
		}
		fmt.Fprintf(taskResultText, "Output:\n%s\n", output)
		fmt.Fprintf(taskResultText, "[End Task Result - ID: %s]", completedTask.ID)
		
		// Adicionar como uma parte de 'tool' ou 'function' simulada (usando role 'user' por enquanto)
		currentTurnParts = append(currentTurnParts, genai.Text(taskResultText.String()))
		log.Printf("--- Sending Task Result to LLM ---")
		log.Printf("%s", taskResultText.String())
		log.Printf("--- End Task Result ---")
	} else if internalResultOutput == nil && userInput == "" { // Modificado: só aviso se não houver NADA
		log.Println("Warning: processAgentTurn called without user input or task/internal result.")
		return 
	}

	// Adiciona o contexto construído como genai.Text
	builtContext := contextInfo.String()
	if builtContext != "" {
		currentTurnParts = append(currentTurnParts, genai.Text(builtContext))
	} else {
		log.Println("[Debug] Skipping agent turn: No effective prompt parts generated.")
		return
	}

	if len(currentTurnParts) == 0 {
	    log.Println("[Debug] Skipping agent turn: No effective prompt parts to send.")
	    return
	}

	// 2. Call LLM
	fmt.Println("[Calling Gemini...]")
	resp, err := a.chatSession.SendMessage(ctx, currentTurnParts...)
	if err != nil {
		log.Printf("Error calling Gemini: %v", err)
		// ... (tratamento de erro da API como antes) ...
		var googleAPIErr *googleapi.Error
		if errors.As(err, &googleAPIErr) {
			log.Printf("Google API Error Details: Code=%d, Message=%s, Body=%s",
				googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
			fmt.Fprintf(os.Stderr, "Error communicating with LLM (Code %d): %s\nDetails: %s\n",
				googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
		} else {
			fmt.Fprintf(os.Stderr, "Error communicating with LLM: %v\n", err)
		}
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
			jsonBlock, err := extractJsonBlock(rawResponseText)
			if err != nil {
				log.Printf("Error extracting JSON from LLM response: %v", err)
			} else {
				if err := json.Unmarshal([]byte(jsonBlock), &parsedResp); err == nil {
					// Validação básica da estrutura parseada
					if parsedResp.Type == "" || parsedResp.Message == "" {
						log.Printf("Warning: Parsed JSON missing mandatory fields 'type' or 'message'. JSON: %s", jsonBlock)
					} else {
						processed = true
					}
				} else {
					log.Printf("Error unmarshaling extracted JSON block: %v\nJSON Block:\n%s", err, jsonBlock)
				}
			}
		} else {
			log.Printf("Warning: Expected genai.Text part in LLM response, but got %T", resp.Candidates[0].Content.Parts[0])
		}
	} else {
		log.Printf("Warning: Could not process LLM response structure. Resp: %+v", resp)
		// Logar feedback se houver
		if resp != nil && resp.PromptFeedback != nil {
			log.Printf("Prompt Feedback: BlockReason=%v, SafetyRatings=%+v", resp.PromptFeedback.BlockReason, resp.PromptFeedback.SafetyRatings)
		}
		if resp != nil && len(resp.Candidates) > 0 {
			 log.Printf("Candidate[0] FinishReason: %v, SafetyRatings: %+v", resp.Candidates[0].FinishReason, resp.Candidates[0].SafetyRatings)
		}
	}

	if !processed {
		log.Println("Warning: Failed to process LLM response into valid JSON structure.")
		// Gera uma mensagem de erro padrão se o parse falhar e não houver mensagem
		if parsedResp.Message == "" {
			parsedResp.Message = fmt.Sprintf("[Agent Error: Could not understand the response format. Please check agent logs. Raw response: %s]", rawResponseText) 
			if len(parsedResp.Message) > 500 { // Truncar raw response no erro
				parsedResp.Message = parsedResp.Message[:500] + "...]"
			}
		}
		parsedResp.Type = "response" // Força tipo response em caso de erro de parse
	}

	// 4. Execute Action
	if parsedResp.Message != "" {
		fmt.Printf("[Agent]: %s\n", parsedResp.Message)
	}

	switch parsedResp.Type {
	case "command", "internal_command":
		if parsedResp.CommandDetails == nil || parsedResp.CommandDetails.Command == "" {
			log.Println("Error: LLM response type is command/internal_command but command_details.command is missing or empty.")
			fmt.Println("[Agent]: I intended to run a command, but I didn't specify which one correctly.")
			break
		}
		cmdToRun := parsedResp.CommandDetails.Command
		if parsedResp.Type == "internal_command" {
			// --- LLM solicitou comando interno --- 
			fmt.Printf("[Executing Internal Command (LLM): %s]\n", cmdToRun)
			output, err := a.executeInternalCommand(ctx, cmdToRun)
			
			if err != nil {
				if errors.Is(err, commands.ErrExitRequested) {
					fmt.Println(output) 
					a.Stop()
					return 
				}
				log.Printf("Error executing internal command requested by LLM: %v", err)
				// O erro está incluído na 'output', que será enviada na chamada recursiva
			}
			
			// <<< CHAMADA RECURSIVA >>>
			// Chama processAgentTurn imediatamente com o resultado do comando interno
			log.Printf("Internal command executed, immediately calling agent turn with its result.")
			a.processAgentTurn(ctx, "internal_command_result", "", nil, &output)
			return // Retorna para não continuar o fluxo normal deste turno

		} else {
			// --- LLM solicitou comando externo --- 
			fmt.Printf("[Submitting Task: %s]\n", cmdToRun)
			isInteractive := false // TODO: decideIfInteractive
			taskID, err := a.execManager.SubmitTask(ctx, cmdToRun, isInteractive)
			if err != nil {
				failureMsg := fmt.Errorf("SubmitTask failed for command '%s': %w", cmdToRun, err)
				fmt.Fprintf(os.Stderr, "%v\n", failureMsg)
				fmt.Printf("[Agent]: Failed to start command: %s\n", cmdToRun)
				a.mu.Lock()
				a.lastActionFailure = failureMsg // Guarda a falha
				a.mu.Unlock()
			} else {
				fmt.Printf("[Task %s Submitted]\n", taskID)
				a.mu.Lock()
				a.lastSubmittedTaskID = taskID 
				a.mu.Unlock()
			}
		}
	case "signal":
		if parsedResp.SignalDetails == nil || parsedResp.SignalDetails.Signal == "" || parsedResp.SignalDetails.TaskID == "" {
			log.Println("Error: LLM response type is signal but signal_details or its fields are missing or empty.")
			fmt.Println("[Agent]: I intended to send a signal, but I didn't specify the signal or target task correctly.")
			break
		}
		signalName := parsedResp.SignalDetails.Signal
		targetTaskID := parsedResp.SignalDetails.TaskID
		sig := mapSignal(signalName)
		if sig == nil {
			fmt.Fprintf(os.Stderr, "Unknown signal in LLM response: %s\n", signalName)
			fmt.Printf("[Agent]: I don't recognize the signal '%s'.\n", signalName)
			break
		}
		
		fmt.Printf("[Sending Signal %v to Task %s]\n", sig, targetTaskID)
		err := a.execManager.SendSignalToTask(targetTaskID, sig)
		if err != nil {
			failureMsg := fmt.Errorf("SendSignal(%v) failed for task '%s': %w", sig, targetTaskID, err)
			fmt.Fprintf(os.Stderr, "%v\n", failureMsg)
			fmt.Printf("[Agent]: Failed to send signal %s to task %s. %v\n", signalName, targetTaskID, err)
			a.mu.Lock()
			a.lastActionFailure = failureMsg // Guarda a falha
			a.mu.Unlock()
		} 
	case "response":
		// Nenhuma ação adicional necessária, apenas a mensagem já foi impressa.
		break
	default:
		log.Printf("Warning: Unknown LLM response type: %s", parsedResp.Type)
		fmt.Printf("[Agent]: I generated an unexpected response type: %s\n", parsedResp.Type)
	}

	// 5. Update history é tratado automaticamente pelo chatSession.SendMessage

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