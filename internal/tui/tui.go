package tui

import (
	"fmt"
	"log"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	// Usaremos interfaces para evitar import cycle direto com agent
	"github.com/rafabd1/Constructo/internal/task"
	"github.com/rafabd1/Constructo/pkg/events"
)

// AgentController define a interface mínima que a TUI precisa do Agent
type AgentController interface {
	// ProcessUserInput recebe a linha de input e dispara o processamento
	ProcessUserInput(input string) 
	// Stop sinaliza para o agente parar (pode ser necessário para Ctrl+C)
	Stop() 
	SetProgram(p *tea.Program)
}

// Model representa o estado da nossa TUI
type Model struct {
	viewport    viewport.Model
	textarea    textarea.Model
	messages    []string
	agent       AgentController // Referência ao core do agente via interface
	taskManager task.ExecutionManager // Referência ao gerenciador de tarefas
	senderStyle lipgloss.Style
	agentStyle  lipgloss.Style
	errorStyle  lipgloss.Style
	helpStyle   lipgloss.Style
	taskStyle   lipgloss.Style // Estilo para eventos de task
	ready       bool
	// Adicionar estado para barra de status
	statusMessage string
}

// --- Mensagens Customizadas (tea.Msg) ---

// AgentOutputMsg é enviada quando o Agent tem uma mensagem para exibir
type AgentOutputMsg struct{
	Content string
}

// TaskEventMsg é enviada quando o TaskManager dispara um evento
type TaskEventMsg struct{
	Event task.TaskEvent
}

// --- Fim Mensagens Customizadas ---

// Init é a primeira função executada quando o programa Bubble Tea inicia.
func (m *Model) Init() tea.Cmd {
	// Por enquanto, só precisamos que o textarea pisque.
	return textarea.Blink
}

// Update lida com mensagens (eventos) e atualiza o modelo.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	log.Printf("[TUI Update]: Received msg Type: %T, Value: %+v", msg, msg)

	var ( 
		vpCmd tea.Cmd
		taCmd tea.Cmd
	)

	// Atualizar viewport e textarea. Note que Update retorna novos models.
	m.viewport, vpCmd = m.viewport.Update(msg)
	m.textarea, taCmd = m.textarea.Update(msg)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			log.Println("Ctrl+C pressed. Requesting agent stop...")
			m.agent.Stop() // Sinaliza ao agente para parar
			return m, tea.Quit
		case tea.KeyEnter:
			input := strings.TrimSpace(m.textarea.Value())
			if input != "" {
				m.messages = append(m.messages, m.senderStyle.Render("You: ")+input)
				m.viewport.SetContent(strings.Join(m.messages, "\n"))
				
				if m.agent == nil {
					log.Println("[TUI Update Enter]: ERROR - m.agent is nil!")
				} else {
					log.Printf("[TUI Update Enter]: Agent reference is valid. Attempting to call ProcessUserInput for: %q", input)
					go m.agent.ProcessUserInput(input)
					log.Printf("[TUI Update Enter]: Goroutine for ProcessUserInput launched.")
				}
				
				m.textarea.Reset()
				// m.viewport.GotoBottom() // Manter comentado por enquanto
			}
		}

	case tea.WindowSizeMsg: // Mensagem quando o tamanho do terminal muda
		headerHeight := lipgloss.Height(m.headerView())
		footerHeight := lipgloss.Height(m.footerView())
		verticalMarginHeight := headerHeight + footerHeight

		if !m.ready { // Primeira vez que recebemos o tamanho
			// Inicializar o viewport com o tamanho correto
			m.viewport = viewport.New(msg.Width, msg.Height-verticalMarginHeight)
			m.viewport.YPosition = headerHeight
			m.viewport.HighPerformanceRendering = false // Ou true se tiver problemas de performance
			m.viewport.SetContent(strings.Join(m.messages, "\n"))
			m.ready = true
		} else { // Redimensionamento subsequente
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - verticalMarginHeight
		}
		// TODO: Ajustar tamanho do textarea também se necessário
		m.textarea.SetWidth(msg.Width)

	// Case para receber mensagens do Agent
	case events.AgentOutputMsg:
		log.Printf("[TUI Received AgentMsg]: %q", msg.Content)
		// Determinar o estilo baseado no conteúdo (simplificado)
		style := m.agentStyle
		if strings.HasPrefix(msg.Content, "[Agent Error]") || strings.HasPrefix(msg.Content, "Error executing command:") {
			style = m.errorStyle
		} else if strings.HasPrefix(msg.Content, "[Executing Internal Command:") || strings.HasPrefix(msg.Content, "[Submitting Task:") || strings.HasPrefix(msg.Content, "[Sending Signal:") {
			style = m.helpStyle // Usar estilo de ajuda/info para ações
		}
		
		m.messages = append(m.messages, style.Render(msg.Content))
		m.viewport.SetContent(strings.Join(m.messages, "\n"))
		// m.viewport.GotoBottom() // Manter comentado
		return m, nil

	// Case para receber eventos do TaskManager
	case events.TaskTuiMsg:
		log.Printf("[TUI Received TaskMsg]: Type=%s ID=%s", msg.Event.EventType, msg.Event.TaskID[:8])
		eventStr := formatTaskEvent(msg.Event)
		m.messages = append(m.messages, m.taskStyle.Render(eventStr))
		
		// Exibir Output Buffer em caso de conclusão
		if msg.Event.EventType == "completed" {
			// Buscar o estado completo da tarefa para obter o buffer
			finalTaskStatus, err := m.taskManager.GetTaskStatus(msg.Event.TaskID)
			if err != nil {
				log.Printf("[TUI Update TaskMsg]: Error getting final status for task %s: %v", msg.Event.TaskID, err)
				m.messages = append(m.messages, m.errorStyle.Render(fmt.Sprintf("  (Could not retrieve final output for task %s)", msg.Event.TaskID[:8])))
			} else if finalTaskStatus.OutputBuffer != nil {
				outputStr := finalTaskStatus.OutputBuffer.String()
				if outputStr != "" {
					maxLen := 500 
					separator := "\n-------------------- Task Output End --------------------\n"
					if len(outputStr) > maxLen {
						outputStr = outputStr[:maxLen] + "\n... (output truncated)"
					}
					m.messages = append(m.messages, lipgloss.NewStyle().Faint(true).Render(outputStr)+separator)
				}
			}
		}

		m.viewport.SetContent(strings.Join(m.messages, "\n"))
		// m.viewport.GotoBottom() 
		return m, nil

	// Case para receber sinal de saída do Agent
	case events.ExitTUIMsg:
		log.Println("[TUI Received ExitMsg]: Quit signal received from agent.")
		return m, tea.Quit // Retorna comando para sair do Bubble Tea
	}

	return m, tea.Batch(vpCmd, taCmd)
}

