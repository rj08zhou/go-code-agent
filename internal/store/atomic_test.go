package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestAtomicWrite_CrashRecovery verifies that an interrupted atomic write
// leaves the original file intact, and a leftover fixed-name tmp does not
// block a subsequent successful replace.
func TestAtomicWrite_CrashRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "config.json")

	original := []byte(`{"version": 1}`)
	if err := AtomicWrite(target, original); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Simulate leftover junk at the old predictable temp path.
	tmpPath := target + ".tmp"
	partial := []byte(`{"version": 2`) // intentionally incomplete JSON
	if err := os.WriteFile(tmpPath, partial, 0644); err != nil {
		t.Fatalf("simulated crash write: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after crash: %v", err)
	}
	if string(data) != string(original) {
		t.Fatalf("original file corrupted after crash: got %q, want %q", string(data), string(original))
	}

	next := []byte(`{"version": 3}`)
	if err := AtomicWrite(target, next); err != nil {
		t.Fatalf("write after leftover tmp: %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after recover write: %v", err)
	}
	if string(data) != string(next) {
		t.Fatalf("after recover write: got %q, want %q", string(data), string(next))
	}
}

func TestAtomicWritePreservesExistingPermissions(t *testing.T) {
	target := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(target, []byte("#!/bin/sh\necho ok\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("mode = %o, want 755", got)
	}
}

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

	// Successful writes must not leave their own random temps behind.
	matches, err := filepath.Glob(filepath.Join(tmpDir, ".data.json.*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("tmp files left behind: %v", matches)
	}
}

func TestAtomicWrite_IgnoresPlantedSymlinkTmp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	outside := filepath.Join(dir, "outside-secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(target, []byte("initial")); err != nil {
		t.Fatal(err)
	}

	planted := target + ".tmp"
	if err := os.Symlink(outside, planted); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	if err := AtomicWrite(target, []byte("safe")); err != nil {
		t.Fatalf("AtomicWrite with planted symlink tmp: %v", err)
	}

	gotOutside, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOutside) != "secret" {
		t.Fatalf("planted symlink target was modified: got %q", gotOutside)
	}
	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotTarget) != "safe" {
		t.Fatalf("target = %q, want safe", gotTarget)
	}
}

func TestAtomicWrite_Concurrent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "shared.json")
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- AtomicWrite(target, []byte(fmt.Sprintf("writer-%d", i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent AtomicWrite: %v", err)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("target empty after concurrent writes")
	}
}
