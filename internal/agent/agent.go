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

// AgentResponse defines the expected JSON structure from the LLM.
type AgentResponse struct {
	Msg    string `json:"msg,omitempty"`    // Message to display to the user.
	Cmd    string `json:"cmd,omitempty"`    // Command to execute in the terminal.
	Signal string `json:"signal,omitempty"` // OS Signal to send (e.g., "SIGINT").
	TaskID string `json:"task_id,omitempty"` // Optional: Task ID to target for signal/input
}

// Define the schema for the AgentResponse
var agentResponseSchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"msg":    {Type: genai.TypeString, Description: "Message to display to the user."},
		"cmd":    {Type: genai.TypeString, Description: "Command to execute in the terminal. Use standard shell syntax."},
		"signal": {Type: genai.TypeString, Description: "OS Signal to send (e.g., 'SIGINT', 'SIGTERM'). Use with task_id if targeting a specific command."},
		"task_id": {Type: genai.TypeString, Description: "ID of the background task to send a signal or input to."},
	},
}

// InternalCommandResult armazena o resultado da execução de um comando interno.
type InternalCommandResult struct {
	Output string
	Error  error
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
	lastActionFailure error 
	// Guarda o resultado do último comando interno executado para incluir no próximo prompt
	lastInternalResult *InternalCommandResult 
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
	model.ResponseMIMEType = "application/json"
	model.ResponseSchema = agentResponseSchema
	log.Println("Model configured for direct JSON output.")

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
			isInternal, _ := a.parseUserInput(line) // Modificado para retornar se é interno
			if !isInternal {
				// É input para o LLM
				a.processAgentTurn(ctx, "user_input", line, nil, nil)
			} else {
				// Usuário digitou um comando interno diretamente - executar e mostrar output,
				// mas *não* enviar resultado para o LLM (para evitar loop se usuário digitar /task status)
				fmt.Printf("[Executing Internal Command (User): %s]\n", line)
				output, err := a.executeInternalCommand(ctx, line) 
				if err != nil {
					if errors.Is(err, commands.ErrExitRequested) {
						fmt.Println(output) // Mostra a mensagem "Exiting..."
						a.Stop()
						return nil
					}
					fmt.Fprintf(os.Stderr, "Error executing internal command: %v\nOutput:\n%s\n", err, output)
				} else {
					fmt.Print(output) // Mostra a saída do comando interno diretamente
				}
			}

		case event := <-a.execManager.Events():
			log.Printf("[TaskManager Event]: ID=%s Type=%s Status=%s Err=%v", event.TaskID, event.EventType, event.Status, event.Error)
			if event.EventType == "completed" {
				taskStatus, err := a.execManager.GetTaskStatus(event.TaskID)
				if err != nil {
					log.Printf("Error getting status for completed task %s: %v", event.TaskID, err)
					// Decide o que fazer - talvez informar o LLM sobre o erro?
					continue
				}
				a.processAgentTurn(ctx, "task_completed", "", taskStatus, nil) // Passa nil para internalResult
			} else if event.EventType == "error" {
				// Lidar com erros específicos do gerenciador, se houver
				log.Printf("Execution manager reported error for task %s: %v", event.TaskID, event.Error)
				// Talvez informar o LLM
			}
			// TODO: Lidar com eventos "output" se quisermos streaming
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
	// Filtro de log (como antes)
	logPattern := `^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`
	matched, _ := regexp.MatchString(logPattern, line)
	if matched || strings.HasPrefix(line, "[") || strings.Contains(line, "--- Sending") || strings.Contains(line, "Trigger:") || strings.Contains(line, "Task Result - ID:") || strings.Contains(line, "Action Failed:") {
		return false, true // É log, não é comando interno
	}

	if strings.HasPrefix(line, "/") {
		return true, false // É comando interno, não é log
	}

	return false, false // Não é comando interno nem log (é input para LLM)
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

// processAgentTurn collects context, calls LLM, and handles the response.
func (a *Agent) processAgentTurn(ctx context.Context, trigger string, userInput string, completedTask *task.Task, internalResult *InternalCommandResult) {
	
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

	// 1. Construir o prompt para o LLM - AGORA COMO HISTÓRICO DE CONTEÚDO
	var currentTurnParts []genai.Part
	contextInfo := bytes.NewBufferString("") 

	// Incluir falha da ação anterior
	a.mu.Lock()
	lastFailure := a.lastActionFailure
	a.lastActionFailure = nil 
	// Incluir resultado do comando interno anterior
	lastIntResult := a.lastInternalResult
	a.lastInternalResult = nil // Limpa após incluir
	a.mu.Unlock()
	if lastFailure != nil {
		fmt.Fprintf(contextInfo, "[Action Failed: %v]\n\n", lastFailure)
		log.Printf("Including previous action failure in context: %v", lastFailure)
	}
	// Adiciona resultado do comando interno anterior ao contexto
	if lastIntResult != nil {
		fmt.Fprintf(contextInfo, "[Internal Command Result]\n")
		output := lastIntResult.Output
		if len(output) > 1000 { output = output[:1000] + "\n... (output truncated)" } // Limita output
		fmt.Fprintf(contextInfo, "%s\n", output)
		if lastIntResult.Error != nil {
			// Não enviar o erro técnico, apenas a saída que pode conter a mensagem de erro
			// fmt.Fprintf(contextInfo, "Error: %v\n", lastIntResult.Error) 
		}
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
	} else {
		log.Println("Warning: processAgentTurn called without user input or completed task/internal result.")
		return 
	}

	// Adiciona o contexto construído como genai.Text
	builtContext := contextInfo.String()
	if builtContext != "" {
		currentTurnParts = append(currentTurnParts, genai.Text(builtContext))
		log.Printf("--- Sending Prompt Context to LLM ---")
		log.Printf("%s", builtContext)
		log.Printf("--- End Prompt Context ---")
	} else {
		log.Println("[Debug] Skipping agent turn: No effective prompt parts generated.")
		return
	}

	if len(currentTurnParts) == 0 {
	    log.Println("[Debug] Skipping agent turn: No effective prompt parts to send.")
	    return
	}

	// 2. Call LLM
	fmt.Println("[Calling Gemini...] ")

	// Envia as partes da *rodada atual*. O chatSession mantém o histórico anterior.
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

	// 3. Parse LLM Response
	var agentResp AgentResponse
	processed := false
	// ... (lógica de parseamento JSON como antes) ...
	if resp != nil && len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil && len(resp.Candidates[0].Content.Parts) > 0 {
		part := resp.Candidates[0].Content.Parts[0]
		if txt, ok := part.(genai.Text); ok {
			jsonData := []byte(txt)
			if err := json.Unmarshal(jsonData, &agentResp); err == nil {
				processed = true
				fmt.Printf("[LLM Response Parsed: %+v]\n", agentResp)
			} else {
				log.Printf("Error unmarshaling direct JSON response: %v\nJSON received: %s", err, string(jsonData))
			}
		} else {
			log.Printf("Warning: Expected genai.Text part for JSON response, but got %T", part)
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
		log.Println("Warning: Could not process LLM response content.")
		if agentResp.Msg == "" { 
		    agentResp.Msg = "[Agent Error: Could not process response content. Please check agent logs.]"
		}
	}

	// 4. Execute Action
	if agentResp.Msg != "" {
		fmt.Printf("[Agent]: %s\n", agentResp.Msg)
	}

	if agentResp.Cmd != "" {
		if strings.HasPrefix(agentResp.Cmd, "/") {
			// --- LLM solicitou comando interno --- 
			fmt.Printf("[Executing Internal Command (LLM): %s]\n", agentResp.Cmd)
			output, err := a.executeInternalCommand(ctx, agentResp.Cmd)
			
			// Guarda o resultado para o próximo turno
			a.mu.Lock()
			a.lastInternalResult = &InternalCommandResult{Output: output, Error: err}
			a.mu.Unlock()

			if err != nil {
				if errors.Is(err, commands.ErrExitRequested) {
					// LLM pediu para sair
					fmt.Println(output) // Mostra a saída do comando exit
					a.Stop()
					return // Encerra o turno aqui
				}
				// Loga o erro, o resultado (incluindo a msg de erro) será enviado no próximo turno
				log.Printf("Error executing internal command requested by LLM: %v", err)
			}
			// Não fazer mais nada neste turno, esperar o próximo para enviar o resultado

		} else {
			// --- LLM solicitou comando externo --- 
			fmt.Printf("[Submitting Task: %s]\n", agentResp.Cmd)
			isInteractive := false // TODO: decideIfInteractive
			taskID, err := a.execManager.SubmitTask(ctx, agentResp.Cmd, isInteractive)
			if err != nil {
				failureMsg := fmt.Errorf("SubmitTask failed for command '%s': %w", agentResp.Cmd, err)
				fmt.Fprintf(os.Stderr, "%v\n", failureMsg)
				fmt.Printf("[Agent]: Failed to start command: %s\n", agentResp.Cmd)
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
	} else if agentResp.Signal != "" {
		sig := mapSignal(agentResp.Signal)
		if sig == nil {
			fmt.Fprintf(os.Stderr, "Unknown signal in LLM response: %s\n", agentResp.Signal)
			fmt.Printf("[Agent]: I don't recognize the signal '%s'.\n", agentResp.Signal)
			return
		}
		
		targetTaskID := agentResp.TaskID
		if targetTaskID == "" {
			a.mu.Lock()
			targetTaskID = a.lastSubmittedTaskID // Tenta usar a última tarefa se nenhuma for especificada
			a.mu.Unlock()
		}
		if targetTaskID == "" {
			fmt.Fprintf(os.Stderr, "Signal requested but no target task ID specified or known.\n")
			fmt.Printf("[Agent]: I need to know which command (task ID) to send the signal '%s' to.\n", agentResp.Signal)
			return
		}

		fmt.Printf("[Sending Signal %v to Task %s]\n", sig, targetTaskID)
		err := a.execManager.SendSignalToTask(targetTaskID, sig)
		if err != nil {
			failureMsg := fmt.Errorf("SendSignal(%v) failed for task '%s': %w", sig, targetTaskID, err)
			fmt.Fprintf(os.Stderr, "%v\n", failureMsg)
			fmt.Printf("[Agent]: Failed to send signal %s to task %s. %v\n", agentResp.Signal, targetTaskID, err)
			a.mu.Lock()
			a.lastActionFailure = failureMsg // Guarda a falha
			a.mu.Unlock()
		} 
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