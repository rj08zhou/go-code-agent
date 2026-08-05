package agent

import (
	"os"
	"path/filepath"
	"unicode/utf8"

	"go-code-agent/internal/config"
)

// projectDocCandidates are checked in order under workdir; first hit wins.
// AGENT.md is this product's primary name; CLAUDE.md / AGENTS.md are common
// aliases used by other coding agents.
var projectDocCandidates = []string{
	"AGENT.md",
	"CLAUDE.md",
	"AGENTS.md",
}

// LoadProjectDocumentation reads the first present project instruction file
// from workdir (AGENT.md, then CLAUDE.md, then AGENTS.md). Missing files are
// not an error. Content is truncated to config.MaxProjectDocChars.
func LoadProjectDocumentation(workdir string) []byte {
	if workdir == "" {
		return nil
	}
	for _, name := range projectDocCandidates {
		path := filepath.Join(workdir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(data) == 0 {
			continue
		}
		return truncateUTF8(data, config.MaxProjectDocChars)
	}
	return nil
}

func truncateUTF8(data []byte, maxChars int) []byte {
	if maxChars <= 0 || utf8.RuneCount(data) <= maxChars {
		return data
	}
	runes := []rune(string(data))
	cut := string(runes[:maxChars])
	return []byte(cut + "\n\n…(truncated)")
}
