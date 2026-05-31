package llm

import (
	"encoding/json"
	"fmt"
)

// ToolResult is the structured return type for all tool handlers.
type ToolResult struct {
	Output string
	OK     bool
}

func MkOk(output string) ToolResult { return ToolResult{Output: output, OK: true} }
func MkErr(msg string) ToolResult   { return ToolResult{Output: "[ERROR] " + msg, OK: false} }

// ParseArgs unmarshals JSON tool arguments into the target struct.
func ParseArgs(raw json.RawMessage, target any) string {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	return ""
}