// View renderiza a UI atual.
func (m *Model) View() string {
	if !m.ready {
		return "\n  Initializing..."
	}
	// Junta as partes: Header, Viewport, Footer
	return fmt.Sprintf(
		"%s\n%s\n%s",
		m.headerView(), // Cabeçalho pode incluir status agora
		m.viewport.View(),
		m.footerView(),
	)
}

// headerView renderiza a barra de título (placeholder)
func (m *Model) headerView() string {
	title := lipgloss.NewStyle().Bold(true).Render("Constructo Agent")
	// Adicionar mais informações de status aqui depois
	line := strings.Repeat("─", m.viewport.Width) // Usa largura do viewport
	return lipgloss.JoinVertical(lipgloss.Left, title, line)
}

// footerView renderiza a área de input
func (m *Model) footerView() string {
	return m.textarea.View()
}

// New initializes a new TUI model with default values and dependencies.
func New(agent AgentController, taskMgr task.ExecutionManager) *Model {
	ta := textarea.New()
	ta.Placeholder = "Send a message or type a command..."
	ta.Focus()

	ta.Prompt = "┃ "
	ta.CharLimit = 280 // Exemplo

	ta.SetWidth(50) // Placeholder, será ajustado em WindowSizeMsg
	ta.SetHeight(1) // Input de linha única inicialmente

	// Remover quebra de linha automática do textarea
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetEnabled(false) // Desabilitar Enter padrão

	// Estilos
	senderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	agentStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	taskStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // Cinza escuro para tasks

	log.Printf("[TUI New]: Agent reference received: %T, TaskMgr reference received: %T", agent, taskMgr)
	
	m := Model{
		textarea:    ta,
		messages:    []string{"Welcome to Constructo! Type /help for commands."},
		agent:       agent,
		taskManager: taskMgr,
		senderStyle: senderStyle,
		agentStyle:  agentStyle,
		errorStyle:  errorStyle,
		helpStyle:   helpStyle,
		taskStyle:   taskStyle,
		statusMessage: "Ready.", // Mensagem de status inicial
	}
	return &m // Retorna ponteiro
}

// StartTUI inicializa e executa o programa Bubble Tea.
func StartTUI(agent AgentController, taskManager task.ExecutionManager) error {
	log.Println("[StartTUI]: Entered function.")
	initialModel := New(agent, taskManager)
	log.Println("[StartTUI]: Initial model created.")
	
	// Criar o programa Bubble Tea
	p := tea.NewProgram(initialModel, tea.WithAltScreen(), tea.WithMouseCellMotion())
	log.Println("[StartTUI]: Bubble Tea program created.")

	// <<< INJETAR REFERÊNCIA DO PROGRAMA NO AGENTE >>>
	agent.SetProgram(p)
	log.Println("[StartTUI]: Injected program reference into agent.")

	// Goroutine para encaminhar eventos do TaskManager
	go func() {
		log.Println("[StartTUI]: Starting TaskManager event forwarder...")
		for event := range taskManager.Events() {
			log.Printf("[StartTUI]: Received task event: %s for task %s", event.EventType, event.TaskID[:8])
			p.Send(events.TaskTuiMsg{Event: event}) // <<< Usar events.TaskTuiMsg
		}
		log.Println("[StartTUI]: Task manager event channel closed. Forwarder exiting.")
	}()
	log.Println("[StartTUI]: TaskManager event forwarder goroutine launched.")

	log.Println("[StartTUI]: Calling p.Run()...")
	if _, err := p.Run(); err != nil {
		log.Printf("[StartTUI]: p.Run() exited with error: %v", err)
		return err
	}
	log.Println("[StartTUI]: p.Run() completed without error.")
	return nil
}

// formatTaskEvent (função auxiliar para formatar eventos de task)
func formatTaskEvent(event task.TaskEvent) string {
	switch event.EventType {
	case "started":
		return fmt.Sprintf("[Task Started: %s (%s)]", event.TaskID[:8], event.Status)
	case "completed":
		if event.Error != nil {
			return fmt.Sprintf("[Task Failed: %s (%s, Code:%d, Err:%v)]", event.TaskID[:8], event.Status, event.ExitCode, event.Error)
		} else {
			return fmt.Sprintf("[Task Success: %s (%s, Code:%d)]", event.TaskID[:8], event.Status, event.ExitCode)
		}
	case "error": // Erro do próprio manager
		return fmt.Sprintf("[Task System Error: %s (%v)]", event.TaskID[:8], event.Error)
	// Adicionar case para "output" se implementado
	default:
		return fmt.Sprintf("[Task Event: %s (%s)]", event.TaskID[:8], event.EventType)
	}
} 