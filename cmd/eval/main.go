// Command eval runs the go-code-agent evaluation harness.
//
// It is a thin CLI wrapper around internal/eval: it selects a task
// subset, drives the harness, prints the human-readable report and
// (optionally) writes a JSON baseline. The harness itself supports
// both an offline mock provider (default) and a live LLM provider
// (--live), and grades each task via its machine-checkable Verify.
//
// Usage:
//
//	go run ./cmd/eval/                       # mock mode, all tasks
//	go run ./cmd/eval/ --task=fix-off-by-one
//	go run ./cmd/eval/ --live --model=gpt-4o --baseline-out=baseline.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-code-agent/internal/eval"
)

func main() {
	var (
		live     = flag.Bool("live", false, "use real LLM provider (default: scripted mock)")
		model    = flag.String("model", "", "model id (default: mock in mock mode; required in --live unless set in env)")
		timeout  = flag.Duration("timeout", 5*time.Minute, "per-task timeout")
		taskName = flag.String("task", "", "run only the task with this name (default: all)")
		baseOut  = flag.String("baseline-out", "", "write JSON baseline to this path")
		verbose  = flag.Bool("verbose", false, "print per-task progress to stderr")
	)
	flag.Parse()

	tasks := eval.Tasks
	if *taskName != "" {
		picked := tasks[:0]
		for _, t := range tasks {
			if t.Name == *taskName {
				picked = append(picked, t)
			}
		}
		tasks = picked
	}
	if len(tasks) == 0 {
		fmt.Fprintln(os.Stderr, "no tasks to run")
		os.Exit(1)
	}

	h := eval.New(tasks, *live, *model)
	h.Timeout = *timeout
	h.Verbose = *verbose

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h.Run(ctx)
	s := h.Summarize()
	fmt.Print(s.Report())

	if *baseOut != "" {
		if err := s.WriteJSON(*baseOut); err != nil {
			fmt.Fprintln(os.Stderr, "baseline:", err)
			os.Exit(1)
		}
	}

	if s.Passed < s.Total {
		os.Exit(1)
	}
}
