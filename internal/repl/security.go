package repl

import (
	"fmt"
	"strings"

	"go-code-agent/internal/security"
)

func (r *Loop) handleSecurityCommands(raw string, parts []string) {
	switch parts[0] {
	case "/permissions":
		if len(parts) > 1 && parts[1] == "reload" && r.built.Security.ReloadPermissions != nil {
			if err := r.built.Security.ReloadPermissions(); err != nil {
				fmt.Println(err)
			} else {
				fmt.Println("Permissions reloaded.")
			}
		} else {
			fmt.Println(r.built.Security.Permissions.Describe())
		}
	case "/security":
		fmt.Println(handleSecurityCommand(raw, parts, r.built.Security.Permissions))
	case "/decisions":
		if r.built.Security.DecisionLog != nil {
			fmt.Println(r.built.Security.DecisionLog.Render())
		} else {
			fmt.Println("Decision log not available.")
		}
	}
}

func handleSecurityCommand(raw string, parts []string, perms *security.Permissions) string {
	if len(parts) == 1 {
		return securityDesc()
	}
	if parts[1] != "test-bash" {
		return fmt.Sprintf("Unknown security command: %s\nUsage: /security test-bash <command>", parts[1])
	}

	rest := strings.TrimSpace(strings.TrimPrefix(raw, "/security"))
	command := strings.TrimSpace(strings.TrimPrefix(rest, "test-bash"))
	if command == "" {
		return "Usage: /security test-bash <command>"
	}

	classification := security.ClassifyCommand(command)
	allowed, needConfirm, policyReason := security.NewDefaultBashPolicy().Validate(command, perms)
	decision := "allow"
	if !allowed {
		decision = "deny"
	} else if needConfirm {
		decision = "confirm"
	}
	if policyReason == "" {
		policyReason = classification.Reason
	}

	return fmt.Sprintf(
		"Command: %s\nRisk: %s\nDecision: %s\nReason: %s",
		command,
		classification.Verdict,
		decision,
		policyReason,
	)
}

func securityDesc() string {
	return `Security Status:
  Bash: whitelist (85 commands) + deny/confirm regexps
  Permissions: rules loaded from permissions.json
  Secrets: output sanitizer active
  Diff: preview available for file writes
  Dry-run: /security test-bash <command>`
}
