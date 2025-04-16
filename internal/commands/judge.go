package commands

import "context"

// JudgeCmd implements the /judge command.
type JudgeCmd struct {
	// Needs access to the AI judge system (e.g., internal/judge.Judge)
}

func (c *JudgeCmd) Name() string        { return "judge" }
func (c *JudgeCmd) Description() string { return "Manually triggers the AI Judge to evaluate the current context." }
func (c *JudgeCmd) Execute(ctx context.Context, args []string) error {
	// Implementation to trigger the AI judge evaluation
	return nil // Placeholder
} 