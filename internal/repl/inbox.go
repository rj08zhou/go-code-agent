package repl

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/utils"
)

func (r *Loop) drainBackground() {
	for _, n := range r.built.Team.BG.Notifications() {
		fmt.Fprintf(os.Stderr, "[bg] job %s: %s\n", n["id"], n["status"])
	}
	for _, j := range r.built.Team.BG.Drain() {
		fmt.Fprintln(os.Stderr, "[bg] completed:", j)
	}
}

func (r *Loop) checkInbox(messages *[]llm.Message) {
	mb := r.built.Team.Bus
	if mb == nil {
		return
	}
	msgs := mb.ReadInbox(r.built.Session.AgentID)
	if len(msgs) == 0 {
		return
	}
	for _, m := range msgs {
		from, _ := m["from"].(string)
		ct, _ := m["content"].(string)
		msgType, _ := m["type"].(string)
		text := fmt.Sprintf("[From %s] %s", from, ct)
		if msgType == "shutdown_request" {
			text = fmt.Sprintf("[Shutdown request from %s]", from)
		}
		*messages = append(*messages, llm.SystemMessage(text))
	}
}

func (r *Loop) readInbox(agentID string) string {
	if r.built.Team.Bus == nil {
		return "Inbox is unavailable."
	}
	return formatInboxMessages(r.built.Team.Bus.ReadInbox(agentID))
}

func formatInboxMessages(messages []map[string]any) string {
	if len(messages) == 0 {
		return "Inbox is empty."
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Inbox (%d):", len(messages))
	for i, message := range messages {
		from, _ := message["from"].(string)
		msgType, _ := message["type"].(string)
		content, _ := message["content"].(string)
		if from == "" {
			from = "unknown"
		}
		if msgType == "" {
			msgType = "message"
		}
		fmt.Fprintf(&out, "\n  %d. [%s] from %s", i+1, inboxText(msgType, 80), inboxText(from, 80))
		if requestID, ok := message["request_id"].(string); ok && requestID != "" {
			fmt.Fprintf(&out, " request_id=%s", inboxText(requestID, 120))
		}
		if content != "" {
			fmt.Fprintf(&out, ": %s", inboxText(content, 500))
		}
	}
	return out.String()
}

func inboxText(text string, limit int) string {
	text = utils.Truncate(strings.TrimSpace(text), limit)
	quoted := strconv.QuoteToGraphic(text)
	return strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
}

func (r *Loop) handleSearchCommand(ctx context.Context, parts []string) {
	if len(parts) < 2 {
		fmt.Println("Usage: /search <query>")
		return
	}
	fmt.Println(r.runSearch(ctx, strings.Join(parts[1:], " ")))
}

func (r *Loop) runSearch(ctx context.Context, query string) string {
	if r.built.Runtime.Web == nil {
		return "Web search is unavailable."
	}
	output, err := r.built.Runtime.Web.Search(ctx, query)
	if err != nil {
		return fmt.Sprintf("Search failed: %v", err)
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return "No search results."
	}
	return output
}
