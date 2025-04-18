package commands

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// HelpCmd implements the /help command.
type HelpCmd struct {
	Registry *Registry // Needs access to the registry to list commands
}

func (c *HelpCmd) Name() string        { return "help" }
func (c *HelpCmd) Description() string { return "Shows available commands and descriptions." }

// Execute lists all registered commands and their descriptions.
func (c *HelpCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	if c.Registry == nil {
		return fmt.Errorf("command registry is not available to HelpCmd")
	}

	cmds := c.Registry.GetAll() // Assume GetAll exists and returns []Command

	// Sort commands by name for consistent output
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name() < cmds[j].Name()
	})

	fmt.Fprintln(output, "Available Commands:")
	for _, cmd := range cmds {
		fmt.Fprintf(output, "  /%s: %s\n", cmd.Name(), cmd.Description())
	}
	return nil
} 