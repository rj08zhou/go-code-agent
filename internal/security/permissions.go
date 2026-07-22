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
	Level   string `json:"level,omitempty"`   // allow, confirm, block (refactor)
	Action  string `json:"action,omitempty"`  // allow, deny, ask (master)
	Pattern string `json:"pattern,omitempty"` // glob pattern for sub-tools (mcp__*)
}

type Permissions struct {
	rules []PermissionRule
}

func NewPermissions() *Permissions { return &Permissions{} }

// masterPermissionsFile is the master-branch schema: { "rules": [...] }.
type masterPermissionsFile struct {
	Rules []PermissionRule `json:"rules"`
}

func (p *Permissions) Load(dataDir string) error {
	path := filepath.Join(dataDir, "permissions.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rules, err := parsePermissionsJSON(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	p.rules = normalizePermissionRules(rules)
	return nil
}

func parsePermissionsJSON(data []byte) ([]PermissionRule, error) {
	// Prefer master wrapped form { "rules": [...] }.
	var wrapped masterPermissionsFile
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Rules != nil {
		return wrapped.Rules, nil
	}
	// Refactor bare array form.
	var rules []PermissionRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// normalizePermissionRules maps master action names onto refactor levels
// and drops rules with unrecognized actions.
func normalizePermissionRules(in []PermissionRule) []PermissionRule {
	out := make([]PermissionRule, 0, len(in))
	for _, r := range in {
		level := strings.ToLower(strings.TrimSpace(r.Level))
		if level == "" {
			level = strings.ToLower(strings.TrimSpace(r.Action))
		}
		switch level {
		case "allow":
			r.Level = "allow"
		case "confirm", "ask":
			r.Level = "confirm"
		case "block", "deny":
			r.Level = "block"
		default:
			continue // unknown → skip
		}
		r.Action = ""
		if strings.TrimSpace(r.Tool) == "" && strings.TrimSpace(r.Pattern) == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Match checks toolName+args against permission rules. Returns level ("allow",
// "confirm", "block") or "" if no rule matched.
func (p *Permissions) Match(toolName, args string) string {
	for _, r := range p.rules {
		if !permissionToolMatches(r, toolName) {
			continue
		}
		if r.Pattern != "" && !permissionArgsMatch(r, toolName, args) {
			continue
		}
		return r.Level
	}
	return ""
}

func permissionToolMatches(r PermissionRule, toolName string) bool {
	if r.Tool == "*" {
		return true
	}
	if r.Tool != "" {
		if strings.ContainsAny(r.Tool, "*?") {
			return wildcardMatch(r.Tool, toolName)
		}
		return strings.EqualFold(r.Tool, toolName)
	}
	// Refactor-style: pattern alone may be a tool-name glob (mcp__*).
	if r.Pattern != "" && !strings.Contains(r.Pattern, " ") {
		return wildcardMatch(r.Pattern, toolName)
	}
	// Combined "bash rm*" pattern encodes the tool name in the pattern.
	if r.Pattern != "" && strings.Contains(r.Pattern, " ") {
		return strings.HasPrefix(r.Pattern, toolName+" ")
	}
	return false
}

func permissionArgsMatch(r PermissionRule, toolName, args string) bool {
	pat := r.Pattern
	if pat == "" {
		return true
	}
	// "bash rm*" → match args against "rm*"
	if strings.Contains(pat, " ") {
		parts := strings.SplitN(pat, " ", 2)
		if len(parts) == 2 && parts[0] == toolName {
			pat = parts[1]
		}
	} else if r.Tool == "" {
		// Pattern was used as tool glob only; no arg constraint.
		return true
	}
	// Master-style: tool matched by Tool field, Pattern applies to args.
	if pat == "*" || pat == "" {
		return true
	}
	return wildcardMatch(pat, args) || strings.Contains(strings.ToLower(args), strings.ToLower(strings.TrimSuffix(pat, "*")))
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
