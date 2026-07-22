// Command eval runs the regression test suite.
package main

import (
	"context"
	"flag"
	"fmt"
	"go-code-agent/internal/eval"
	"os"
	"time"
)

func main() {
	live := flag.Bool("live", false, "run against real LLM (otherwise mock)")
	model := flag.String("model", "", "LLM model for live mode")
	verbose := flag.Bool("v", false, "verbose output")
	verboseLong := flag.Bool("verbose", false, "verbose output")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-task timeout")
	category := flag.String("category", "", "run only tasks in this category")
	taskName := flag.String("task", "", "run only the task with this name")
	output := flag.String("output", "", "write JSON results to file")
	baselineOut := flag.String("baseline-out", "", "write JSON baseline to this path")
	flag.Parse()

	tasks := eval.DefaultTasks()

	if *category != "" || *taskName != "" {
		filtered := make([]eval.Task, 0)
		for _, t := range tasks {
			if (*category == "" || t.Category == *category) && (*taskName == "" || t.Name == *taskName) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	h := eval.Harness{
		Tasks:   tasks,
		Live:    *live,
		Model:   *model,
		Timeout: *timeout,
		Verbose: *verbose || *verboseLong,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*time.Duration(len(tasks)))
	defer cancel()

	summary := h.Run(ctx)
	fmt.Println(summary.Report())

	outPath := *output
	if outPath == "" {
		outPath = *baselineOut
	}
	if outPath != "" {
		if err := summary.WriteJSON(outPath); err != nil {
			fmt.Fprintf(os.Stderr, "write JSON: %v\n", err)
		}
	}

	exitCode := 0
	if summary.Passed < summary.Total {
		exitCode = 1
	}
	os.Exit(exitCode)
}
