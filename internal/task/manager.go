package task

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid" // Para gerar IDs únicos
	// Precisaremos potencialmente do terminal Controller para tarefas interativas no futuro
	// "github.com/rafabd1/Constructo/internal/terminal"
)

// TaskStatus representa o estado de uma tarefa.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusSuccess   TaskStatus = "success"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
)

// Task representa uma unidade de trabalho (comando) gerenciada.
type Task struct {
	ID            string
	CommandString string
	IsInteractive bool
	Status        TaskStatus
	StartTime     time.Time
	EndTime       time.Time
	ExitCode      int
	OutputBuffer  *bytes.Buffer // Buffer para stdout e stderr combinados
	Error         error         // Erro durante a execução (não apenas código de saída != 0)

	// Campos internos para gerenciamento
	ctx         context.Context    // Contexto da tarefa para cancelamento
	cancelFunc  context.CancelFunc // Função para cancelar o contexto
	cmd         *exec.Cmd          // Comando para tarefas não interativas
	ptyController interface{}      // Placeholder para terminal.Controller (tarefas interativas)
	mu          sync.RWMutex       // Protege acesso concorrente aos campos da Task
}

// TaskEvent representa uma notificação sobre o estado de uma tarefa.
type TaskEvent struct {
	TaskID      string
	EventType   string // e.g., "started", "output", "completed", "error"
	Status      TaskStatus 
	OutputChunk []byte // Para eventos de "output"
	ExitCode    int    // Para eventos "completed"
	Error       error  // Para eventos "error" ou "completed" com falha
}

// ExecutionManager define a interface para gerenciar a execução de tarefas.
type ExecutionManager interface {
	// SubmitTask inicia a execução de um novo comando.
	// Retorna o ID da tarefa ou um erro se a submissão falhar.
	SubmitTask(ctx context.Context, command string, interactive bool) (string, error)

	// GetTaskStatus retorna o estado atual e a saída acumulada de uma tarefa.
	GetTaskStatus(taskID string) (*Task, error) // Retorna cópia segura ou usa RWMutex

	// SendInputToTask envia dados para a entrada de uma tarefa interativa.
	SendInputToTask(taskID string, input []byte) error

	// SendSignalToTask envia um sinal OS para o processo da tarefa.
	SendSignalToTask(taskID string, sig os.Signal) error

	// CancelTask tenta cancelar uma tarefa em execução.
	CancelTask(taskID string) error

	// Events retorna um canal somente leitura para receber eventos de tarefas.
	Events() <-chan TaskEvent

	// GetRunningTasksSummary retorna um resumo das tarefas atualmente em execução.
	GetRunningTasksSummary() string

	// Start inicia o gerenciador (se necessário para background tasks).
	Start() error 
	// Stop para o gerenciador e limpa os recursos.
	Stop() error
}

// --- Implementação Concreta (Simplificada Inicialmente) ---

type manager struct {
	tasks     map[string]*Task
	tasksMu   sync.RWMutex
	eventChan chan TaskEvent
	stopChan  chan struct{} // Para sinalizar o fim do gerenciador
}

// NewManager cria uma nova instância do ExecutionManager.
func NewManager() ExecutionManager {
	return &manager{
		tasks:     make(map[string]*Task),
		eventChan: make(chan TaskEvent, 10), // Canal com buffer
		stopChan:  make(chan struct{}),
	}
}

func (m *manager) Start() error {
	// No momento, nenhuma goroutine de fundo permanente é necessária
	// mas a estrutura está aqui caso seja preciso no futuro.
	return nil
}

func (m *manager) Stop() error {
	close(m.stopChan)
	// TODO: Cancelar todas as tarefas ativas?
	m.tasksMu.Lock()
	defer m.tasksMu.Unlock()
	for _, task := range m.tasks {
		if task.Status == StatusRunning || task.Status == StatusPending {
			task.cancelFunc() // Tenta cancelar via contexto
		}
	}
	close(m.eventChan) // Fecha o canal de eventos
	return nil
}


func (m *manager) Events() <-chan TaskEvent {
	return m.eventChan
}

