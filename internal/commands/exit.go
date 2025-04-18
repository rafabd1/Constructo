package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrExitRequested é um erro especial para sinalizar que o comando /exit foi executado.
var ErrExitRequested = errors.New("exit requested by user command")

// ExitCmd implements the /exit command.
type ExitCmd struct {
	// Pode precisar de acesso ao Agent ou a um canal de controle para sinalizar o shutdown.
}

func (c *ExitCmd) Name() string        { return "exit" }
func (c *ExitCmd) Description() string { return "Exits the Constructo agent." }
func (c *ExitCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	fmt.Fprintln(output, "Exiting Constructo agent...")
	// Retorna um erro especial que o Agent pode interceptar para iniciar o shutdown.
	return ErrExitRequested 
} 