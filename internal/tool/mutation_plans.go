package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"go-code-agent/internal/security"
)

func planWriteMutation(scope *ToolScope, args json.RawMessage) (MutationPlan, error) {
	var a struct{ Path, Content string }
	if err := json.Unmarshal(args, &a); err != nil {
		return MutationPlan{}, err
	}
	if a.Path == "" {
		return MutationPlan{}, fmt.Errorf("path is required")
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return MutationPlan{}, err
	}
	original, err := os.ReadFile(fp)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return MutationPlan{}, err
	}
	return MutationPlan{
		Path:            a.Path,
		OriginalContent: original,
		Content:         []byte(a.Content),
		Existed:         existed,
	}, nil
}

func planEditMutation(scope *ToolScope, args json.RawMessage) (MutationPlan, error) {
	var a struct {
		Path       string `json:"path"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return MutationPlan{}, err
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return MutationPlan{}, err
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return MutationPlan{}, err
	}
	content, err := replaceFileContent(string(data), a.OldText, a.NewText, a.ReplaceAll)
	if errors.Is(err, errMutationTextNotFound) {
		return MutationPlan{}, fmt.Errorf("text not found")
	}
	if err != nil {
		return MutationPlan{}, err
	}
	return MutationPlan{
		Path:            a.Path,
		OriginalContent: data,
		Content:         []byte(content),
		Existed:         true,
	}, nil
}

func planInsertMutation(scope *ToolScope, args json.RawMessage) (MutationPlan, error) {
	var a struct {
		Path     string `json:"path"`
		InsertAt int    `json:"insert_at"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return MutationPlan{}, err
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return MutationPlan{}, err
	}
	data, err := os.ReadFile(fp)
	if err != nil {
		return MutationPlan{}, err
	}
	return MutationPlan{
		Path:            a.Path,
		OriginalContent: data,
		Content:         []byte(insertFileContent(string(data), a.InsertAt, a.Content)),
		Existed:         true,
	}, nil
}

func planDeleteMutation(scope *ToolScope, args json.RawMessage) (MutationPlan, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return MutationPlan{}, err
	}
	fp, err := security.SecurePath(scope.Workdir, a.Path, true)
	if err != nil {
		return MutationPlan{}, err
	}
	original, err := os.ReadFile(fp)
	if err != nil {
		return MutationPlan{}, err
	}
	return MutationPlan{
		Path:            a.Path,
		OriginalContent: original,
		Existed:         true,
		Delete:          true,
	}, nil
}
