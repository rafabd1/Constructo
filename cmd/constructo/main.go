package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	syscall "syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rafabd1/Constructo/internal/agent"
	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/config"
	"github.com/rafabd1/Constructo/internal/task"
	"github.com/rafabd1/Constructo/internal/tui"
	// TODO: Import other command packages if they are separate
)

// Variável global temporária para armazenar o manager (melhorar com injeção de dependência depois)
// var globalExecManager task.ExecutionManager

func main() {
	f, err := tea.LogToFile("constructo-debug.log", "debug")
	if err != nil {
		fmt.Println("fatal: could not open log file:", err)
		os.Exit(1)
	}
	defer f.Close()

	log.Println("--- Starting Constructo Agent --- TUI Mode ---")
	log.Println("Log file configured.")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 0. Load Configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	if cfg.LLM.ModelName == "" { // Certificar que o nome do modelo está presente
		log.Fatalf("Fatal: llm.model_name is missing in the configuration.")
	}
	log.Printf("Configuration loaded. Using model: %s", cfg.LLM.ModelName)

	// --- REMOVIDO: Canal tuiChan não é mais necessário aqui ---
	// tuiChan := make(chan tea.Msg, 10)
	// -------------------------------------------

	// 1. Create Command Registry
	registry := commands.NewRegistry()

	// 2. Create Agent (Não precisa mais do tuiChan)
	constructoAgent, err := agent.NewAgent(ctx, cfg, registry)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}
	execManager := constructoAgent.GetExecutionManager()
	if execManager == nil {
		log.Fatalf("Failed to get execution manager from agent")
	}

	// 3. Register Commands (Needs registry and manager provider)
	helpCmd := &commands.HelpCmd{Registry: registry}
	if err := registry.Register(helpCmd); err != nil {
		log.Fatalf("Failed to register help command: %v", err)
	}
	getManager := func() task.ExecutionManager {
		return execManager
	}
	cmdsToRegister := []commands.Command{
		&commands.TaskCmd{ExecManagerProvider: getManager},
		&commands.MemoryCmd{},
		&commands.ReasonCmd{},
		&commands.JudgeCmd{},
		&commands.ResetCmd{},
		&commands.ConfigCmd{},
		&commands.ProfileCmd{},
		&commands.ExportCmd{},
		&commands.ExitCmd{},
	}
	for _, cmd := range cmdsToRegister {
		if err := registry.Register(cmd); err != nil {
			log.Fatalf("Failed to register command %s: %v", cmd.Name(), err)
		}
	}

	log.Println("Agent and commands initialized successfully.")
	log.Println("Attempting to start TUI...")

	// 4. Run the TUI
	log.Println("Starting TUI...")
	// StartTUI só precisa do agent e manager
	if err := tui.StartTUI(constructoAgent, execManager); err != nil {
		log.Printf("TUI exited with error: %v", err)
		os.Exit(1)
	}

	// O programa TUI agora controla o loop principal.
	// A lógica de shutdown precisa ser gerenciada dentro da TUI ou pelo Agent.
	fmt.Println("Constructo Agent stopped gracefully.")
	os.Exit(0)
}

// main is the entry point for the Constructo application.