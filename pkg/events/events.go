package events

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/rafabd1/Constructo/internal/task"
)

// AgentOutputMsg é uma mensagem tea.Msg enviada pelo Agent para a TUI.
type AgentOutputMsg struct {
	Content string
}

// TaskTuiMsg é uma mensagem tea.Msg enviada quando um evento do TaskManager
// deve ser repassado para a TUI.
type TaskTuiMsg struct {
	Event task.TaskEvent // Reutiliza a definição original por enquanto
	// Poderíamos redefinir TaskEvent aqui se quiséssemos desacoplar completamente
	// ou adicionar campos específicos para a TUI.
}

// ExitTUIMsg é enviada pelo Agent para sinalizar que a TUI deve encerrar.
type ExitTUIMsg struct{}

// Compile-time check to ensure our messages implement tea.Msg
// (Embora structs vazias/simples não precisem de métodos para implementar interface vazia)
var _ tea.Msg = AgentOutputMsg{}
var _ tea.Msg = TaskTuiMsg{}
var _ tea.Msg = ExitTUIMsg{}

// -- Poderíamos mover a definição de TaskEvent para cá também --
/*
type TaskEvent struct {
    TaskID      string
    EventType   string // e.g., "started", "output", "completed", "error"
    Status      TaskStatus // Definir TaskStatus aqui também?
    OutputChunk []byte // Para eventos de "output"
    ExitCode    int    // Para eventos "completed"
    Error       error  // Para eventos "error" ou "completed" com falha
}
*/ 