package commands

import (
	"context"
	"fmt"
	"io"
)

// ResetCmd implements the /reset command.
type ResetCmd struct {
	// Needs access to the agent's context/state management
}

func (c *ResetCmd) Name() string        { return "reset" }
func (c *ResetCmd) Description() string { return "Resets the current conversation context." }
func (c *ResetCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	// Placeholder implementation
	// TODO: Actually clear the LLM chat history (might need access to the agent or session)
	fmt.Fprintln(output, "Conversation context reset. (History clearing not yet implemented)")
	return nil
} 