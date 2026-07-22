package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreatesDailyDir(t *testing.T) {
	root := t.TempDir()
	// Construct store without going through NewStore so daily/ is absent.
	s := &Store{dataDir: root, dailyDir: filepath.Join(root, "daily")}

	out := s.Write("hello from test", "fact")
	if strings.HasPrefix(out, "Error") {
		t.Fatalf("Write failed: %s", out)
	}
	entries, err := os.ReadDir(s.dailyDir)
	if err != nil {
		t.Fatalf("daily dir missing: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected a daily jsonl file")
	}
}
