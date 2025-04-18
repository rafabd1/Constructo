package commands

import (
	"context"
	"fmt"
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

func (c *TaskCmd) Execute(ctx context.Context, args []string) error {
	if len(args) < 2 {
		fmt.Println(c.Description())
		return nil
	}

	subcommand := strings.ToLower(args[0])
	taskID := args[1]

	execManager := c.ExecManagerProvider()
	if execManager == nil {
		return fmt.Errorf("execution manager is not available")
	}

	switch subcommand {
	case "status":
		taskInfo, err := execManager.GetTaskStatus(taskID)
		if err != nil {
			fmt.Printf("Error getting status for task %s: %v\n", taskID, err)
			return nil // O erro é mostrado ao usuário, comando tratado
		}

		// Imprime as informações formatadas
		fmt.Printf("--- Task Status [%s] ---\n", taskInfo.ID)
		fmt.Printf("Command: %s\n", taskInfo.CommandString)
		fmt.Printf("Status: %s\n", taskInfo.Status)
		fmt.Printf("Interactive: %t\n", taskInfo.IsInteractive)
		fmt.Printf("Start Time: %s\n", taskInfo.StartTime.Format(time.RFC3339))
		if !taskInfo.EndTime.IsZero() {
			fmt.Printf("End Time: %s\n", taskInfo.EndTime.Format(time.RFC3339))
			fmt.Printf("Duration: %s\n", taskInfo.EndTime.Sub(taskInfo.StartTime))
		}
		if taskInfo.Status == task.StatusSuccess || taskInfo.Status == task.StatusFailed || taskInfo.Status == task.StatusCancelled {
			fmt.Printf("Exit Code: %d\n", taskInfo.ExitCode)
		}
		if taskInfo.Error != nil {
			fmt.Printf("Execution Error: %v\n", taskInfo.Error)
		}
		output := taskInfo.OutputBuffer.String()
		if output != "" {
			fmt.Printf("--- Output (last ~20 lines) ---\n")
			// Mostra apenas as últimas linhas para não poluir muito
			lines := strings.Split(strings.TrimSpace(output), "\n")
			start := len(lines) - 20
			if start < 0 {
				start = 0
			}
			fmt.Println(strings.Join(lines[start:], "\n"))
			fmt.Printf("--- End Output ---\n")
		} else {
			fmt.Println("Output: (empty)")
		}
		fmt.Printf("--- End Task Status [%s] ---\n", taskInfo.ID)

	default:
		fmt.Printf("Unknown task subcommand: %s\n", subcommand)
		fmt.Println(c.Description())
	}

	return nil
} 