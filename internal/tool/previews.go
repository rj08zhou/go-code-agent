package tool

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"go-code-agent/internal/security"
)

func previewWriteFile(scope *ToolScope, args json.RawMessage) (PreviewRequest, error) {
	var a struct{ Path, Content string }
	if err := json.Unmarshal(args, &a); err != nil {
		return PreviewRequest{}, err
	}
	if a.Path == "" {
		return PreviewRequest{}, fmt.Errorf("path is required")
	}
	return PreviewRequest{Path: a.Path, Content: []byte(a.Content)}, nil
}

func previewEditFile(scope *ToolScope, args json.RawMessage) (PreviewRequest, error) {
	var a struct {
		Path, OldText, NewText string
		ReplaceAll             bool `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return PreviewRequest{}, err
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return PreviewRequest{}, err
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return PreviewRequest{}, err
	}
	content, old := string(data), a.OldText
	if !strings.Contains(content, old) {
		for _, line := range strings.Split(content, "\n") {
			if security.WhitespaceNormalize(line) == security.WhitespaceNormalize(old) {
				old = line
				break
			}
		}
	}
	if !strings.Contains(content, old) {
		return PreviewRequest{}, fmt.Errorf("text not found")
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, old, a.NewText)
	} else {
		content = strings.Replace(content, old, a.NewText, 1)
	}
	return PreviewRequest{Path: a.Path, Content: []byte(content)}, nil
}

func previewInsertFile(scope *ToolScope, args json.RawMessage) (PreviewRequest, error) {
	var a struct {
		Path     string `json:"path"`
		InsertAt int    `json:"insert_at"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return PreviewRequest{}, err
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return PreviewRequest{}, err
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return PreviewRequest{}, err
	}
	lines := strings.Split(string(data), "\n")
	idx := a.InsertAt - 1
	if idx < 0 {
		idx = 0
	}
	if idx > len(lines) {
		idx = len(lines)
	}
	updated := append([]string{}, lines[:idx]...)
	updated = append(updated, strings.Split(a.Content, "\n")...)
	updated = append(updated, lines[idx:]...)
	return PreviewRequest{Path: a.Path, Content: []byte(strings.Join(updated, "\n"))}, nil
}

func previewDeleteFile(scope *ToolScope, args json.RawMessage) (PreviewRequest, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return PreviewRequest{}, err
	}
	if _, err := security.SecurePath(scope.Workdir, a.Path, true); err != nil {
		return PreviewRequest{}, err
	}
	return PreviewRequest{Path: a.Path, Delete: true}, nil
}
