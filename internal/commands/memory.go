package commands

import (
	"context"
	"fmt"
	"io"
)

// MemoryCmd implements the /memory command.
type MemoryCmd struct {
	// Needs access to the memory system
}

func (c *MemoryCmd) Name() string        { return "memory" }
func (c *MemoryCmd) Description() string { return "Manages agent memory (save, list, load). Usage: /memory <save|list|load> [name]" }
func (c *MemoryCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	// Placeholder implementation
	fmt.Fprintln(output, "Memory command not yet implemented.")
	return nil 
} 