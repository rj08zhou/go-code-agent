package application

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDataDirSeparatesSameNamedWorkspaces(t *testing.T) {
	cfgDir := t.TempDir()
	root := t.TempDir()
	first := filepath.Join(root, "company", "api")
	second := filepath.Join(root, "personal", "api")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	firstData := ResolveDataDir(cfgDir, first)
	secondData := ResolveDataDir(cfgDir, second)
	if firstData == secondData {
		t.Fatalf("same-named workspaces share state directory %q", firstData)
	}
	if firstData != ResolveDataDir(cfgDir, first) {
		t.Fatal("state directory is not deterministic")
	}
	for _, dataDir := range []string{firstData, secondData} {
		if filepath.Dir(dataDir) != filepath.Join(cfgDir, "go-code-agent") {
			t.Fatalf("state directory %q has unexpected parent", dataDir)
		}
		if !strings.HasPrefix(filepath.Base(dataDir), "api-") {
			t.Fatalf("state directory %q is not recognizable by workspace basename", dataDir)
		}
	}
}

func TestResolveDataDirNormalizesWorkspacePath(t *testing.T) {
	cfgDir := t.TempDir()
	workdir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, workdir)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := ResolveDataDir(cfgDir, relative), ResolveDataDir(cfgDir, workdir); got != want {
		t.Fatalf("relative and absolute paths resolve differently: got %q, want %q", got, want)
	}
}
