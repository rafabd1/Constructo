package commands

import "context"

// HelpCmd implements the /help command.
type HelpCmd struct {
	Registry *Registry // Needs access to the registry to list commands
}

func (c *HelpCmd) Name() string        { return "help" }
func (c *HelpCmd) Description() string { return "Shows available commands and descriptions." }
func (c *HelpCmd) Execute(ctx context.Context, args []string) error {
	// Implementation to fetch all commands from Registry and print help text
	return nil // Placeholder
} 