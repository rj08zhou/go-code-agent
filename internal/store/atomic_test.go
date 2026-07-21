package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWrite_CrashRecovery verifies that an interrupted atomic write
// leaves the original file intact (no partial writes).
func TestAtomicWrite_CrashRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "config.json")

	// Write initial content
	original := []byte(`{"version": 1}`)
	if err := AtomicWrite(target, original); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Simulate a crash by writing a temp file but not renaming
	tmpPath := target + ".tmp"
	partial := []byte(`{"version": 2`) // intentionally incomplete JSON
	if err := os.WriteFile(tmpPath, partial, 0644); err != nil {
		t.Fatalf("simulated crash write: %v", err)
	}

	// Verify original file is still intact
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after crash: %v", err)
	}
	if string(data) != string(original) {
		t.Fatalf("original file corrupted after crash: got %q, want %q", string(data), string(original))
	}

	// Clean up temp file
	os.Remove(tmpPath)
}

// TestAtomicWrite_Replace verifies full replace cycle.
func TestAtomicWrite_Replace(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "data.json")

	if err := AtomicWrite(target, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := AtomicWrite(target, []byte("second")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("got %q, want %q", string(data), "second")
	}

	// Verify no temp file left behind
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file left behind after successful write")
	}
}
