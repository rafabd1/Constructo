package commands

import (
	"context"
	"io"
)

// Command defines the interface for executable slash commands.
type Command interface {
	Name() string        // Returns the command name (e.g., "help")
	Description() string // Returns a brief description
	// Executes the command, writing output to the provided writer.
	Execute(ctx context.Context, args []string, output io.Writer) error
}

// ContextKey is a type for context keys within the commands package.
type ContextKey string

// // Example context keys (adjust as needed)
// const AgentContextKey ContextKey = "agentContext"
// const MemoryContextKey ContextKey = "memoryContext"
// const LLMClientContextKey ContextKey = "llmClientContext" 