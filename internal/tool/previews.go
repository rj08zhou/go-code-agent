package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return PreviewRequest{}, err
	}
	original, err := os.ReadFile(fp)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return PreviewRequest{}, err
	}
	return PreviewRequest{
		Path:            a.Path,
		OriginalContent: original,
		Content:         []byte(a.Content),
		Existed:         existed,
	}, nil
}

func previewEditFile(scope *ToolScope, args json.RawMessage) (PreviewRequest, error) {
	var a struct {
		Path       string `json:"path"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		ReplaceAll bool   `json:"replace_all"`
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
	content, err := replaceFileContent(string(data), a.OldText, a.NewText, a.ReplaceAll)
	if errors.Is(err, errMutationTextNotFound) {
		return PreviewRequest{}, fmt.Errorf("text not found")
	}
	if err != nil {
		return PreviewRequest{}, err
	}
	return PreviewRequest{
		Path:            a.Path,
		OriginalContent: data,
		Content:         []byte(content),
		Existed:         true,
	}, nil
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
	return PreviewRequest{
		Path:            a.Path,
		OriginalContent: data,
		Content:         []byte(insertFileContent(string(data), a.InsertAt, a.Content)),
		Existed:         true,
	}, nil
}

func previewDeleteFile(scope *ToolScope, args json.RawMessage) (PreviewRequest, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return PreviewRequest{}, err
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return PreviewRequest{}, err
	}
	original, err := os.ReadFile(fp)
	if err != nil {
		return PreviewRequest{}, err
	}
	return PreviewRequest{
		Path:            a.Path,
		OriginalContent: original,
		Existed:         true,
		Delete:          true,
	}, nil
}
