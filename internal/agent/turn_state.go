package agent

import "go-code-agent/internal/llm"

// turnState holds ephemeral counters and flags for a single Run() invocation.
// Runner instances are reused across REPL turns; this state is reset each time.
// Fields are grouped by concern so transitions live next to their data.
type turnState struct {
	rounds       int
	failures     int
	originalTask string
	usage        llm.Usage

	explore  exploreBudget
	failure  failureTracker
	judge    judgeState
	lesson   lessonState
	planning planningState
	tokens   tokenBudget
	tools    toolCallTracker
}

func newTurnState() turnState {
	return turnState{
		explore:  exploreBudget{ReadCounts: make(map[string]int)},
		planning: planningState{LastTriggered: make(map[string]int)},
		tools:    toolCallTracker{CallCounts: make(map[string]int)},
	}
}

// --- explore / post-explore budget ---

type exploreBudget struct {
	Used        bool
	Succeeded   bool // at least one explore tool result succeeded this Run
	Delegations int
	ReadsAfter  int // lead read_file/list_dir count after Succeeded
	ReadCounts  map[string]int
}

// notePathRead increments the per-path read/list counter and returns the new count.
func (e *exploreBudget) notePathRead(path string) int {
	if path == "" {
		return 0
	}
	e.ReadCounts[path]++
	return e.ReadCounts[path]
}

func (e *exploreBudget) noteDelegation() int {
	e.Delegations++
	return e.Delegations
}

func (e *exploreBudget) noteSuccess() {
	e.Used = true
	e.Succeeded = true
}

func (e *exploreBudget) noteLeadRead(role string) {
	if e.Succeeded && role != "explore" {
		e.ReadsAfter++
	}
}

// --- failure streak ---

type failureTracker struct {
	Consecutive         int
	LastTool            string
	RoundsSinceComplete int
}

func (f *failureTracker) noteResult(toolName string, ok bool) {
	if !ok {
		if toolName == f.LastTool {
			f.Consecutive++
		} else {
			f.Consecutive = 1
			f.LastTool = toolName
		}
		return
	}
	f.Consecutive = 0
	f.LastTool = ""
	f.RoundsSinceComplete++
}

func (f *failureTracker) clearConsecutive() { f.Consecutive = 0 }

func (f *failureTracker) clearRoundsSinceComplete() { f.RoundsSinceComplete = 0 }

// --- judge ---

type judgeState struct {
	RetryInjects int
}

// --- lesson stage ---

type lessonState struct {
	Written         bool
	RoundsRemaining int
	PromptInjected  bool
}

// --- planning / reflection inputs ---

type planningState struct {
	PlanEstablished   bool
	RoundsWithoutTodo int
	HasOpenItems      bool
	LastTriggered     map[string]int
}

func (p *planningState) establishPlan() { p.PlanEstablished = true }

func (p *planningState) noteTodoWrite(args string) {
	p.RoundsWithoutTodo = 0
	p.HasOpenItems = false
	for _, item := range parseArgsItems(args) {
		if item["status"] != "completed" {
			p.HasOpenItems = true
			break
		}
	}
	if p.HasOpenItems {
		p.establishPlan()
	}
}

func (p *planningState) noteNonTodoTool() { p.RoundsWithoutTodo++ }

func (p *planningState) clearRoundsWithoutTodo() { p.RoundsWithoutTodo = 0 }

func (p *planningState) markTriggered(kind string, round int) {
	p.LastTriggered[kind] = round
}

// --- token / compaction budget ---

type tokenBudget struct {
	PromptUsed         int64
	Cached             int
	CachedAt           int
	BudgetWarnInjected bool
}

func (t *tokenBudget) invalidateCache() { t.Cached = 0 }

func (t *tokenBudget) refreshCache(tokens, round int) {
	t.Cached = tokens
	t.CachedAt = round
}

// --- identical tool-call tracking ---

type toolCallTracker struct {
	CallCounts map[string]int
}

func (t *toolCallTracker) bump(key string) int {
	t.CallCounts[key]++
	return t.CallCounts[key]
}

func (t *toolCallTracker) count(key string) int { return t.CallCounts[key] }
