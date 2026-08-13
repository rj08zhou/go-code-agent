package repl

import (
	"context"
	"fmt"
	"os"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"
)

func (r *Loop) runUserTurn(sessionCtx context.Context, turns *turnCanceller, messages []llm.Message, line string) []llm.Message {
	histStore := r.built.Session.HistStore
	r.drainBackground()
	r.checkInbox(&messages)

	userHistorySaved := true
	if err := persistUserHistory(histStore, line); err != nil {
		fmt.Fprintln(os.Stderr, formatHistorySaveWarning(err))
		userHistorySaved = false
	}
	messages = append(messages, llm.UserMessage(line))

	before := len(messages)
	if !userHistorySaved {
		// Compaction must not treat this unsaved user message as covered.
		before--
	}

	turnCtx, cancelTurn := context.WithCancel(sessionCtx)
	turns.arm(cancelTurn)
	outcome := r.built.Runtime.Runner.Run(
		agent.WithPersistedBoundary(turnCtx, &before),
		messages,
		gateway.NewTraceID(),
	)
	turns.disarm()
	cancelTurn()

	// Incomplete streamed text is not part of outcome.Messages, so a
	// cancelled partial response is not persisted.
	messages = outcome.Messages
	if before > len(messages) {
		before = len(messages)
	}
	if userHistorySaved {
		if err := persistAgentHistory(histStore, messages[before:]); err != nil {
			fmt.Fprintln(os.Stderr, formatHistorySaveWarning(err))
		}
	}
	if notice := formatTurnOutcomeError(outcome.Error, sessionCtx.Err()); notice != "" {
		fmt.Fprintln(os.Stderr, notice)
	}
	return messages
}
