package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

func main() {
	// Try starting cmd.exe in a PTY
	cmd := exec.Command("cmd.exe") // Using cmd.exe for Windows test
	fmt.Println("Attempting to start pty...")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		fmt.Printf("Error starting pty: %v\n", err)
		os.Exit(1)
	}
	defer ptmx.Close() // Best effort close

	fmt.Println("PTY started successfully!")

	// Simple: just wait for the command to exit
	err = cmd.Wait()
	fmt.Printf("Command finished with error: %v\n", err)
	// Exit with 0 if command exited cleanly (err == nil or ExitError with code 0)
	if err == nil {
		os.Exit(0)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() == 0 {
			os.Exit(0)
		}
	}
	os.Exit(1) // Exit non-zero for other errors
} 