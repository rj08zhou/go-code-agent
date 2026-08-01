module go-code-agent

go 1.25.3

require (
	github.com/anthropics/anthropic-sdk-go v1.41.0
	github.com/chzyer/readline v1.5.2-0.20250620033330-9dfc369f8652
	github.com/openai/openai-go v1.12.0
	golang.org/x/net v0.57.0
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.0-20260427160145-3afa6683f8b2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Wide-character (CJK) backspace fix: upstream PR chzyer/readline#250 (fixes #184)
// is unmerged; this fork tag is upstream main (9dfc369) plus only that 4-line patch
// (verified by full module diff; content is hash-locked via go.sum).
// Drop this replace once upstream merges the fix.
replace github.com/chzyer/readline => github.com/rkonfj/readline v1.5.2
