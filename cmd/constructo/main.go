package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	syscall "syscall"

	"github.com/rafabd1/Constructo/internal/agent"
	"github.com/rafabd1/Constructo/internal/commands"
	"github.com/rafabd1/Constructo/internal/config"
	"github.com/rafabd1/Constructo/internal/task"
	// TODO: Import other command packages if they are separate
)

// Variável global temporária para armazenar o manager (melhorar com injeção de dependência depois)
// var globalExecManager task.ExecutionManager

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 0. Load Configuration
	cfg, err := config.Load() 
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	// Validate mandatory config (e.g., API Key if not using ADC)
	if cfg.LLM.APIKey == "" {
		// Depending on auth strategy, this might be a fatal error
		// Or just a warning if ADC is expected to work.
		log.Println("Warning: llm.api_key is not set in config. Attempting ADC.")
		// Optionally, check if project_id/location are set for ADC
		// if cfg.LLM.ProjectID == "" || cfg.LLM.Location == "" {
		// 	log.Fatalf("Fatal: llm.api_key is missing and llm.project_id/llm.location are not set for ADC.")
		// }
	}

	// 1. Create Command Registry
	registry := commands.NewRegistry()

	// 2. Create Agent (PRECISA VIR ANTES para obter o ExecutionManager)
	//    Isso expõe um pequeno problema de ordem de inicialização que pode ser 
	//    resolvido com injeção de dependência mais robusta no futuro.
	constructoAgent, err := agent.NewAgent(ctx, cfg, registry)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}
	// Obter o ExecutionManager do Agent (Exige um método getter no Agent)
	execManager := constructoAgent.GetExecutionManager() // <<< PRECISA ADICIONAR ESTE MÉTODO AO AGENT
	if execManager == nil {
		log.Fatalf("Failed to get execution manager from agent")
	}

	// 3. Register Commands (Agora podemos injetar dependências)
	helpCmd := &commands.HelpCmd{Registry: registry}
	if err := registry.Register(helpCmd); err != nil {
		log.Fatalf("Failed to register help command: %v", err)
	}

	// Função provider para o TaskCmd
	getManager := func() task.ExecutionManager {
		return execManager
	}

	cmdsToRegister := []commands.Command{
		&commands.TaskCmd{ExecManagerProvider: getManager}, // <<< Comando Task registrado
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

	// 4. Run the Agent
	fmt.Println("Starting Constructo Agent...")
	if err := constructoAgent.Run(ctx); err != nil {
		// Log errors that occur during run, but don't necessarily Fatal
		log.Printf("Agent run finished with error: %v", err)
		// Exit with non-zero status on error
		os.Exit(1)
	}

	fmt.Println("Constructo Agent stopped gracefully.")
	os.Exit(0)
}

// main is the entry point for the Constructo application.