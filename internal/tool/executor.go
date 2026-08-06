package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
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

// ChainSanitizers applies sanitizers in declaration order. Use it when a
// role needs both a safety transform (such as secret redaction) and a
// role-specific presentation transform (such as output truncation).
func ChainSanitizers(sanitizers ...OutputSanitizer) OutputSanitizer {
	filtered := make([]OutputSanitizer, 0, len(sanitizers))
	for _, sanitizer := range sanitizers {
		if sanitizer != nil {
			filtered = append(filtered, sanitizer)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return chainedSanitizer(filtered)
	}
}

type chainedSanitizer []OutputSanitizer

func (s chainedSanitizer) Sanitize(output string) string {
	for _, sanitizer := range s {
		output = sanitizer.Sanitize(output)
	}
	return output
}

// DecisionLogger records authorization and approval decisions.
type DecisionLogger interface {
	Record(tool, action, reason string, round int)
}

// DiffTextApprovalChecker can attach rendered diff text to an approval decision.
type DiffTextApprovalChecker interface {
	AllowToolWithDiffText(toolName string, args json.RawMessage, diffText string) (bool, string)
}

// DetailedApprovalChecker preserves HITL's allow/reject/modify decision and
// returns any approved replacement content with the same invocation result.
type DetailedApprovalChecker interface {
	DecideTool(toolName string, args json.RawMessage, approvalInput MutationApprovalInput) ApprovalResult
}

type MutationApprovalInput struct {
	DiffText string
	Plan     *MutationPlan
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

// preparedCall is the resolved, schema-validated tool invocation.
type preparedCall struct {
	def     ToolDefinition
	handler ToolHandler
	args    json.RawMessage
}

// authorizedCall is a preparedCall that has passed capability, path, mutation
// planning, approval, and network gates. ReplacementContent, when set, overrides
// the mutation body approved for the handler.
type authorizedCall struct {
	preparedCall
	approvalInput      MutationApprovalInput
	replacementContent *string
}

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
// The pipeline is intentionally ordered as:
//
//	prepare → authorize → invoke
//
// so each stage has one job and early-exit semantics stay local.
func (e *Executor) Execute(ctx context.Context, scope *ToolScope, tc llm.ToolCall) Result {
	started := time.Now()

	prepared, early, ok := e.prepareExecution(scope, tc)
	if !ok {
		return early
	}
	authorized, early, ok := e.authorizeExecution(scope, tc, prepared)
	if !ok {
		return early
	}
	return e.invokeExecution(ctx, scope, tc, authorized, started)
}

// prepareExecution validates arguments and resolves the tool definition/handler.
func (e *Executor) prepareExecution(scope *ToolScope, tc llm.ToolCall) (preparedCall, Result, bool) {
	if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
		return preparedCall{}, InvalidArgs(fmt.Sprintf("tool call '%s' has truncated arguments", tc.Name)), false
	}

	snap := e.catalog.Load()
	def, known := snap.Definitions[tc.Name]
	handler, hasHandler := snap.Handlers[tc.Name]
	if !known || !hasHandler {
		return preparedCall{}, Unavailable(fmt.Sprintf("unknown tool %q", tc.Name)), false
	}
	if scope == nil {
		return preparedCall{}, Denied(fmt.Sprintf("tool %q requires an execution scope", tc.Name)), false
	}
	if strings.HasPrefix(tc.Name, "mcp__") &&
		(!def.Effects.Declared() || def.HasEffect(EffectUnclassified)) &&
		e.approval == nil {
		return preparedCall{}, Denied(fmt.Sprintf("MCP tool %q has unclassified effects and requires an approval policy", tc.Name)), false
	}

	return preparedCall{
		def:     def,
		handler: handler,
		args:    json.RawMessage(tc.Arguments),
	}, Result{}, true
}

// authorizeExecution enforces capability, path, preview, approval, and network
// gates. Order matches the historical Execute pipeline and must stay stable.
func (e *Executor) authorizeExecution(scope *ToolScope, tc llm.ToolCall, prepared preparedCall) (authorizedCall, Result, bool) {
	def := prepared.def
	args := prepared.args

	if early, ok := e.checkCapabilities(scope, tc.Name, def); !ok {
		return authorizedCall{}, early, false
	}
	if early, ok := e.checkAllowedRoots(scope, def, string(args)); !ok {
		return authorizedCall{}, early, false
	}

	approvalInput, early, ok := e.planMutation(scope, def, args)
	if !ok {
		return authorizedCall{}, early, false
	}

	replacement, early, ok := e.decideApproval(scope, tc.Name, args, approvalInput)
	if !ok {
		return authorizedCall{}, early, false
	}

	if early, ok := e.checkNetwork(scope, def, string(args)); !ok {
		return authorizedCall{}, early, false
	}

	return authorizedCall{
		preparedCall:       prepared,
		approvalInput:      approvalInput,
		replacementContent: replacement,
	}, Result{}, true
}

func (e *Executor) checkCapabilities(scope *ToolScope, name string, def ToolDefinition) (Result, bool) {
	switch {
	case def.HasEffect(EffectExecuteProcess) && !scope.CanExecute:
		return Denied(fmt.Sprintf("tool %q requires execute capability", name)), false
	case def.HasEffect(EffectWriteFile) && !scope.CanWrite:
		return Denied(fmt.Sprintf("tool %q requires write capability", name)), false
	case def.HasEffect(EffectDeleteFile) && !scope.CanWrite:
		return Denied(fmt.Sprintf("tool %q requires write capability (delete)", name)), false
	case def.HasEffect(EffectReadFile) && !scope.CanRead:
		return Denied(fmt.Sprintf("tool %q requires read capability", name)), false
	case def.HasEffect(EffectNetworkAccess) && !scope.CanNetwork:
		return Denied(fmt.Sprintf("tool %q requires network capability", name)), false
	case def.HasEffect(EffectMemoryMutation) && !scope.CanMemory:
		return Denied(fmt.Sprintf("tool %q requires memory capability", name)), false
	case def.HasEffect(EffectTeamMutation) && !scope.CanTeam:
		return Denied(fmt.Sprintf("tool %q requires team capability", name)), false
	}
	return Result{}, true
}

func (e *Executor) checkAllowedRoots(scope *ToolScope, def ToolDefinition, args string) (Result, bool) {
	if def.HasEffect(EffectReadFile) || def.HasEffect(EffectWriteFile) || def.HasEffect(EffectDeleteFile) {
		if path := extractPath(args); path != "" && !pathAllowed(scope, path) {
			return Denied(fmt.Sprintf("path %q is outside allowed roots", path)), false
		}
	}
	return Result{}, true
}

func (e *Executor) planMutation(scope *ToolScope, def ToolDefinition, args json.RawMessage) (MutationApprovalInput, Result, bool) {
	approvalInput := MutationApprovalInput{}
	if def.PlanMutation != nil && scope.DiffPreview == nil {
		return MutationApprovalInput{}, Denied("diff renderer is required for mutating tools"), false
	}
	if def.PlanMutation != nil && scope.DiffPreview != nil {
		req, err := def.PlanMutation(scope, args)
		if err != nil {
			return MutationApprovalInput{}, Denied(fmt.Sprintf("cannot plan mutation: %v", err)), false
		}
		text, err := scope.DiffPreview.PreviewChange(
			req.Path,
			req.OriginalContent,
			req.Content,
		)
		if err != nil {
			return MutationApprovalInput{}, Denied(fmt.Sprintf("cannot render mutation diff: %v", err)), false
		}
		approvalInput.DiffText = text
		approvalInput.Plan = &req
	}
	return approvalInput, Result{}, true
}

func (e *Executor) decideApproval(scope *ToolScope, name string, args json.RawMessage, approvalInput MutationApprovalInput) (*string, Result, bool) {
	var replacementContent *string
	if e.approval != nil {
		if detailed, ok := e.approval.(DetailedApprovalChecker); ok {
			approvalResult := detailed.DecideTool(name, args, approvalInput)
			switch approvalResult.Decision {
			case ApprovalRejected:
				return nil, Rejected(approvalResult.Reason), false
			case ApprovalModified:
				return nil, Modified(approvalResult.Feedback), false
			}
			if approvalResult.ReplacementContent != nil {
				if approvalInput.Plan == nil || approvalInput.Plan.Delete {
					return nil, Denied("approval returned replacement content for a non-write mutation"), false
				}
				replacementContent = approvalResult.ReplacementContent
			}
		} else if checker, ok := e.approval.(DiffTextApprovalChecker); ok {
			if allowed, reason := checker.AllowToolWithDiffText(name, args, approvalInput.DiffText); !allowed {
				return nil, Rejected(reason), false
			}
		} else if allowed, reason := e.approval.AllowTool(name, args); !allowed {
			return nil, Rejected(reason), false
		}
	}

	if scope.ApprovalPolicy != nil {
		if allowed, reason := scope.ApprovalPolicy.AllowTool(name, args); !allowed {
			return nil, Rejected(reason), false
		}
	}
	return replacementContent, Result{}, true
}

func (e *Executor) checkNetwork(scope *ToolScope, def ToolDefinition, args string) (Result, bool) {
	if !def.HasEffect(EffectNetworkAccess) {
		return Result{}, true
	}
	if scope.NetworkPolicy != nil {
		for _, rawURL := range extractURLs(args) {
			if !scope.NetworkPolicy.AllowURL(rawURL) {
				return Denied(fmt.Sprintf("URL %q blocked by network policy", rawURL)), false
			}
		}
	}
	if e.network != nil {
		for _, rawURL := range extractURLs(args) {
			if !e.network.AllowURL(rawURL) {
				return Denied(fmt.Sprintf("URL %q blocked by network policy", rawURL)), false
			}
		}
	}
	return Result{}, true
}

// invokeExecution runs the authorized handler under timeout and finalizes the result.
func (e *Executor) invokeExecution(ctx context.Context, scope *ToolScope, tc llm.ToolCall, call authorizedCall, started time.Time) Result {
	toolTimeout := e.timeout
	if call.def.Timeout > 0 {
		toolTimeout = call.def.Timeout
	}

	callCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	resultCh := make(chan Result, 1)
	handlerScope := *scope
	handlerScope.Context = callCtx
	if call.approvalInput.Plan != nil {
		request := *call.approvalInput.Plan
		request.OriginalContent = append([]byte(nil), request.OriginalContent...)
		request.Content = append([]byte(nil), request.Content...)
		if call.replacementContent != nil {
			request.Content = []byte(*call.replacementContent)
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
			return call.handler(&handlerScope, call.args)
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
	raw = security.MapPathIntoWorkdir(scope.Workdir, scope.SourceWorkdir, raw)
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

// extractURLs finds network destination fields in nested JSON arguments.
// It intentionally inspects only URL-shaped field names so arbitrary prose
// containing a URL does not turn every network-capable tool into a rejection.
func extractURLs(args string) []string {
	if args == "" {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(args), &value) != nil {
		return nil
	}

	keys := map[string]struct{}{
		"url": {}, "uri": {}, "endpoint": {}, "webhook": {}, "callback_url": {},
		"callbackurl": {},
	}
	var urls []string
	seen := make(map[string]struct{})
	var walk func(any)
	walk = func(current any) {
		switch value := current.(type) {
		case map[string]any:
			for key, child := range value {
				if _, ok := keys[strings.ToLower(key)]; ok {
					if rawURL, ok := child.(string); ok && rawURL != "" {
						if _, duplicate := seen[rawURL]; !duplicate {
							seen[rawURL] = struct{}{}
							urls = append(urls, rawURL)
						}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(value)
	return urls
}
