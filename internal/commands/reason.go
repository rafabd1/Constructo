package commands

import "context"

// ReasonCmd implements the /reason command.
type ReasonCmd struct {
	// Needs access to the reasoning engine (e.g., internal/reasoning.Engine)
}

func (c *ReasonCmd) Name() string        { return "reason" }
func (c *ReasonCmd) Description() string { return "Explicitly triggers the reasoning engine for a given query." }
func (c *ReasonCmd) Execute(ctx context.Context, args []string) error {
	// Implementation to take the query from args and call the reasoning engine
	return nil // Placeholder
} 