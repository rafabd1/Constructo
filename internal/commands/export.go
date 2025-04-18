package commands

import (
	"context"
	"fmt"
	"io"
)

// ExportCmd implements the /export command.
type ExportCmd struct {
	// Needs access to session history/state
}

func (c *ExportCmd) Name() string        { return "export" }
func (c *ExportCmd) Description() string { return "Exports the current session history to a file." }
func (c *ExportCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	// Placeholder implementation
	fmt.Fprintln(output, "Export command not yet implemented.")
	return nil
} 