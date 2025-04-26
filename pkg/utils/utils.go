package utils

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// Package utils contains general utility functions.

// Add utility functions below

// extractJsonBlock attempts to extract a valid JSON block from a raw string.
// It tries different methods to find the JSON, from simple trimming to finding braces.
func ExtractJsonBlock(rawResponse string) (string, error) {
	// Attempt 1: Trim whitespace and check if it's valid JSON directly
	trimmed := strings.TrimSpace(rawResponse)
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		if json.Valid([]byte(trimmed)) {
			return trimmed, nil
		}
	}

	// Attempt 2: Find the first '{' and the last '}'
	firstBrace := strings.Index(rawResponse, "{")
	lastBrace := strings.LastIndex(rawResponse, "}")
	if firstBrace != -1 && lastBrace != -1 && lastBrace > firstBrace {
		extracted := rawResponse[firstBrace : lastBrace+1]
		if json.Valid([]byte(extracted)) {
			log.Println("[Debug] Extracted JSON block using first/last brace.")
			return extracted, nil
		}
	}

	// Could add more robust regex or parsing logic here if needed

	return "", fmt.Errorf("could not extract valid JSON block from LLM response")
}

// mapSignal converts a signal name string (case-insensitive) to an os.Signal.
// Returns nil if the signal name is not recognized.
func MapSignal(signalName string) os.Signal {
	switch strings.ToUpper(signalName) {
	case "SIGINT":
		return os.Interrupt
	case "SIGTERM":
		// Note: os.Kill is syscall.SIGKILL on Unix-like systems.
		// It's generally better to send SIGTERM first if possible,
		// but for simplicity here, we map SIGTERM directly to Kill.
		return os.Kill
	case "SIGKILL":
		return os.Kill
	default:
		return nil
	}
}