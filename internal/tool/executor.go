package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"path/filepath"
	"strings"
	"time"
)

// It owns the ToolCatalog and runs every tool through
// validation, authorization, timeout, and result formatting.
type Executor struct {
	catalog   *ToolCatalog
	approval  ApprovalChecker
	network   NetworkChecker
	sanitizer OutputSanitizer
	decisions DecisionLogger
	timeout   time.Duration
}

// OutputSanitizer redacts secrets from tool outputs.
type OutputSanitizer interface {
	Sanitize(s string) string
}

// DecisionLogger records authorization and approval decisions.
type DecisionLogger interface {
	Record(tool, action, reason string, round int)
}

// PreviewApprovalChecker can display a mutation preview as part of approval.
type PreviewApprovalChecker interface {
	AllowToolWithPreview(toolName string, args json.RawMessage, preview string) (bool, string)
}

// DetailedApprovalChecker preserves HITL's allow/reject/modify decision and
// returns any approved replacement content with the same invocation result.
type DetailedApprovalChecker interface {
	DecideTool(toolName string, args json.RawMessage, preview ApprovalPreview) ApprovalResult
}

type ApprovalPreview struct {
	Text     string
	Mutation *PreviewRequest
}

type ApprovalResult struct {
	Decision           ApprovalDecision
	Reason             string
	Feedback           string
	ReplacementContent *string
}

type ApprovalDecision int

const (
	ApprovalAllowed ApprovalDecision = iota
	ApprovalRejected
	ApprovalModified
)

// WithDecisionLogger attaches a decision audit sink.
func (e *Executor) WithDecisionLogger(l DecisionLogger) *Executor { e.decisions = l; return e }

// WithSanitizer sets the output sanitizer (e.g. secrets redactor).
func (e *Executor) WithSanitizer(s OutputSanitizer) *Executor {
	e.sanitizer = s
	return e
}

func NewExecutor(catalog *ToolCatalog, approval ApprovalChecker, network NetworkChecker) *Executor {
	return &Executor{
		catalog:  catalog,
		approval: approval,
		network:  network,
		timeout:  config.PerToolTimeout,
	}
}

// Definition returns the current immutable definition for a registered tool.
func (e *Executor) Definition(name string) (ToolDefinition, bool) {
	if e == nil || e.catalog == nil {
		return ToolDefinition{}, false
	}
	definition, ok := e.catalog.Load().Definitions[name]
	return definition, ok
}

