package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	// Dependência para acessar o ExecutionManager (indireto)
	"github.com/rafabd1/Constructo/internal/task" // Acesso direto às definições
)

// TaskCmd implementa comandos relacionados a tarefas (e.g., /task status).
type TaskCmd struct {
	// Precisa de uma forma de acessar o ExecutionManager.
	// Uma opção é injetar uma referência ao Agent ou diretamente ao ExecutionManager.
	// Vamos usar uma interface para desacoplar.
	ExecManagerProvider func() task.ExecutionManager
}

func (c *TaskCmd) Name() string {
	return "task"
}

func (c *TaskCmd) Description() string {
	return "Manages background tasks. Usage: /task status <task_id>"
}

// Execute handles subcommands like 'status'.
func (c *TaskCmd) Execute(ctx context.Context, args []string, output io.Writer) error {
	if len(args) < 2 {
		fmt.Fprintln(output, c.Description()) // Usa output
		return nil
	}

	subcommand := strings.ToLower(args[0])
	taskID := args[1]

	execManager := c.ExecManagerProvider()
	if execManager == nil {
		// Escreve o erro no output também para o LLM ver
		fmt.Fprintln(output, "Error: execution manager is not available")
		return fmt.Errorf("execution manager is not available")
	}

	switch subcommand {
	case "status":
		taskInfo, err := execManager.GetTaskStatus(taskID)
		if err != nil {
			fmt.Fprintf(output, "Error getting status for task %s: %v\n", taskID, err) // Usa output
			return nil 
		}

		// Imprime as informações formatadas no output
		fmt.Fprintf(output, "--- Task Status [%s] ---\n", taskInfo.ID)
		fmt.Fprintf(output, "Command: %s\n", taskInfo.CommandString)
		fmt.Fprintf(output, "Status: %s\n", taskInfo.Status)
		fmt.Fprintf(output, "Interactive: %t\n", taskInfo.IsInteractive)
		fmt.Fprintf(output, "Start Time: %s\n", taskInfo.StartTime.Format(time.RFC3339))
		if !taskInfo.EndTime.IsZero() {
			fmt.Fprintf(output, "End Time: %s\n", taskInfo.EndTime.Format(time.RFC3339))
			fmt.Fprintf(output, "Duration: %s\n", taskInfo.EndTime.Sub(taskInfo.StartTime))
		}
		if taskInfo.Status == task.StatusSuccess || taskInfo.Status == task.StatusFailed || taskInfo.Status == task.StatusCancelled {
			fmt.Fprintf(output, "Exit Code: %d\n", taskInfo.ExitCode)
		}
		if taskInfo.Error != nil {
			fmt.Fprintf(output, "Execution Error: %v\n", taskInfo.Error)
		}
		cmdOutput := taskInfo.OutputBuffer.String()
		if cmdOutput != "" {
			fmt.Fprintf(output, "--- Output (last ~20 lines) ---\n")
			lines := strings.Split(strings.TrimSpace(cmdOutput), "\n")
			start := len(lines) - 20
			if start < 0 {
				start = 0
			}
			fmt.Fprintln(output, strings.Join(lines[start:], "\n")) // Usa output
			fmt.Fprintf(output, "--- End Output ---\n")
		} else {
			fmt.Fprintln(output, "Output: (empty)") // Usa output
		}
		fmt.Fprintf(output, "--- End Task Status [%s] ---\n", taskInfo.ID)

	default:
		fmt.Fprintf(output, "Unknown task subcommand: %s\n", subcommand) // Usa output
		fmt.Fprintln(output, c.Description()) // Usa output
	}

	return nil
} 