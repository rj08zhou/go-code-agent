package security

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type PermissionRule struct {
	Tool    string `json:"tool"`
	Level   string `json:"level"`             // allow, confirm, block
	Pattern string `json:"pattern,omitempty"` // glob pattern for sub-tools (mcp__*)
}

type Permissions struct {
	rules []PermissionRule
}

func NewPermissions() *Permissions { return &Permissions{} }

func (p *Permissions) Load(dataDir string) error {
	path := filepath.Join(dataDir, "permissions.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var rules []PermissionRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	p.rules = rules
	return nil
}

// Match checks toolName+args against permission rules. Returns level and whether matched.
func (p *Permissions) Match(toolName, args string) string {
	for _, r := range p.rules {
		if r.Pattern != "" {
			if !wildcardMatch(r.Pattern, toolName) {
				continue
			}
		} else if !strings.EqualFold(r.Tool, toolName) {
			continue
		}
		// Arg-level sub-matching: if rule has pattern like "bash rm*", check args
		if r.Pattern != "" && strings.Contains(r.Pattern, " ") {
			parts := strings.SplitN(r.Pattern, " ", 2)
			if len(parts) == 2 && parts[0] == toolName {
				if !strings.Contains(strings.ToLower(args), strings.ToLower(parts[1])) {
					continue
				}
			}
		}
		return r.Level
	}
	return ""
}

func (p *Permissions) Count() int { return len(p.rules) }

func (p *Permissions) Describe() string {
	if len(p.rules) == 0 {
		return "No custom permission rules."
	}
	var sb strings.Builder
	for _, r := range p.rules {
		fmt.Fprintf(&sb, "  %-20s -> %s", r.Tool, r.Level)
		if r.Pattern != "" {
			fmt.Fprintf(&sb, " (pattern: %s)", r.Pattern)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// wildcardMatch supports * that crosses / boundaries.
func wildcardMatch(pattern, name string) bool {
	px, nx := 0, 0
	nextPx, nextNx := 0, 0
	for nx < len(name) || px < len(pattern) {
		if px < len(pattern) {
			c := pattern[px]
			switch c {
			case '*':
				nextPx = px
				nextNx = nx + 1
				px++
				continue
			default:
				if nx < len(name) && (name[nx] == c || (c == '?')) {
					px++
					nx++
					continue
				}
			}
		}
		if nextNx > 0 && nextNx <= len(name) {
			px = nextPx + 1
			nx = nextNx
			nextNx++
			continue
		}
		return false
	}
	return true
}
