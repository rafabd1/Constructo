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
	"github.com/rafabd1/Constructo/internal/terminal"
	"github.com/rafabd1/Constructo/pkg/events"
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

	// Referência ao programa Bubble Tea para enviar mensagens de volta
	program *tea.Program
	
	// Contexto principal do agente para operações em background
	ctx context.Context
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
		stopChan:      make(chan struct{}),
		ctx:           ctx,
		program:       nil,
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
		// Remover log de debug de skip
		// log.Println("[Debug] Skipping agent turn: No effective prompt parts generated.")
		return // Skip turn if no context generated
	}

	if len(currentTurnParts) == 0 {
	    // Remover log de debug de skip
		// log.Println("[Debug] Skipping agent turn: No effective prompt parts to send.")
	    return
	}

	// 2. Call LLM
	a.sendToTUI(events.AgentOutputMsg{Content: "[Calling Gemini...]"}) // Enviar status para TUI
	resp, err := a.chatSession.SendMessage(ctx, currentTurnParts...)
	if err != nil {
		errMsg := fmt.Sprintf("Error calling LLM: %v", err)
		log.Printf("%s", errMsg)
		// Tentar extrair detalhes do erro da API
		var googleAPIErr *googleapi.Error
		if errors.As(err, &googleAPIErr) {
			errMsg = fmt.Sprintf("%s\nDetails: Code=%d, Message=%s, Body=%s", errMsg, googleAPIErr.Code, googleAPIErr.Message, string(googleAPIErr.Body))
		}
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: %s", errMsg)}) // Enviar erro para TUI
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
			parsedResp.Message = fmt.Sprintf("[Agent Error: Could not understand the response format. Raw: %s]", rawResponseText) 
			if len(parsedResp.Message) > 500 { // Truncar raw response no erro
				parsedResp.Message = parsedResp.Message[:500] + "...]"
			}
		}
		parsedResp.Type = "response" // Força tipo response em caso de erro de parse
	}

	// 4. Execute Action
	if parsedResp.Message != "" {
		a.sendToTUI(events.AgentOutputMsg{Content: parsedResp.Message})
	}

	switch parsedResp.Type {
	case "command", "internal_command":
		if parsedResp.CommandDetails == nil || parsedResp.CommandDetails.Command == "" {
			log.Println("Error: LLM response type is command/internal_command but command_details.command is missing or empty.")
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to run command but didn't specify which."}) 
			break
		}
		cmdToRun := parsedResp.CommandDetails.Command
		if parsedResp.Type == "internal_command" {
			// --- LLM solicitou comando interno --- 
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Executing Internal Command: %s]", cmdToRun)})
			output, err := a.executeInternalCommand(ctx, cmdToRun)
			
			if err != nil {
				if errors.Is(err, commands.ErrExitRequested) {
					// Envia output ("Exiting...") e pede para parar
					a.sendToTUI(events.AgentOutputMsg{Content: output})
					a.Stop()
					return 
				}
				log.Printf("Error executing internal command requested by LLM: %v", err)
				// O erro é incluído na output que vai para a chamada recursiva
			}
			
			// Chama processAgentTurn imediatamente com o resultado
			log.Printf("Internal command executed, immediately calling agent turn with its result.")
			a.processAgentTurn(ctx, "internal_command_result", "", nil, &output)
			return

		} else {
			// --- LLM solicitou comando externo --- 
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Submitting Task: %s]", cmdToRun)})
			isInteractive := false // TODO
			taskID, err := a.execManager.SubmitTask(ctx, cmdToRun, isInteractive)
			if err != nil {
				failureMsg := fmt.Errorf("SubmitTask failed for command '%s': %w", cmdToRun, err)
				log.Printf("%v", failureMsg) // Log interno
				a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: Failed to start command: %s]", cmdToRun)}) // Para TUI
				a.mu.Lock()
				a.lastActionFailure = failureMsg
				a.mu.Unlock()
			} else {
				// Não precisamos enviar [Task Submitted] para a TUI,
				// o evento "started" do TaskManager fará isso.
				a.mu.Lock()
				a.lastSubmittedTaskID = taskID 
				a.mu.Unlock()
			}
		}
	case "signal":
		if parsedResp.SignalDetails == nil || parsedResp.SignalDetails.Signal == "" || parsedResp.SignalDetails.TaskID == "" {
			a.sendToTUI(events.AgentOutputMsg{Content: "[Agent Error]: Tried to send signal but didn't specify signal or target."}) 
			break
		}
		signalName := parsedResp.SignalDetails.Signal
		targetTaskID := parsedResp.SignalDetails.TaskID
		sig := mapSignal(signalName)
		if sig == nil {
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: Unknown signal '%s'.", signalName)})
			break
		}
		a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Sending Signal %v to Task %s]", sig, targetTaskID)})
		err := a.execManager.SendSignalToTask(targetTaskID, sig)
		if err != nil {
			failureMsg := fmt.Errorf("SendSignal(%v) failed for task '%s': %w", sig, targetTaskID, err)
			log.Printf("%v", failureMsg)
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Agent Error]: Failed to send signal %s to task %s. %v]", signalName, targetTaskID, err)})
			a.mu.Lock()
			a.lastActionFailure = failureMsg 
			a.mu.Unlock()
		}
	case "response":
		// Mensagem já enviada no início do passo 4.
		break
	default:
		unknownTypeMsg := fmt.Sprintf("[Agent Error]: Received unknown response type from LLM: %s", parsedResp.Type)
		log.Printf("%s", unknownTypeMsg)
		//a.sendToTUI(AgentOutputMsg{Content: unknownTypeMsg})
	}
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
			a.sendToTUI(events.AgentOutputMsg{Content: fmt.Sprintf("[Executing Internal Command: %s]", input)}) // Informa TUI
			output, err := a.executeInternalCommand(a.ctx, input)
			if err != nil {
				if errors.Is(err, commands.ErrExitRequested) {
					a.sendToTUI(events.AgentOutputMsg{Content: output})
					a.Stop()
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