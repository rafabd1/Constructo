package types

// ParsedLLMResponse defines the expected structure of the JSON response from the LLM.
// This structure is flattened for simplicity.
type ParsedLLMResponse struct {
	Type    string `json:"type"`               // "response", "command", "internal_command", "signal"
	Message string `json:"message"`            // Mandatory for response, optional otherwise
	Command string `json:"command,omitempty"`  // Mandatory if type is command/internal_command
	Signal  string `json:"signal,omitempty"`   // Mandatory if type is signal (e.g., "SIGINT")
	TaskID  string `json:"task_id,omitempty"` // Mandatory if type is signal
} 