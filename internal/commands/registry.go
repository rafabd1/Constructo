package commands

import (
	"fmt"
	"sync"
)

// Registry holds the registered slash commands.
type Registry struct {
	mu       sync.RWMutex
	commands map[string]Command
}

// NewRegistry creates a new command registry.
func NewRegistry() *Registry {
	return &Registry{
		commands: make(map[string]Command),
	}
}

// Register adds a command to the registry.
func (r *Registry) Register(cmd Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := cmd.Name()
	if _, exists := r.commands[name]; exists {
		return fmt.Errorf("command '%s' already registered", name)
	}
	r.commands[name] = cmd
	return nil
}

// Get returns a command by its name.
func (r *Registry) Get(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cmd, exists := r.commands[name]
	return cmd, exists
}

// GetAll returns all registered commands.
func (r *Registry) GetAll() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cmds := make([]Command, 0, len(r.commands))
	for _, cmd := range r.commands {
		cmds = append(cmds, cmd)
	}
	return cmds
} 