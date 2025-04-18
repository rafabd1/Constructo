package commands

import (
	"context"
	"fmt"
	"io"
)

// ReasonCmd implements the /reason command.
type ReasonCmd struct {
	// Needs access to the reasoning engine
}

func (c *ReasonCmd) Name() string        { return "reason" }
func (c *ReasonCmd) Description() string { return "Triggers deep reasoning for a complex task." }
func (c *ReasonCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	// Placeholder implementation
	fmt.Fprintln(output, "Reason command not yet implemented.")
	return nil
} 