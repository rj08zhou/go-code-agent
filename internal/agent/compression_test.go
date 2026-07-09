package agent

import (
	"go-code-agent/internal/llm"
	"testing"
)

// helper builders for readable message sequences
func sysMsg() llm.Message  { return llm.SystemMessage("sys") }
func userMsg() llm.Message { return llm.UserMessage("u") }
func asstText() llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: "a"}
}
func asstCall(id string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "bash", Arguments: "{}"}}}
}
func toolRes(id string) llm.Message { return llm.ToolMessage("out", id) }

// assertNoOrphans verifies the invariant AutoCompact relies on: after
// splitting at `split`, neither side orphans a tool_call/tool_result.
func assertNoOrphans(t *testing.T, msgs []llm.Message, split int) {
	t.Helper()
	if split == 0 {
		return // summarize-everything fallback is always safe
	}
	// tail must not start with a tool result
	if msgs[split].Role == llm.RoleTool {
		t.Fatalf("split=%d: tail starts with orphaned tool_result", split)
	}
	// prefix must not end with an assistant that has pending tool_calls
	if prev := msgs[split-1]; prev.Role == llm.RoleAssistant && len(prev.ToolCalls) > 0 {
		t.Fatalf("split=%d: prefix ends with assistant tool_call whose results are in the tail", split)
	}
}

func TestFindCompactionSplit_PrefersUserBoundary(t *testing.T) {
	// system, [old...], user, asst, ... keepRecent small so desired lands
	// near the end; a user boundary should be chosen.
	msgs := []llm.Message{
		sysMsg(),
		userMsg(), asstText(),
		asstCall("1"), toolRes("1"), asstText(),
		userMsg(), // <- clean boundary
		asstCall("2"), toolRes("2"), asstText(),
	}
	split := findCompactionSplit(msgs, 4)
	assertNoOrphans(t, msgs, split)
	if msgs[split].Role != llm.RoleUser {
		t.Errorf("expected a user-message boundary, got split=%d role=%v", split, msgs[split].Role)
	}
}

func TestFindCompactionSplit_AvoidsOrphanToolResult(t *testing.T) {
	// desired split would land exactly on a tool result; must be adjusted.
	msgs := []llm.Message{
		sysMsg(),
		asstText(),
		asstCall("1"), toolRes("1"), // desired might point at toolRes
		asstText(),
		asstCall("2"), toolRes("2"),
		asstText(),
	}
	for keep := 1; keep < len(msgs); keep++ {
		split := findCompactionSplit(msgs, keep)
		assertNoOrphans(t, msgs, split)
	}
}

func TestFindCompactionSplit_NoUserTurns(t *testing.T) {
	// A long autonomous run with no user messages: must still find a safe
	// boundary (after a completed tool result) and never orphan.
	msgs := []llm.Message{
		sysMsg(),
		asstCall("1"), toolRes("1"),
		asstCall("2"), toolRes("2"),
		asstCall("3"), toolRes("3"),
		asstText(),
	}
	for keep := 1; keep < len(msgs); keep++ {
		split := findCompactionSplit(msgs, keep)
		assertNoOrphans(t, msgs, split)
	}
}

func TestFindCompactionSplit_ShortConversation(t *testing.T) {
	msgs := []llm.Message{sysMsg(), userMsg(), asstText()}
	if got := findCompactionSplit(msgs, 20); got != 0 {
		t.Errorf("short conversation should return 0 (nothing to summarize), got %d", got)
	}
}

// TestFindCompactionSplit_DistantUserBoundaryNotPreferred verifies the
// bounded-drift rule: when the only user boundary is far below desired
// (a long autonomous run since the last user turn), we must NOT snap to
// it (that would keep almost everything verbatim). We fall back to the
// nearest safe boundary, keeping the tail close to keepRecent.
func TestFindCompactionSplit_DistantUserBoundaryNotPreferred(t *testing.T) {
	// index: 0 sys, 1 user, then a long autonomous run of asst-text.
	msgs := []llm.Message{sysMsg(), userMsg()}
	for i := 0; i < 30; i++ {
		msgs = append(msgs, asstText())
	}
	n := len(msgs) // 32
	keep := 5
	desired := n - keep // 27
	split := findCompactionSplit(msgs, keep)
	assertNoOrphans(t, msgs, split)

	// The lone user boundary is at index 1, far below desired(27) and
	// below minPreferUser(desired-keep=22), so it must NOT be chosen.
	if split == 1 {
		t.Fatalf("distant user boundary at 1 should not be preferred")
	}
	// Should land at/very near desired (all asst-text boundaries are
	// safe), i.e. keep ~keepRecent verbatim, not ~everything.
	if split < desired-keep || split > desired {
		t.Errorf("split=%d not within bounded window [%d,%d] of desired", split, desired-keep, desired)
	}
}

// TestFindCompactionSplit_NearUserBoundaryPreferred is the complement:
// a user boundary within the drift window IS preferred over a closer
// non-user safe boundary.
func TestFindCompactionSplit_NearUserBoundaryPreferred(t *testing.T) {
	// Construct so the user turn lands exactly at desired = len-keep:
	//   sys + N*asst + user + (keep-1)*asst
	//   len = N + keep + 1 ; desired = len - keep = N + 1 = userIdx.
	keep := 5
	msgs := []llm.Message{sysMsg()}
	for i := 0; i < 10; i++ { // N = 10
		msgs = append(msgs, asstText())
	}
	userIdx := len(msgs) // = N+1 = 11
	msgs = append(msgs, userMsg())
	for i := 0; i < keep-1; i++ { // keep-1 messages after the user turn
		msgs = append(msgs, asstText())
	}
	desired := len(msgs) - keep
	if desired != userIdx {
		t.Fatalf("test setup: desired(%d) != userIdx(%d)", desired, userIdx)
	}
	split := findCompactionSplit(msgs, keep)
	assertNoOrphans(t, msgs, split)
	if split != userIdx {
		t.Errorf("split=%d, expected the in-window user boundary at %d", split, userIdx)
	}
}

func TestIsSafeSplit(t *testing.T) {
	msgs := []llm.Message{
		sysMsg(),      // 0
		asstCall("1"), // 1
		toolRes("1"),  // 2
		userMsg(),     // 3
		asstText(),    // 4
	}
	// s=2 -> tail starts with toolRes -> unsafe
	if isSafeSplit(msgs, 2) {
		t.Errorf("s=2 should be unsafe (tail starts with tool result)")
	}
	// s=2 also: prev(1) is assistant with tool_calls -> unsafe (double reason)
	// s=3 -> prev(2)=toolRes, msgs[3]=user -> safe
	if !isSafeSplit(msgs, 3) {
		t.Errorf("s=3 should be safe (user boundary after completed tool result)")
	}
	// s=1 -> prev(0)=system, msgs[1]=asstCall (not tool) -> safe by rule
	if !isSafeSplit(msgs, 1) {
		t.Errorf("s=1 should be safe")
	}
	// out of range
	if isSafeSplit(msgs, 0) || isSafeSplit(msgs, len(msgs)) {
		t.Errorf("out-of-range indices must be unsafe")
	}
}
