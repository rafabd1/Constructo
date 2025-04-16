package commands

import (
	"context"
	"os"
)

// ExitCmd implements the /sair command.
type ExitCmd struct{}

func (c *ExitCmd) Name() string        { return "sair" }
func (c *ExitCmd) Description() string { return "Exits the Constructo application." }
func (c *ExitCmd) Execute(ctx context.Context, args []string) error {
	// Implementation might involve cleanup before exiting
	os.Exit(0)
	return nil // Technically unreachable
} 