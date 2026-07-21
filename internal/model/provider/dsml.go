package provider

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"

	"go-code-agent-refactor/internal/llm"
)

const (
	dsmlToolCallsOpen  = "<｜DSML｜tool_calls>"
	dsmlToolCallsClose = "</｜DSML｜tool_calls>"
)

var (
	dsmlInvokeRE = regexp.MustCompile(`(?s)<｜DSML｜invoke\s+name\s*=\s*"([^"]+)"\s*>(.*?)</｜DSML｜invoke\s*>`)
	dsmlParamRE  = regexp.MustCompile(`(?s)<｜DSML｜parameter\s+name\s*=\s*"([^"]+)"(?:\s+string\s*=\s*"[^"]*")?\s*>(.*?)</｜DSML｜parameter\s*>`)
)

// parseDSMLToolCalls converts DeepSeek's content-embedded DSML tool calls
// into the provider-neutral tool-call representation. The parser only
// claims a response when at least one complete invoke block is present;
// malformed or partial content is left untouched for safe handling upstream.
func parseDSMLToolCalls(content string) (clean string, calls []llm.ToolCall, parsed bool) {
	start := strings.Index(content, dsmlToolCallsOpen)
	if start < 0 {
		return content, nil, false
	}
	end := strings.Index(content[start+len(dsmlToolCallsOpen):], dsmlToolCallsClose)
	if end < 0 {
		return content, nil, false
	}
	end += start + len(dsmlToolCallsOpen)
	body := content[start+len(dsmlToolCallsOpen) : end]

	for i, invoke := range dsmlInvokeRE.FindAllStringSubmatch(body, -1) {
		params := make(map[string]string)
		for _, param := range dsmlParamRE.FindAllStringSubmatch(invoke[2], -1) {
			params[html.UnescapeString(strings.TrimSpace(param[1]))] = html.UnescapeString(param[2])
		}
		args, err := json.Marshal(params)
		if err != nil {
			continue
		}
		calls = append(calls, llm.ToolCall{
			ID:        "dsml_call_" + formatDSMLIndex(i),
			Name:      html.UnescapeString(strings.TrimSpace(invoke[1])),
			Arguments: string(args),
		})
	}
	if len(calls) == 0 {
		return content, nil, false
	}

	clean = content[:start] + content[end+len(dsmlToolCallsClose):]
	return strings.TrimSpace(clean), calls, true
}

func formatDSMLIndex(i int) string {
	return strconv.Itoa(i)
}
