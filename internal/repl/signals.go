package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/chzyer/readline"
)

// turnCanceller tracks the active agent turn so Ctrl-C can interrupt it
// without tearing down the whole REPL session.
type turnCanceller struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (t *turnCanceller) arm(cancel context.CancelFunc) {
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
}

func (t *turnCanceller) disarm() {
	t.mu.Lock()
	t.cancel = nil
	t.mu.Unlock()
}

func (t *turnCanceller) interrupt() bool {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (r *Loop) Run() {
	r.next = nil

	messages, restored, err := r.built.Session.HistStore.LoadRuntime(r.built.Session.SysPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load history: %v\n", err)
		return
	}
	r.printBanner(restored)

	sessionCtx, cancelSession := context.WithCancel(r.rtCtx)
	defer cancelSession()

	var turns turnCanceller
	defer r.watchSignals(sessionCtx, cancelSession, &turns)()

	for sessionCtx.Err() == nil {
		line, action := r.readPrompt()
		switch action {
		case promptContinue:
			continue
		case promptExit:
			return
		}

		if strings.HasPrefix(line, "/") {
			if r.handleCommand(sessionCtx, line, &messages) {
				return
			}
			continue
		}

		messages = r.runUserTurn(sessionCtx, &turns, messages, line)
		if sessionCtx.Err() != nil {
			return
		}
		fmt.Println()
	}
}

type promptAction int

const (
	promptRun promptAction = iota
	promptContinue
	promptExit
)

func (r *Loop) readPrompt() (string, promptAction) {
	line, err := r.readFn()
	if err != nil {
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			return "", promptContinue
		case errors.Is(err, io.EOF):
			fmt.Println("Goodbye!")
		default:
			fmt.Fprintf(os.Stderr, "Input error: %v\n", err)
		}
		return "", promptExit
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", promptContinue
	}
	return line, promptRun
}

func (r *Loop) watchSignals(sessionCtx context.Context, cancelSession context.CancelFunc, turns *turnCanceller) func() {
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sessionCtx.Done():
				return
			case sig := <-sigCh:
				if sig == syscall.SIGTERM {
					fmt.Fprintln(os.Stderr, "\nShutting down...")
					cancelSession()
					return
				}
				if turns.interrupt() {
					fmt.Fprintln(os.Stderr, "\nInterrupting current turn...")
				}
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}
