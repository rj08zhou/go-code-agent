// Package prompts embeds system-level prompt templates into the
// compiled binary so the agent can reference them regardless of the
// working directory it is launched from.
package prompts

import "embed"

// FS contains every .md file in this directory, accessible as
// prompts.FS.Open("system.md") etc.
//
//go:embed *.md
var FS embed.FS
