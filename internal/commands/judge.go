package commands

import (
	"context"
	"fmt"
	"io"
)

// JudgeCmd implements the /judge command.
type JudgeCmd struct {
	// Needs access to the judge system
}

func (c *JudgeCmd) Name() string        { return "judge" }
func (c *JudgeCmd) Description() string { return "Triggers the AI Judge to evaluate the current context/plan." }
func (c *JudgeCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	// Placeholder implementation
	fmt.Fprintln(output, "Judge command not yet implemented.")
	return nil
} 