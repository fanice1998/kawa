package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerFormatsAndTrigger(t *testing.T) {
	tmpDir := t.TempDir()
	var triggered int32

	sched, err := NewScheduler(tmpDir, func(ctx context.Context, job Job) {
		atomic.AddInt32(&triggered, 1)
	})
	if err != nil {
		t.Fatalf("NewScheduler failed: %v", err)
	}

	// Test @every, @daily, @once, and 5-field cron parsing
	if _, err := sched.AddJob("Daily Report", "@daily 09:30", ModeAgentTask, "Summarize news", "telegram", "123"); err != nil {
		t.Fatalf("AddJob @daily failed: %v", err)
	}
	if _, err := sched.AddJob("Cron Check", "*/15 * * * *", ModeAgentTask, "Run health check", "telegram", "123"); err != nil {
		t.Fatalf("AddJob cron failed: %v", err)
	}
	onceJob, err := sched.AddJob("Quick Reminder", "@once 1s", ModeDirectMessage, "Drink water", "telegram", "123")
	if err != nil {
		t.Fatalf("AddJob @once 1s failed: %v", err)
	}

	if sched.Count() != 3 {
		t.Fatalf("expected 3 scheduled jobs, got %d", sched.Count())
	}

	// Tick at future time (+2 seconds) to fire the @once 1s job
	sched.tick(context.Background(), time.Now().Add(2*time.Second))
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&triggered) != 1 {
		t.Fatalf("expected @once job to trigger once, got %d", triggered)
	}
	// One-shot job should be automatically removed after firing
	if sched.Count() != 2 {
		t.Fatalf("expected 2 remaining recurring jobs after one-shot fired, got %d (onceID=%s)", sched.Count(), onceJob.ID)
	}
}
