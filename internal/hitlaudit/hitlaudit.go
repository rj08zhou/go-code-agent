package hitlaudit

import (
	"encoding/json"
	"fmt"
	"go-code-agent/utils"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HITL Audit Log.
//
// Every HITL decision is appended to {dataDir}/memory/hitl_audit.jsonl.
// Exempt from the 7-day TTL sweep (security audit trail).
// Best-effort writes: never blocks the main flow on failure.

// HITLAuditEntry is one row in hitl_audit.jsonl.
//
// Field names are short and stable - downstream tooling (grep, jq, log
// shippers) will key off them.
type HITLAuditEntry struct {
	Timestamp string `json:"ts"`                 // RFC3339 UTC
	SessionID string `json:"session_id"`         // active session at decision time, "" if none
	Tool      string `json:"tool"`               // tool name being gated
	Arguments string `json:"args"`               // raw JSON args (truncated to keep file scannable)
	RiskLevel string `json:"risk"`               // "low" | "medium" | "high"
	Reason    string `json:"reason"`             // why HITL was triggered
	Mode      string `json:"mode"`               // interactive | auto-approve | auto-reject | notify-only
	Decision  string `json:"decision"`           // approve | reject | modify
	Feedback  string `json:"feedback,omitempty"` // operator feedback (only for modify)
}

// HITLAuditor owns the audit JSONL file.
type HITLAuditor struct {
	mu       sync.Mutex
	path     string // absolute file path; empty until Init succeeds
	disabled bool   // true if Init failed; subsequent Record calls become no-ops
}

// hitlAuditor is the package-level singleton. main() calls InitHITLAudit
// once after the workspace is known.
var hitlAuditor = &HITLAuditor{}

// InitHITLAudit configures the audit log location. Safe to call more than
// once (e.g. on workdir change), though in practice we call it once at
// startup. dataDir is the resolved per-project state directory; the log
// lives under {dataDir}/memory/hitl_audit.jsonl.
func InitHITLAudit(dataDir string) {
	if dataDir == "" {
		hitlAuditor.disabled = true
		return
	}
	dir := filepath.Join(dataDir, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// We can't create the audit dir. Don't kill the agent over it,
		// just stop trying to write so we don't spam stderr per call.
		fmt.Fprintf(os.Stderr, "[hitl-audit] cannot create %s: %v (audit disabled)\n", dir, err)
		hitlAuditor.disabled = true
		return
	}
	hitlAuditor.mu.Lock()
	hitlAuditor.path = filepath.Join(dir, "hitl_audit.jsonl")
	hitlAuditor.disabled = false
	hitlAuditor.mu.Unlock()
}

// Record appends one decision to the audit log (best-effort).
func (a *HITLAuditor) Record(req HITLRequest, mode HITLMode, resp HITLResponse) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.disabled || a.path == "" {
		return
	}

	entry := HITLAuditEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SessionID: req.SessionID,
		Tool:      req.ToolName,
		Arguments: utils.Truncate(req.Arguments, hitlAuditArgsMax),
		RiskLevel: req.RiskLevel,
		Reason:    req.Reason,
		Mode:      mode.String(),
		Decision:  decisionString(resp.Decision),
		Feedback:  utils.Truncate(resp.Feedback, hitlAuditFeedbackMax),
	}

	data, err := json.Marshal(&entry)
	if err != nil {
		// Marshal failure on a struct of strings is essentially impossible
		// short of an OOM; surface and skip.
		fmt.Fprintf(os.Stderr, "[hitl-audit] marshal error: %v\n", err)
		return
	}
	data = append(data, '\n')

	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hitl-audit] open %s: %v\n", a.path, err)
		return
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "[hitl-audit] write %s: %v\n", a.path, err)
		return
	}
	// Audit lines are individually small and infrequent; pay the fsync
	// cost so a crash 50ms after a destructive approval still leaves
	// evidence on disk. Same reasoning as MemoryStore.WriteMemory.
	_ = f.Sync()
}

// decisionString renders HITLDecision for the audit log. Keeps the wire
// format independent of iota ordering changes in human_approval.go.
func decisionString(d HITLDecision) string {
	switch d {
	case HITLApprove:
		return "approve"
	case HITLReject:
		return "reject"
	case HITLModify:
		return "modify"
	}
	return "unknown"
}

// Truncation budgets: keep the audit file grep-friendly. Full args/feedback
// can still be reconstructed from the session log if forensic depth is
// needed - audit only needs enough context to spot suspicious activity.
const (
	hitlAuditArgsMax     = 2048
	hitlAuditFeedbackMax = 512
)
