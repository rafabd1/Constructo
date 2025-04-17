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
	// TODO: Import other command packages if they are separate
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. Create Command Registry
	registry := commands.NewRegistry()

	// 2. Register Commands (Example - register all from internal/commands)
	// In a real app, you might have a function to discover and register commands.
	helpCmd := &commands.HelpCmd{Registry: registry} // HelpCmd needs the registry
	if err := registry.Register(helpCmd); err != nil {
		log.Fatalf("Failed to register help command: %v", err)
	}
	// Register other commands (assuming they don't need complex dependencies for now)
	cmdsToRegister := []commands.Command{
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
			// Use Fatalf for critical startup errors
			log.Fatalf("Failed to register command %s: %v", cmd.Name(), err)
		}
	}
	// Make HelpCmd aware of all commands *after* they are registered
	// helpCmd.Initialize() // Or pass registry later if needed

	// 3. Create Agent (passing registry)
	constructoAgent, err := agent.NewAgent(ctx, registry)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
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