// SubmitTask (Implementação Inicial - Apenas Não Interativo)
func (m *manager) SubmitTask(ctx context.Context, command string, interactive bool) (string, error) {
	if interactive {
		// TODO: Implementar lógica para iniciar PTY dedicado
		return "", fmt.Errorf("interactive task execution not yet implemented")
	}

	taskID := uuid.New().String()
	taskCtx, cancelFunc := context.WithCancel(ctx) // Cria um contexto filho para a tarefa

	newTask := &Task{
		ID:            taskID,
		CommandString: command,
		IsInteractive: interactive,
		Status:        StatusPending,
		StartTime:     time.Now(),
		OutputBuffer:  new(bytes.Buffer),
		ctx:           taskCtx,
		cancelFunc:    cancelFunc,
	}

	// Armazena a tarefa antes de iniciar a goroutine
	m.tasksMu.Lock()
	m.tasks[taskID] = newTask
	m.tasksMu.Unlock()

	// Inicia a execução em uma goroutine
	go m.runNonInteractiveTask(newTask)

	return taskID, nil
}

// Goroutine para executar tarefas não interativas
func (m *manager) runNonInteractiveTask(task *Task) {
	
	task.mu.Lock()
	task.Status = StatusRunning
	task.StartTime = time.Now() 
	task.mu.Unlock()

	// Envia evento de início (opcional, mas útil)
	m.sendEvent(TaskEvent{TaskID: task.ID, EventType: "started", Status: StatusRunning})

	// Configura o comando
	// Usamos "bash -c" para permitir pipelines e redirecionamentos simples na string do comando
	cmd := exec.CommandContext(task.ctx, "bash", "-c", task.CommandString)
	cmd.Stdout = task.OutputBuffer // Captura stdout no buffer da tarefa
	cmd.Stderr = task.OutputBuffer // Captura stderr no mesmo buffer

	task.mu.Lock()
	task.cmd = cmd // Armazena a referência ao cmd
	task.mu.Unlock()

	// Executa o comando
	err := cmd.Run() // Bloqueia até o comando terminar

	// Atualiza o estado da tarefa após a conclusão
	task.mu.Lock()
	task.EndTime = time.Now()
	task.Error = err
	if task.ctx.Err() == context.Canceled { // Verifica se foi cancelado
		task.Status = StatusCancelled
	} else if err != nil {
		task.Status = StatusFailed
		if exitErr, ok := err.(*exec.ExitError); ok {
			task.ExitCode = exitErr.ExitCode()
		} else {
			task.ExitCode = -1 // Código de saída indeterminado
		}
	} else {
		task.Status = StatusSuccess
		task.ExitCode = 0
	}
	task.mu.Unlock()
	
	// Envia evento de conclusão
	m.sendEvent(TaskEvent{
		TaskID:    task.ID,
		EventType: "completed",
		Status:    task.Status,
		ExitCode:  task.ExitCode,
		Error:     task.Error,
	})
	
	// Limpa a função de cancelamento para liberar recursos
	task.cancelFunc()
}

// sendEvent envia um evento pelo canal de forma segura.
func (m *manager) sendEvent(event TaskEvent) {
	select {
	case m.eventChan <- event:
	case <-m.stopChan:
		// Gerenciador está parando, não enviar mais eventos
	}
}


// GetTaskStatus (Implementação Segura)
func (m *manager) GetTaskStatus(taskID string) (*Task, error) {
	m.tasksMu.RLock()
	task, exists := m.tasks[taskID]
	m.tasksMu.RUnlock() // Libera leitura o mais rápido possível

	if !exists {
		return nil, fmt.Errorf("task with ID '%s' not found", taskID)
	}

	// Retorna uma cópia segura dos dados relevantes para evitar race conditions
	// ou expor o mutex interno da tarefa.
	task.mu.RLock()
	defer task.mu.RUnlock()

	// Cria uma cópia segura
	statusCopy := &Task{
		ID:            task.ID,
		CommandString: task.CommandString,
		IsInteractive: task.IsInteractive,
		Status:        task.Status,
		StartTime:     task.StartTime,
		EndTime:       task.EndTime,
		ExitCode:      task.ExitCode,
		OutputBuffer:  bytes.NewBuffer(task.OutputBuffer.Bytes()), // Copia o buffer
		Error:         task.Error,
	}
	
	return statusCopy, nil
}


