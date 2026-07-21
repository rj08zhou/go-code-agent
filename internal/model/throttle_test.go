package model

import (
	"context"
	"go-code-agent-refactor/internal/config"
	"testing"
)

func init() { config.SetConfig(config.Load()) }

func TestRoleThrottle_AcquireRelease(t *testing.T) {
	th := NewRoleThrottle(10)
	release, err := th.Acquire(context.Background(), "lead")
	if err != nil {
		t.Fatalf("failed to acquire: %v", err)
	}
	release()
}

func TestRoleThrottle_EachRoleHasCapacity(t *testing.T) {
	th := NewRoleThrottle(10)
	for _, role := range []string{"lead", "explore", "teammate", "judge"} {
		cap := th.Capacity(role)
		if cap < 1 {
			t.Errorf("%s: capacity should be >= 1, got %d", role, cap)
		}
	}
}

func TestRoleThrottle_LeadGetsLargestShare(t *testing.T) {
	th := NewRoleThrottle(20)
	leadCap := th.Capacity("lead")
	exploreCap := th.Capacity("explore")
	if leadCap < exploreCap {
		t.Errorf("lead(%d) should have >= capacity than explore(%d)", leadCap, exploreCap)
	}
}

func TestRoleThrottle_UnknownRoleFallsBack(t *testing.T) {
	th := NewRoleThrottle(10)
	release, err := th.Acquire(context.Background(), "unknown-role")
	if err != nil {
		t.Fatalf("unknown role should fall back to default bucket: %v", err)
	}
	release()
}

func TestRoleThrottle_ContextCancel(t *testing.T) {
	th := NewRoleThrottle(1)
	release, _ := th.Acquire(context.Background(), "lead")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := th.Acquire(ctx, "lead")
	if err == nil {
		release()
		t.Fatal("expected context cancellation error")
	}
	release()
}

func TestRoleThrottle_ConcurrentRelease(t *testing.T) {
	th := NewRoleThrottle(10)
	// lead capacity: floor(10 * 0.4) = 4
	release1, _ := th.Acquire(context.Background(), "lead")
	release2, _ := th.Acquire(context.Background(), "lead")

	done := make(chan struct{})
	go func() {
		defer close(done)
		release1()
	}()
	release2()
	<-done
}
