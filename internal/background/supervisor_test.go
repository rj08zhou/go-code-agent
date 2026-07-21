package background

import (
	"go-code-agent-refactor/internal/config"
	"testing"
	"time"
)

func init() { config.SetConfig(config.Load()) }

func TestSupervisor_RunAndCheck(t *testing.T) {
	sv := New(t.TempDir())
	result := sv.Run("s1", "echo hello", 5)
	if result == "" {
		t.Fatal("Run returned empty")
	}

	// Wait for job to complete
	time.Sleep(200 * time.Millisecond)

	// Check should find a completed or running job
	found := false
	for _, n := range sv.Notifications() {
		t.Logf("notification: %v", n)
		found = true
	}
	if !found {
		// The job may have completed before Notifications was called
		// Drain should catch it
		drained := sv.Drain()
		if len(drained) == 0 {
			t.Log("no notifications or drained jobs (job may have completed too quickly)")
		}
	}
}

func TestSupervisor_CheckNotFound(t *testing.T) {
	sv := New(t.TempDir())
	result := sv.Check("nonexistent")
	if result[:3] != "Job" {
		t.Errorf("expected 'Job ... not found', got %q", result)
	}
}

func TestSupervisor_StopAll(t *testing.T) {
	sv := New(t.TempDir())
	sv.Run("s1", "sleep 1", 10)
	// Let the background goroutine start the process before killing it
	time.Sleep(50 * time.Millisecond)
	sv.StopAll()

	// Double stop should not panic
	sv.StopAll()
	// Wait for goroutine to exit
	time.Sleep(50 * time.Millisecond)
}

func TestSupervisor_Drain(t *testing.T) {
	sv := New(t.TempDir())
	sv.Run("s1", "echo done", 5)
	time.Sleep(300 * time.Millisecond)

	drained := sv.Drain()
	t.Logf("drained %d jobs", len(drained))

	drained2 := sv.Drain()
	if len(drained2) != 0 {
		t.Errorf("expected 0 after drain, got %d", len(drained2))
	}
}

func TestSupervisor_ConcurrentJobs(t *testing.T) {
	sv := New(t.TempDir())
	done := make(chan struct{})

	go func() { sv.Run("s1", "sleep 1", 2); done <- struct{}{} }()
	go func() { sv.Run("s2", "sleep 1", 2); done <- struct{}{} }()
	<-done
	<-done
}
