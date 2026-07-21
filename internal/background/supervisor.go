// Package background manages long-running background commands.
package background

import (
	"context"
	"fmt"
	"go-code-agent-refactor/internal/config"
	"os/exec"
	"sync"
	"time"
)

// Job represents a running background command.
type Job struct {
	ID      string
	Command string
	Status  string
	Result  string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
}

// Supervisor manages background jobs per session.
type Supervisor struct {
	workdir string
	mu      sync.Mutex
	jobs    map[string]*Job
}

func New(workdir string) *Supervisor {
	return &Supervisor{
		workdir: workdir,
		jobs:    make(map[string]*Job),
	}
}

func (s *Supervisor) Run(sessionID, command string, timeout int) string {
	if timeout <= 0 {
		timeout = int(config.BashTimeout.Seconds())
	}
	jobID := fmt.Sprintf("bg_%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	job := &Job{
		ID:      jobID,
		Command: command,
		Status:  "running",
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	job.cmd = exec.CommandContext(ctx, "sh", "-c", command)
	job.cmd.Dir = s.workdir

	s.mu.Lock()
	s.jobs[jobID] = job
	s.mu.Unlock()

	go func() {
		defer cancel()
		defer close(job.done)
		output, err := job.cmd.CombinedOutput()
		s.mu.Lock()
		if err != nil {
			job.Status = "failed"
			job.Result = fmt.Sprintf("Error: %v\n%s", err, string(output))
		} else {
			job.Status = "completed"
			job.Result = string(output)
		}
		s.mu.Unlock()
	}()

	return fmt.Sprintf("Started background job %s: %s", jobID, command)
}

func (s *Supervisor) Check(taskID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[taskID]
	if !ok {
		return fmt.Sprintf("Job %s not found", taskID)
	}
	select {
	case <-job.done:
		return fmt.Sprintf("[%s] %s: %s", job.Status, taskID, job.Result)
	default:
		return fmt.Sprintf("[%s] %s: %s", job.Status, taskID, job.Command)
	}
}

func (s *Supervisor) Notifications() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var notif []map[string]string
	for id, j := range s.jobs {
		select {
		case <-j.done:
			notif = append(notif, map[string]string{
				"task_id": id,
				"status":  j.Status,
				"result":  j.Result,
			})
		default:
		}
	}
	return notif
}

func (s *Supervisor) Drain() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var notif []map[string]string
	for id, j := range s.jobs {
		select {
		case <-j.done:
			notif = append(notif, map[string]string{
				"task_id": id,
				"status":  j.Status,
				"result":  j.Result,
			})
			delete(s.jobs, id)
		default:
		}
	}
	return notif
}

func (s *Supervisor) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
}
