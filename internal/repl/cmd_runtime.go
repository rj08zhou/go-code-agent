package repl

import (
	"context"
	"fmt"

	"go-code-agent/internal/llm"
)

func (r *Loop) handleRuntimeCommand(ctx context.Context, parts []string, messages *[]llm.Message) {
	switch parts[0] {
	case "/judge":
		if r.built.Runtime.Judge.IsEnabled() {
			r.built.Runtime.Judge.SetEnabled(false)
		} else {
			r.built.Runtime.Judge.SetEnabled(true)
		}
		fmt.Printf("Judge: %v\n", r.built.Runtime.Judge.IsEnabled())
	case "/usage":
		if r.built.Session.Usage != nil {
			fmt.Println(r.built.Session.Usage.Render())
		} else {
			fmt.Println("Usage tracking not available.")
		}
	case "/compact":
		if r.built.Session.Compact == nil {
			fmt.Println("Compaction unavailable.")
		} else {
			before := len(*messages)
			*messages = r.built.Session.Compact(ctx, *messages)
			if len(*messages) < before {
				fmt.Printf("Compacted conversation: %d -> %d messages.\n", before, len(*messages))
			} else {
				fmt.Println("Compaction skipped.")
			}
		}
	}
}
