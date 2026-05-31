---
name: judge
description: |
  LLM-as-Judge verification for agent outputs.
  Use when you need to verify agent work quality, validate task completion,
  or implement quality gates. Keywords: verify, validate, judge, score, evaluate.
type: code
---

# Judge Skill

LLM-based verification layer that evaluates whether the agent's actions and results truly match the user's intent.

## What This Skill Does

After significant events (task completion, post-reflection), the Judge:

1. Collects recent conversation and tool results
2. Sends them to a SEPARATE (usually cheaper) LLM
3. Gets a structured verdict with:
   - `approved`: bool - overall pass/fail
   - `score`: int (1-10) - quality score
   - `issues`: []string - concrete problems
   - `suggestions`: []string - how to fix
   - `should_retry`: bool - whether to force another round

## Configuration

```yaml
skills:
  - name: judge
    enabled: true
    config:
      min_score: 7          # below this, should_retry is forced
      model: ""             # empty = reuse main model; use "claude-haiku-4.5" for cost savings
      max_history: 12       # max messages included in judge prompt
      max_retry_injects: 2  # cap verification-failed injections per loop
```

## Prompt Templates

This skill uses templates in `prompts/judge_system.md`. Customize them for your domain:

- `judge_system.md` - Main verification prompt (with scoring rubric)
- `judge_critical.md` - Injected when score <= 3 (forces strategy change)

## Usage Example

```go
import "github.com/yourname/go-code-agent/skills/judge"

// In your agent setup:
judgeSkill := judge.New(judge.Config{
    MinScore: 7,
    Model:    "claude-haiku-4.5",
})

agent := NewAgent().
    WithSkills(judgeSkill)
```

## Design Notes

- **Cost control**: Uses a separate (cheaper) LLM call with compact context
- **Non-blocking**: On internal errors, returns permissive default (approved=true)
- **Configurable strictness**: Adjust `min_score` to your quality bar
- **Domain adaptable**: Override prompt templates for different domains (code, writing, analysis)
