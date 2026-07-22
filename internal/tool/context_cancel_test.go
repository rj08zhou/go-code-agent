package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestBashHandlerRespectsCancelledContext(t *testing.T) {
	defs := BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	var bash *ToolDefinition
	for i := range defs {
		if defs[i].Name == "bash" {
			bash = &defs[i]
			break
		}
	}
	if bash == nil {
		t.Fatal("bash tool not found")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	started := time.Now()
	result := bash.Handler(&ToolScope{
		Context:    ctx,
		Workdir:    t.TempDir(),
		CanExecute: true,
	}, json.RawMessage(`{"command":"sleep 30"}`))
	elapsed := time.Since(started)

	if elapsed > 3*time.Second {
		t.Fatalf("bash ignored cancellation: took %v, result=%+v", elapsed, result)
	}
}
