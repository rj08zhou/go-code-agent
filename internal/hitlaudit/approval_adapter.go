package hitlaudit

import (
	"encoding/json"
	"fmt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/tool"
	"os"
	"path/filepath"
	"sync"
)

// HITLApprovalAdapter adapts HITLManager to the tool.ApprovalChecker interface,
// enabling the executor to call HITL for human-in-the-loop approval.
type HITLApprovalAdapter struct {
	mgr                 *HITLManager
	approval            *security.ApprovalState
	workdir             string
	mu                  sync.Mutex
	lastAcceptedContent string
}

func NewHITLApprovalAdapter(mgr *HITLManager) *HITLApprovalAdapter {
	return &HITLApprovalAdapter{mgr: mgr}
}

func (a *HITLApprovalAdapter) SetWorkdir(wd string) { a.workdir = wd }

// SetApproval binds the session ApprovalState (diff-preview skip /approve presets).
func (a *HITLApprovalAdapter) SetApproval(s *security.ApprovalState) { a.approval = s }

func (a *HITLApprovalAdapter) shouldPreviewDiff() bool {
	if a.approval == nil {
		return true
	}
	return a.approval.ShouldPreviewDiff()
}

// AcceptedContent returns the partially-accepted content from chunk-by-chunk
// diff review, or empty string if no review happened or content wasn't modified.
func (a *HITLApprovalAdapter) AcceptedContent() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastAcceptedContent
}

func (a *HITLApprovalAdapter) readOldContent(path string) string {
	if path == "" || a.workdir == "" {
		return ""
	}
	content, err := os.ReadFile(filepath.Join(a.workdir, path))
	if err != nil {
		return ""
	}
	return string(content)
}

func extractPathAndContent(args json.RawMessage) (string, string) {
	var m struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &m); err != nil {
		return "", ""
	}
	return m.Path, m.Content
}

func (a *HITLApprovalAdapter) AllowTool(toolName string, args json.RawMessage) (bool, string) {
	decision, reason, feedback := a.DecideTool(toolName, args, "")
	if decision == tool.ApprovalModified {
		return false, feedback
	}
	return decision == tool.ApprovalAllowed, reason
}

func (a *HITLApprovalAdapter) AllowToolWithPreview(toolName string, args json.RawMessage, preview string) (bool, string) {
	decision, reason, feedback := a.DecideTool(toolName, args, preview)
	if decision == tool.ApprovalModified {
		return false, feedback
	}
	return decision == tool.ApprovalAllowed, reason
}

func isFileMutation(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "insert_file", "delete_file":
		return true
	}
	return false
}

func (a *HITLApprovalAdapter) DecideTool(toolName string, args json.RawMessage, preview string) (tool.ApprovalDecision, string, string) {
	needsReview, riskLevel, reason := a.mgr.NeedsReview(toolName, string(args))

	// For file mutations with diff preview, do chunk-by-chunk review
	// only when not in auto-approve mode and diff preview is enabled.
	if preview != "" && a.mgr.IsEnabled() && isFileMutation(toolName) &&
		a.mgr.Mode() != HITLModeAutoApprove && a.shouldPreviewDiff() {
		path, newContent := extractPathAndContent(args)
		oldContent := a.readOldContent(path)
		a.mu.Lock()
		a.lastAcceptedContent = ""
		a.mu.Unlock()

		accepted, ok := security.PreviewAndConfirm(path, oldContent, newContent, preview)
		if !ok {
			return tool.ApprovalRejected, "changes rejected by operator", ""
		}
		a.mu.Lock()
		a.lastAcceptedContent = accepted
		a.mu.Unlock()
		return tool.ApprovalAllowed, "", ""
	}
	if !needsReview {
		return tool.ApprovalAllowed, "", ""
	}
	if preview != "" {
		reason += "\n\nProposed mutation:\n" + preview
	}
	resp := a.mgr.RequestApproval(HITLRequest{ToolName: toolName, Arguments: string(args), RiskLevel: riskLevel, Reason: reason})
	switch resp.Decision {
	case HITLApprove:
		return tool.ApprovalAllowed, "", ""
	case HITLReject:
		return tool.ApprovalRejected, fmt.Sprintf("HITL rejected %s: %s", toolName, reason), ""
	case HITLModify:
		return tool.ApprovalModified, "", resp.Feedback
	default:
		return tool.ApprovalRejected, "unknown HITL decision", ""
	}
}
