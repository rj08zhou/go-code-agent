package background

import (
	"testing"
	"time"
)

func pollTask(t *testing.T, bg *BackgroundManager, id string, timeout time.Duration) bgTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		bg.mu.Lock()
		task, ok := bg.tasks[id]
		if ok && task.Status != "running" {
			tk := *task
			bg.mu.Unlock()
			return tk
		}
		bg.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish within %s", id, timeout)
	return bgTask{}
}

func stripID(t *testing.T, s string) string {
	t.Helper()
	start := len("Background task ")
	end := start
	for end < len(s) && s[end] != ' ' {
		end++
	}
	if end >= len(s) {
		t.Fatalf("could not parse task id from %q", s)
	}
	return s[start:end]
}

func TestRun_Success(t *testing.T) {
	bg := NewBgMgr(t.TempDir(), nil)
	id := stripID(t, bg.Run("echo hello", 10))
	task := pollTask(t, bg, id, 3*time.Second)
	if task.Status != "completed" {
		t.Errorf("status = %q, want completed", task.Status)
	}
	if task.Result != "hello" {
		t.Errorf("result = %q, want hello", task.Result)
	}
}

// Regression: non-zero exit with output must be "error", not "completed".
func TestRun_FailsWithOutput_ReportsError(t *testing.T) {
	bg := NewBgMgr(t.TempDir(), nil)
	id := stripID(t, bg.Run("echo boom; exit 1", 10))
	task := pollTask(t, bg, id, 3*time.Second)
	if task.Status != "error" {
		t.Errorf("status = %q, want error", task.Status)
	}
	if task.Result != "boom" {
		t.Errorf("result = %q, want boom", task.Result)
	}
}

func TestRun_FailsNoOutput(t *testing.T) {
	bg := NewBgMgr(t.TempDir(), nil)
	id := stripID(t, bg.Run("false", 10))
	task := pollTask(t, bg, id, 3*time.Second)
	if task.Status != "error" {
		t.Errorf("status = %q, want error", task.Status)
	}
}

func TestRun_Timeout(t *testing.T) {
	bg := NewBgMgr(t.TempDir(), nil)
	id := stripID(t, bg.Run("sleep 30", 1))
	task := pollTask(t, bg, id, 5*time.Second)
	if task.Status != "timeout" {
		t.Errorf("status = %q, want timeout", task.Status)
	}
}

func TestRun_SecurityBlocked(t *testing.T) {
	validator := func(cmd string) (bool, bool, string) {
		return false, false, "blocked by test policy"
	}
	bg := NewBgMgr(t.TempDir(), validator)
	out := bg.Run("echo hi", 10)
	if out == "" || out[:5] != "Error" {
		t.Errorf("expected security block error, got %q", out)
	}
}

func TestRun_NeedsConfirmBlocked(t *testing.T) {
	validator := func(cmd string) (bool, bool, string) {
		return true, true, "needs confirm"
	}
	bg := NewBgMgr(t.TempDir(), validator)
	out := bg.Run("echo hi", 10)
	if out == "" || out[:5] != "Error" {
		t.Errorf("expected confirm-required block, got %q", out)
	}
}

func TestCheck_UnknownID(t *testing.T) {
	bg := NewBgMgr(t.TempDir(), nil)
	if got := bg.Check("nope"); got != "Unknown: nope" {
		t.Errorf("Check(unknown) = %q, want Unknown: nope", got)
	}
}

func TestDrain(t *testing.T) {
	bg := NewBgMgr(t.TempDir(), nil)
	id := stripID(t, bg.Run("echo done", 10))
	pollTask(t, bg, id, 3*time.Second)
	n := bg.Drain()
	if len(n) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(n))
	}
	if got := bg.Drain(); len(got) != 0 {
		t.Errorf("expected empty drain, got %d", len(got))
	}
}
