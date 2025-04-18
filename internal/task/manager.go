package task

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

// Start starts the background task monitoring.
func (m *manager) Start() error {
	// go m.monitorTasks() // Removido - funcionalidade não implementada ou removida
	return nil
}

// Stop signals the monitoring goroutine to stop and waits for tasks to finish (or times out).
func (m *manager) Stop() error {
	close(m.stopChan)
	// Lógica original de Stop (sem wait group ou timeout específico aqui,
	// a responsabilidade de esperar por tarefas pode ser externa ou gerenciada
	// de outra forma se necessário no futuro)
	// Por exemplo, fechar o canal de eventos se ele for usado para sinalizar conclusão geral.
	// close(m.eventChan)
	log.Println("Task manager stop requested.")
	return nil
}

func (m *manager) Events() <-chan TaskEvent {
	return m.eventChan
}

// SubmitTask starts a new command as a background task.
func (m *manager) SubmitTask(ctx context.Context, command string, interactive bool) (string, error) {
	if interactive {
		return "", fmt.Errorf("interactive tasks not yet supported by task manager")
	}

	args := strings.Fields(command)
	if len(args) == 0 {
		return "", fmt.Errorf("command cannot be empty")
	}

	cmdName := args[0]
	cmdArgs := args[1:]

	// Gerar um ID único para a tarefa
	taskID := uuid.NewString()

	// Criar contexto para a tarefa que pode ser cancelado
	taskCtx, cancel := context.WithCancel(ctx) // Usa o contexto pai

	t := &Task{
		ID:            taskID,
		CommandString: command,
		Status:        StatusPending,
		cancelFunc:    cancel,
		StartTime:     time.Now(), // Marca o tempo de submissão
	}

	// Log de submissão
	log.Printf("Submitting task %s: %s", t.ID, t.CommandString)

	// Adiciona a tarefa ao mapa antes de iniciar a goroutine
	m.tasksMu.Lock()
	m.tasks[taskID] = t
	m.tasksMu.Unlock()

	// Cria o comando
	cmd := exec.CommandContext(taskCtx, cmdName, cmdArgs...)

	// Buffer para capturar stdout e stderr combinados
	outputBuf := new(bytes.Buffer)
	cmd.Stdout = outputBuf
	cmd.Stderr = outputBuf

	// Inicia o comando em uma nova goroutine para não bloquear
	go func() {
		defer func() {
			t.EndTime = time.Now()
			// Atualiza status final e envia evento
			m.tasksMu.Lock()
			if t.Status == StatusRunning { // Só muda se ainda estava Running
				if t.Error == nil {
					t.Status = StatusSuccess
				} else {
					t.Status = StatusFailed
				}
			}
			// Guarda o buffer final
			t.OutputBuffer = outputBuf
			m.tasksMu.Unlock()

			m.sendEvent(TaskEvent{
				TaskID:    t.ID,
				EventType: "completed",
				Status:    t.Status,
				ExitCode:  t.ExitCode,
				Error:     t.Error, // Pode ser nil
			})
		}()

		// Atualiza o status para Running logo antes de Start()
		m.tasksMu.Lock()
		t.Status = StatusRunning
		m.tasksMu.Unlock()
		m.sendEvent(TaskEvent{
			TaskID:    t.ID,
			EventType: "started",
			Status:    StatusRunning,
		})

		// Logar o início da execução da tarefa
		log.Printf("Executing task %s: %s", t.ID, t.CommandString)
		err := cmd.Start()
		if err != nil {
			t.Error = fmt.Errorf("failed to start command: %w", err)
			log.Printf("Error starting task %s: %v", t.ID, t.Error)
			// Status será atualizado para Errored no defer principal
			m.tasksMu.Lock()
			t.Status = StatusFailed
			m.tasksMu.Unlock()
			return
		}

		t.cmd = cmd // Armazena a referência ao cmd

		// Aguarda a conclusão do comando
		err = cmd.Wait()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				t.ExitCode = exitErr.ExitCode()
				t.Error = fmt.Errorf("command finished with non-zero exit code %d", t.ExitCode)
				log.Printf("Task %s finished with error: %v", t.ID, err)
			} else {
				t.Error = fmt.Errorf("command wait failed: %w", err)
				log.Printf("Error waiting for task %s: %v", t.ID, t.Error)
			}
			// Status será atualizado no defer principal
			m.tasksMu.Lock()
			t.Status = StatusFailed
			m.tasksMu.Unlock()
		} else {
			t.ExitCode = 0
			// Logar conclusão bem-sucedida
			log.Printf("Task %s completed successfully (Exit Code 0)", t.ID)
			// Status será atualizado no defer principal (para Completed)
			m.tasksMu.Lock()
			t.Status = StatusSuccess
			m.tasksMu.Unlock()
		}
	}()

	return t.ID, nil
}

// GetTaskStatus retorna o status de uma tarefa específica.
func (m *manager) GetTaskStatus(taskID string) (*Task, error) {
	m.tasksMu.RLock()
	defer m.tasksMu.RUnlock()
	task, exists := m.tasks[taskID]
	if !exists {
		return nil, fmt.Errorf("task with ID %s not found", taskID)
	}
	// Retorna uma cópia para evitar modificação externa do estado interno
	// (Cuidado: OutputBuffer ainda é compartilhado se for ponteiro)
	copyTask := *task
	if task.OutputBuffer != nil {
		// Cria uma cópia do buffer se existir
		bufCopy := bytes.NewBuffer(task.OutputBuffer.Bytes())
		copyTask.OutputBuffer = bufCopy
	}
	return &copyTask, nil
}

// SendInputToTask envia dados para a entrada de uma tarefa interativa.
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

// SendSignalToTask envia um sinal OS para uma tarefa em execução.
func (m *manager) SendSignalToTask(taskID string, signal os.Signal) error {
	m.tasksMu.RLock()
	task, exists := m.tasks[taskID]
	if !exists {
		m.tasksMu.RUnlock()
		return fmt.Errorf("task with ID %s not found", taskID)
	}
	pid := task.cmd.Process.Pid
	status := task.Status
	m.tasksMu.RUnlock()

	if status != StatusRunning {
		return fmt.Errorf("task %s is not running (status: %s)", taskID, status)
	}
	if pid <= 0 {
		return fmt.Errorf("task %s has an invalid PID (%d)", taskID, pid)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		// Processo pode já ter terminado entre a leitura do status e aqui
		log.Printf("Could not find process for task %s (PID: %d): %v", taskID, pid, err)
		return fmt.Errorf("could not find process for task %s (PID: %d): %w", taskID, pid, err)
	}

	log.Printf("Sending signal %v to task %s (PID: %d)", signal, taskID, pid)
	err = process.Signal(signal)
	if err != nil {
		log.Printf("Error sending signal %v to task %s (PID: %d): %v", signal, taskID, pid, err)
		return fmt.Errorf("failed to send signal %v to task %s (PID: %d): %w", signal, taskID, pid, err)
	}
	return nil
}

// CancelTask tenta cancelar uma tarefa em execução.
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

// sendEvent envia um evento para o canal de eventos (sem lock, pois o canal é thread-safe).
func (m *manager) sendEvent(event TaskEvent) {
	select {
	case m.eventChan <- event:
	case <-m.stopChan:
		log.Println("Manager stopped, discarding event:", event.EventType, event.TaskID)
	default:
		// Opcional: Logar se o canal de eventos estiver cheio
		log.Printf("Warning: Event channel full, discarding event for task %s (%s)", event.TaskID, event.EventType)
	}
} 