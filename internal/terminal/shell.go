package terminal

import (
	"os"
	"os/exec"
	"runtime"
)

// defaultShell returns the default shell command for the current OS.
func defaultShell() string {
	shell := os.Getenv("SHELL")
	if shell != "" {
		return shell
	}

	if runtime.GOOS == "windows" {
		// COMSPEC usually points to cmd.exe, but PowerShell is often preferred.
		// We might need a more robust detection or rely on configuration.
		// For now, let's prefer PowerShell if available, otherwise fallback to COMSPEC or cmd.
		if psPath, err := exec.LookPath("powershell.exe"); err == nil {
			return psPath
		}
		comspec := os.Getenv("COMSPEC")
		if comspec != "" {
			return comspec
		}
		return "cmd.exe"
	}

	// Default for non-Windows (Linux, macOS, etc.)
	return "/bin/sh" // Basic fallback
}

// shell provides abstractions for different command shells (bash, powershell, etc.).