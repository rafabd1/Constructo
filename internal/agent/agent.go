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
	geminiModelName = "gemini-2.0-flash"
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

	// Rastreia a última tarefa iniciada (simplificação inicial)
	lastSubmittedTaskID string
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
	if modelName == "" {
		modelName = geminiModelName // Fallback para constante
	}
	log.Printf("Using model: %s", modelName)

	var client *genai.Client
	if apiKey != "" {
		log.Println("Initializing Gemini client with API Key from config.")
		client, err = genai.NewClient(ctx, option.WithAPIKey(apiKey))
	} else {
		log.Println("API Key not found in config. Attempting to use default credentials (ADC).")
		client, err = genai.NewClient(ctx)
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

	// 3. Main select loop - Agora orientado a eventos
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
			if !a.handleUserInput(ctx, line) {
				// Se handleUserInput retornar false (não foi comando interno nem log)
				// É uma requisição para o LLM
				a.processAgentTurn(ctx, "user_input", line, nil)
			}

		case event := <-a.execManager.Events():
			log.Printf("[TaskManager Event]: ID=%s Type=%s Status=%s Err=%v", event.TaskID, event.EventType, event.Status, event.Error)
			if event.EventType == "completed" {
				// Pega o estado final da tarefa para incluir a saída
				taskStatus, err := a.execManager.GetTaskStatus(event.TaskID)
				if err != nil {
					log.Printf("Error getting status for completed task %s: %v", event.TaskID, err)
					// Decide o que fazer - talvez informar o LLM sobre o erro?
					continue
				}
				// Chama o LLM com o resultado da tarefa
				a.processAgentTurn(ctx, "task_completed", "", taskStatus)
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

// handleUserInput checks for internal commands or log feedback.
// Returns true if the input was handled internally (command or ignored log).
// Returns false if it's likely a request for the LLM.
func (a *Agent) handleUserInput(ctx context.Context, line string) bool {
	// Filtro de log
	logPattern := `^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`
	matched, _ := regexp.MatchString(logPattern, line)
	if matched || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "User Request:") || strings.HasPrefix(line, "Event Trigger:") || strings.HasPrefix(line, "Configuration loaded") || strings.HasPrefix(line, "Starting Constructo Agent") || strings.HasPrefix(line, "Constructo Agent Started") || strings.HasPrefix(line, "Error calling Gemini") || strings.HasPrefix(line, "Google API Error Details"){
		// log.Printf("[Debug] Ignored potential log feedback line: %q", line)
		return true // Ignorado, considerado "tratado"
	}

	// Verifica comandos internos
	if strings.HasPrefix(line, "/") {
		parts := strings.Fields(line)
		commandName := strings.TrimPrefix(parts[0], "/")
		args := parts[1:]

		cmd, exists := a.cmdRegistry.Get(commandName)
		if !exists {
			fmt.Printf("Unknown command: %s\n", commandName)
			return true // Tratado (mostrando erro)
		}

		fmt.Printf("[Executing Internal Command: /%s]\n", commandName)
		err := cmd.Execute(ctx, args)
		if err != nil {
			fmt.Printf("Error executing command /%s: %v\n", commandName, err)
		}
		// TODO: Comandos internos podem precisar interagir com o ExecutionManager também?
		return true // Comando interno tratado
	}

	// Se não for log e não for comando interno, é para o LLM
	return false
}

// processAgentTurn collects context, calls LLM, and handles the response.
// Agora recebe o input do usuário OU o resultado de uma tarefa concluída.
func (a *Agent) processAgentTurn(ctx context.Context, trigger string, userInput string, completedTask *task.Task) {
	
	// 1. Construir o prompt para o LLM - AGORA COMO HISTÓRICO DE CONTEÚDO
	var currentTurnParts []genai.Part

	if userInput != "" {
		// Turno iniciado pelo usuário
		currentTurnParts = append(currentTurnParts, genai.Text(userInput))
		log.Printf("--- Sending User Input to LLM ---")
		log.Printf("User: %s", userInput)
		log.Printf("--- End User Input ---")
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
		log.Println("Warning: processAgentTurn called without user input or completed task.")
		return // Nada a fazer
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
		fmt.Printf("[Submitting Task: %s]\n", agentResp.Cmd)
		// TODO: Implementar heurística decideIfInteractive
		isInteractive := false // Por enquanto, assumir não interativo
		taskID, err := a.execManager.SubmitTask(ctx, agentResp.Cmd, isInteractive)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error submitting task: %v\n", err)
			// Informar o usuário sobre a falha na submissão
			fmt.Printf("[Agent]: Failed to start command: %s\n", agentResp.Cmd)
		} else {
			fmt.Printf("[Task %s Submitted]\n", taskID)
			a.mu.Lock()
			a.lastSubmittedTaskID = taskID // Guarda o ID da última tarefa
			a.mu.Unlock()
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
			fmt.Fprintf(os.Stderr, "Error sending signal to task %s: %v\n", targetTaskID, err)
			fmt.Printf("[Agent]: Failed to send signal %s to task %s. It might have already finished.\n", agentResp.Signal, targetTaskID)
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