// Execute runs a single tool call through the full security+execution pipeline.
func (e *Executor) Execute(ctx context.Context, scope *ToolScope, tc llm.ToolCall) Result {
	started := time.Now()
	defer func() { /* duration set below */ }()

	// 1. Validate JSON
	if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
		return InvalidArgs(fmt.Sprintf("tool call '%s' has truncated arguments", tc.Name))
	}

	// 2. Resolve handler from immutable snapshot
	snap := e.catalog.Load()
	def, known := snap.Definitions[tc.Name]
	handler, hasHandler := snap.Handlers[tc.Name]
	if !known || !hasHandler {
		return Unavailable(fmt.Sprintf("unknown tool %q", tc.Name))
	}
	if scope == nil {
		return Denied(fmt.Sprintf("tool %q requires an execution scope", tc.Name))
	}

	// 3. Check capability via scope
	switch {
	case def.HasEffect(EffectExecuteProcess) && !scope.CanExecute:
		return Denied(fmt.Sprintf("tool %q requires execute capability", tc.Name))
	case def.HasEffect(EffectWriteFile) && !scope.CanWrite:
		return Denied(fmt.Sprintf("tool %q requires write capability", tc.Name))
	case def.HasEffect(EffectDeleteFile) && !scope.CanWrite:
		return Denied(fmt.Sprintf("tool %q requires write capability (delete)", tc.Name))
	case def.HasEffect(EffectReadFile) && !scope.CanRead:
		return Denied(fmt.Sprintf("tool %q requires read capability", tc.Name))
	case def.HasEffect(EffectNetworkAccess) && !scope.CanNetwork:
		return Denied(fmt.Sprintf("tool %q requires network capability", tc.Name))
	case def.HasEffect(EffectMemoryMutation) && !scope.CanMemory:
		return Denied(fmt.Sprintf("tool %q requires memory capability", tc.Name))
	case def.HasEffect(EffectTeamMutation) && !scope.CanTeam:
		return Denied(fmt.Sprintf("tool %q requires team capability", tc.Name))
	}

	// 3b. Enforce the invocation's allowed filesystem roots.
	if def.HasEffect(EffectReadFile) || def.HasEffect(EffectWriteFile) || def.HasEffect(EffectDeleteFile) {
		if path := extractPath(tc.Arguments); path != "" && !pathAllowed(scope, path) {
			return Denied(fmt.Sprintf("path %q is outside allowed roots", path))
		}
	}

	// 4. Compute a mutation preview before approval and before the handler runs.
	approvalPreview := ApprovalPreview{}
	if def.Preview != nil && scope.DiffPreview == nil {
		return Denied("mutation preview service is required for this tool")
	}
	if def.Preview != nil && scope.DiffPreview != nil {
		req, err := def.Preview(scope, json.RawMessage(tc.Arguments))
		if err != nil {
			return Denied(fmt.Sprintf("cannot create mutation preview: %v", err))
		}
		approvalPreview.Text, err = scope.DiffPreview.PreviewChange(
			req.Path,
			req.OriginalContent,
			req.Content,
		)
		if err != nil {
			return Denied(fmt.Sprintf("cannot create mutation preview: %v", err))
		}
		approvalPreview.Mutation = &req
	}

	// 5. Approval check — preview-aware HITL runs before mutation.
	var replacementContent *string
	if e.approval != nil {
		if detailed, ok := e.approval.(DetailedApprovalChecker); ok {
			approvalResult := detailed.DecideTool(tc.Name, json.RawMessage(tc.Arguments), approvalPreview)
			switch approvalResult.Decision {
			case ApprovalRejected:
				return Rejected(approvalResult.Reason)
			case ApprovalModified:
				return Modified(approvalResult.Feedback)
			}
			if approvalResult.ReplacementContent != nil {
				if approvalPreview.Mutation == nil || approvalPreview.Mutation.Delete {
					return Denied("approval returned replacement content for a non-write mutation")
				}
				replacementContent = approvalResult.ReplacementContent
			}
		} else if checker, ok := e.approval.(PreviewApprovalChecker); ok {
			if allowed, reason := checker.AllowToolWithPreview(tc.Name, json.RawMessage(tc.Arguments), approvalPreview.Text); !allowed {
				return Rejected(reason)
			}
		} else if allowed, reason := e.approval.AllowTool(tc.Name, json.RawMessage(tc.Arguments)); !allowed {
			return Rejected(reason)
		}
	}

	// 5b. Scope-level approval check.
	if scope.ApprovalPolicy != nil {
		if allowed, reason := scope.ApprovalPolicy.AllowTool(tc.Name, json.RawMessage(tc.Arguments)); !allowed {
			return Rejected(reason)
		}
	}

	// 4c. Scope-level network policy (validates any URL in arguments)
	if def.HasEffect(EffectNetworkAccess) && scope.NetworkPolicy != nil {
		if url := extractURL(tc.Arguments); url != "" {
			if !scope.NetworkPolicy.AllowURL(url) {
				return Denied(fmt.Sprintf("URL %q blocked by network policy", url))
			}
		}
	}
	// 4d. Global network checker
	if def.HasEffect(EffectNetworkAccess) && e.network != nil {
		if url := extractURL(tc.Arguments); url != "" {
			if !e.network.AllowURL(url) {
				return Denied(fmt.Sprintf("URL %q blocked by network policy", url))
			}
		}
	}

	// 5. Set timeout
	toolTimeout := e.timeout
	if def.Timeout > 0 {
		toolTimeout = def.Timeout
	}

	callCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	// 6. Execute with timeout
	resultCh := make(chan Result, 1)
	handlerScope := *scope
	handlerScope.Context = callCtx
	if approvalPreview.Mutation != nil {
		request := *approvalPreview.Mutation
		request.OriginalContent = append([]byte(nil), request.OriginalContent...)
		request.Content = append([]byte(nil), request.Content...)
		if replacementContent != nil {
			request.Content = []byte(*replacementContent)
		}
		handlerScope.approvedMutation = &request
	}
	go func() {
		result := func() (result Result) {
			defer func() {
				if recovered := recover(); recovered != nil {
					result = Failed(fmt.Sprintf("tool panicked: %v", recovered))
				}
			}()
			return handler(&handlerScope, json.RawMessage(tc.Arguments))
		}()
		if e.sanitizer != nil {
			result.Output = e.sanitizer.Sanitize(result.Output)
		}
		resultCh <- result
	}()

	select {
	case result := <-resultCh:
		result.Duration = time.Since(started)
		if e.decisions != nil {
			e.decisions.Record(tc.Name, string(result.Status), result.Output, 0)
		}
		return result
	case <-callCtx.Done():
		if ctx.Err() != nil {
			return Cancelled(ctx.Err())
		}
		return Timeout(tc.Name, toolTimeout)
	}
}

// ToolDefs returns the LLM-facing tool schemas from the catalog.
func (e *Executor) ToolDefs() []llm.ToolDef {
	return e.catalog.LLMToolDefs()
}

// ExecuteAll runs multiple tool calls from a single assistant turn.
func (e *Executor) ExecuteAll(ctx context.Context, scope *ToolScope, calls []llm.ToolCall) []Result {
	results := make([]Result, len(calls))
	for i, tc := range calls {
		results[i] = e.Execute(ctx, scope, tc)
	}
	return results
}

func extractPath(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	for _, key := range []string{"path", "file", "filename"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func pathAllowed(scope *ToolScope, raw string) bool {
	if scope == nil {
		return false
	}
	// Handler-level SecurePath remains the default sandbox. Executor-level
	// root enforcement is activated when the caller explicitly supplies roots.
	if len(scope.AllowedRoots) == 0 {
		return true
	}
	if scope.Workdir == "" {
		return false
	}
	// Match SecurePath: absolute inputs must not be Join'd onto workdir
	// (Go 1.25+ Join keeps both sides: Join("/wd", "/Users/x") → "/wd/Users/x").
	var candidate string
	if filepath.IsAbs(raw) {
		candidate = filepath.Clean(raw)
	} else {
		candidate = filepath.Join(scope.Workdir, raw)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		// For a new file, resolve the parent path instead.
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(abs))
		if parentErr != nil {
			return false
		}
		abs = filepath.Join(parent, filepath.Base(abs))
	}
	roots := scope.AllowedRoots
	if len(roots) == 0 {
		roots = []string{scope.Workdir}
	}
	for _, root := range roots {
		r, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		r = filepath.Clean(r)
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// extractURL tries to pull a "url" or "URL" field from raw JSON tool arguments.
func extractURL(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	if u, ok := m["url"].(string); ok && u != "" {
		return u
	}
	if u, ok := m["URL"].(string); ok && u != "" {
		return u
	}
	return ""
}