// Implementações restantes (SendInputToTask, SendSignalToTask, CancelTask)
// precisam ser adicionadas e lidarão com tarefas interativas (PTY) e não interativas (exec.Cmd).

func (m *manager) SendInputToTask(taskID string, input []byte) error {
	m.tasksMu.RLock()
	task, exists := m.tasks[taskID]
	m.tasksMu.RUnlock()

	if !exists {
		return fmt.Errorf("task with ID '%s' not found", taskID)
	}

	task.mu.RLock()
	defer task.mu.RUnlock()
	
	if !task.IsInteractive || task.ptyController == nil {
		return fmt.Errorf("task '%s' is not interactive or PTY not initialized", taskID)
	}
	
	// TODO: Implementar escrita no PTYController associado
	// ptyWriter, ok := task.ptyController.(io.Writer)
	// if !ok { ... error }
	// _, err := ptyWriter.Write(input)
	// return err
	
	return fmt.Errorf("SendInputToTask not yet implemented")
}

func (m *manager) SendSignalToTask(taskID string, sig os.Signal) error {
	 m.tasksMu.RLock()
	 task, exists := m.tasks[taskID]
	 m.tasksMu.RUnlock()

	 if !exists {
		 return fmt.Errorf("task with ID '%s' not found", taskID)
	 }
	 
	 task.mu.RLock()
	 defer task.mu.RUnlock()

	 if task.IsInteractive && task.ptyController != nil {
		 // TODO: Implementar envio de sinal via PTYController
		 // signaler, ok := task.ptyController.(interface{ SendSignal(os.Signal) error })
		 // if !ok { ... error }
		 // return signaler.SendSignal(sig)
		 return fmt.Errorf("sending signal to interactive task not yet implemented")
	 } else if !task.IsInteractive && task.cmd != nil && task.cmd.Process != nil {
		 // Enviar sinal para processo não interativo
		 return task.cmd.Process.Signal(sig)
	 } else {
		 return fmt.Errorf("cannot send signal to task '%s': process not running or not available", taskID)
	 }
}

func (m *manager) CancelTask(taskID string) error {
	m.tasksMu.RLock()
	task, exists := m.tasks[taskID]
	m.tasksMu.RUnlock()

	if !exists {
		return fmt.Errorf("task with ID '%s' not found", taskID)
	}

	task.mu.Lock() // Precisa de Lock para modificar status e chamar cancelFunc
	defer task.mu.Unlock()

	if task.Status != StatusRunning && task.Status != StatusPending {
		return fmt.Errorf("task '%s' is not in a cancellable state (%s)", taskID, task.Status)
	}
	
	// Cancela o contexto da tarefa, que deve propagar para exec.CommandContext
	// ou ser verificado no loop do PTY (quando implementado)
	task.cancelFunc() 
	task.Status = StatusCancelled // Marca como cancelado imediatamente
	// O status final e evento serão definidos pela goroutine da tarefa ao detectar ctx.Err()

	return nil
}

// GetRunningTasksSummary retorna um resumo das tarefas atualmente em execução.
func (m *manager) GetRunningTasksSummary() string {
	m.tasksMu.RLock()
	defer m.tasksMu.RUnlock()

	var runningTasks []string
	for id, task := range m.tasks {
		task.mu.RLock() // Leitura segura do status da tarefa individual
		status := task.Status
		cmd := task.CommandString
		task.mu.RUnlock()

		if status == StatusRunning {
			// Limitar o tamanho do comando no resumo para evitar poluir o prompt
			maxCmdLen := 40
			if len(cmd) > maxCmdLen {
				cmd = cmd[:maxCmdLen] + "..."
			}
			runningTasks = append(runningTasks, fmt.Sprintf("[%s: %s]", id, cmd))
		}
	}

	if len(runningTasks) == 0 {
		return ""
	}

	return fmt.Sprintf("Running Tasks: %s", strings.Join(runningTasks, ", "))
} 