package tool

import (
	"errors"
	"testing"
)

func TestReplaceFileContent(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		oldText    string
		newText    string
		replaceAll bool
		want       string
		wantErr    error
	}{
		{
			name:    "replace first exact occurrence",
			content: "before before",
			oldText: "before",
			newText: "after",
			want:    "after before",
		},
		{
			name:       "replace all exact occurrences",
			content:    "before before",
			oldText:    "before",
			newText:    "after",
			replaceAll: true,
			want:       "after after",
		},
		{
			name:    "replace exact substring before whitespace fallback",
			content: "  before  \nother",
			oldText: "before",
			newText: "after",
			want:    "  after  \nother",
		},
		{
			name:    "replace whitespace-normalized line fallback",
			content: "before\titem\nother",
			oldText: "before item",
			newText: "after",
			want:    "after\nother",
		},
		{
			name:    "text not found",
			content: "before",
			oldText: "missing",
			newText: "after",
			wantErr: errMutationTextNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replaceFileContent(tt.content, tt.oldText, tt.newText, tt.replaceAll)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("replaceFileContent error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("replaceFileContent result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInsertFileContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		insertAt int
		inserted string
		want     string
	}{
		{
			name:     "insert at one-based line",
			content:  "one\ntwo\nthree",
			insertAt: 2,
			inserted: "middle",
			want:     "one\nmiddle\ntwo\nthree",
		},
		{
			name:     "clamp before first line",
			content:  "one\ntwo",
			insertAt: 0,
			inserted: "first",
			want:     "first\none\ntwo",
		},
		{
			name:     "clamp after final line",
			content:  "one\ntwo",
			insertAt: 99,
			inserted: "last",
			want:     "one\ntwo\nlast",
		},
		{
			name:     "insert multiple lines",
			content:  "one\ntwo",
			insertAt: 2,
			inserted: "middle-a\nmiddle-b",
			want:     "one\nmiddle-a\nmiddle-b\ntwo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := insertFileContent(tt.content, tt.insertAt, tt.inserted); got != tt.want {
				t.Fatalf("insertFileContent result = %q, want %q", got, tt.want)
			}
		})
	}
}
