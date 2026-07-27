package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

type fakeMemoryService struct {
	writeContent  string
	writeCategory string
	searchQuery   string
	searchTopK    int
	searchDays    int
	searchCat     string
	deleteQuery   string
	sessionID     string
	summary       string
}

func (f *fakeMemoryService) Write(content, category string) string {
	f.writeContent, f.writeCategory = content, category
	return "memory written"
}
func (f *fakeMemoryService) Search(query string, topK, withinDays int, category string) string {
	f.searchQuery, f.searchTopK, f.searchDays, f.searchCat = query, topK, withinDays, category
	return "hits"
}
func (f *fakeMemoryService) Delete(query, category string) string {
	f.deleteQuery = query
	return "deleted"
}
func (f *fakeMemoryService) Stats() string { return "stats" }
func (f *fakeMemoryService) SaveSessionMemory(sessionID, summary string) string {
	f.sessionID, f.summary = sessionID, summary
	return "session saved"
}

func TestMemoryTools(t *testing.T) {
	mem := &fakeMemoryService{}
	defs := memoryTools(builtinDeps{memorySvc: mem})
	scope := &ToolScope{SessionID: "sess-1", AgentID: "lead"}

	tests := []struct {
		name       string
		tool       string
		args       json.RawMessage
		nilSvc     bool
		wantStatus Status
		wantSubstr string
		check      func(t *testing.T)
	}{
		{
			name:       "memory_write defaults category",
			tool:       "memory_write",
			args:       json.RawMessage(`{"content":"prefer tabs"}`),
			wantStatus: StatusSucceeded,
			check: func(t *testing.T) {
				if mem.writeContent != "prefer tabs" || mem.writeCategory != "fact" {
					t.Fatalf("write = %q/%q", mem.writeContent, mem.writeCategory)
				}
			},
		},
		{
			name:       "memory_write explicit category",
			tool:       "memory_write",
			args:       json.RawMessage(`{"content":"use gofmt","category":"style"}`),
			wantStatus: StatusSucceeded,
			check: func(t *testing.T) {
				if mem.writeCategory != "style" {
					t.Fatalf("category = %q", mem.writeCategory)
				}
			},
		},
		{
			name:       "memory_write nil service",
			tool:       "memory_write",
			args:       json.RawMessage(`{"content":"x"}`),
			nilSvc:     true,
			wantStatus: StatusFailed,
			wantSubstr: "memory service unavailable",
		},
		{
			name:       "memory_search defaults top_k",
			tool:       "memory_search",
			args:       json.RawMessage(`{"query":"tabs"}`),
			wantStatus: StatusSucceeded,
			check: func(t *testing.T) {
				if mem.searchQuery != "tabs" || mem.searchTopK != 5 {
					t.Fatalf("search = %q topK=%d", mem.searchQuery, mem.searchTopK)
				}
			},
		},
		{
			name:       "memory_delete delegates",
			tool:       "memory_delete",
			args:       json.RawMessage(`{"query":"old fact"}`),
			wantStatus: StatusSucceeded,
			check: func(t *testing.T) {
				if mem.deleteQuery != "old fact" {
					t.Fatalf("deleteQuery = %q", mem.deleteQuery)
				}
			},
		},
		{
			name:       "session_save_memory requires summary",
			tool:       "session_save_memory",
			args:       json.RawMessage(`{"summary":""}`),
			wantStatus: StatusFailed,
			wantSubstr: "summary is required",
		},
		{
			name:       "session_save_memory passes SessionID",
			tool:       "session_save_memory",
			args:       json.RawMessage(`{"summary":"fixed the bug"}`),
			wantStatus: StatusSucceeded,
			check: func(t *testing.T) {
				if mem.sessionID != "sess-1" || mem.summary != "fixed the bug" {
					t.Fatalf("save = %q/%q", mem.sessionID, mem.summary)
				}
			},
		},
		{
			name:       "memory_stats nil service",
			tool:       "memory_stats",
			args:       json.RawMessage(`{}`),
			nilSvc:     true,
			wantStatus: StatusFailed,
			wantSubstr: "memory service unavailable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			use := defs
			if tc.nilSvc {
				use = memoryTools(builtinDeps{})
			}
			tool := mustTool(t, use, tc.tool)
			got := tool.Handler(scope, tc.args)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (output=%q)", got.Status, tc.wantStatus, got.Output)
			}
			if tc.wantSubstr != "" && !strings.Contains(got.Output, tc.wantSubstr) {
				t.Fatalf("output %q missing %q", got.Output, tc.wantSubstr)
			}
			if tc.check != nil {
				tc.check(t)
			}
		})
	}
}
