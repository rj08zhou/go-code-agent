---
name: judge
description: |
  LLM-as-Judge verification for agent outputs.
  Use when you need to verify agent work quality, validate task completion,
  or implement quality gates. Keywords: verify, validate, judge, score, evaluate.
---

# Judge

LLM-based verification layer that evaluates whether the agent's actions and
results truly match the user's intent.

> **Note on scope**: `judge` is a **code-internal capability** of
> `go-code-agent` (implemented in `internal/agent/judge.go`, wired into the
> agent loop at `loop.go`). This skill file is **documentation only** — it is
> loaded on demand via the `load_skill` tool to give the agent methodological
> guidance. It does **not** participate in the automatic verification flow and
> there is no separate `skills/judge` Go package or `WithSkills(...)` API. The
> judge runs automatically whenever it is enabled (see below); you never call
> it from this skill.

## What the Judge Does

After the agent finishes a task (a turn where it declared the task complete
and used at least one tool), the loop calls `Judge.Verify(...)`:

1. Collects the recent conversation tail and the round's tool results.
2. Sends them to a **separate (usually cheaper) LLM** via the judge provider.
3. Parses a structured verdict:
   - `approved`: bool — overall pass/fail
   - `score`: int (1–10) — quality score
   - `issues`: []string — concrete problems
   - `suggestions`: []string — how to fix
   - `should_retry`: bool — whether to force another round

If `score < JUDGE_MIN_SCORE`, the verdict is forced to `should_retry = true`
(and `approved = false`), and the loop injects a `<verification-failed>`
feedback block back into the conversation so the agent can self-correct. On any
internal error (bad model, empty response, parse failure) the judge falls back
to a **permissive verdict** (`approved = true`) so it never blocks progress.

## Configuration

The judge is configured **entirely through `JUDGE_*` environment variables**
(no CLI flags, no YAML, no per-skill config). Parsed centrally in
`infra/config.go`:

```bash
JUDGE_ENABLED=1                              # turn the judge on (1 | true | yes | on)
JUDGE_MODEL=claude-haiku-4.5                 # empty = reuse the main agent model
JUDGE_MIN_SCORE=7                            # below this, should_retry is forced
JUDGE_PROVIDER=openai                        # optional: explicit backend SDK
JUDGE_API_KEY=<key>                          # optional: judge-only API key
JUDGE_BASE_URL=https://api.deepseek.com      # optional: judge-only endpoint
```

Notes:
- `JUDGE_MODEL` empty → the judge reuses the **main model** as a fallback.
  Generally a *cheaper* model is used to save cost, but that is a deployment
  choice, not a code requirement.
- `JUDGE_PROVIDER` / `JUDGE_API_KEY` / `JUDGE_BASE_URL` let the judge live
  behind a **separate endpoint** from the main agent; if unset, it uses the
  main model's provider.
- `JUDGE_MIN_SCORE` invalid/empty → falls back to the infra default (7).

## Prompt Templates

The judge renders prompt templates loaded from the project's `prompts/`
directory (same dir as the agent's other prompts). Customize them for your
domain:

- `prompts/judge_system.md` — main verification prompt (with scoring rubric)
- `prompts/judge_critical.md` — injected when the score is critically low,
  forcing a strategy change

## Design Notes

- **Cost control**: uses a separate (cheaper) LLM call with compact context
  (last ~12 messages by default).
- **Non-blocking**: on internal errors, returns a permissive default
  (`approved = true`), never stalls the agent.
- **Configurable strictness**: raise/lower `JUDGE_MIN_SCORE` to set your
  quality bar.
- **Domain adaptable**: override the prompt templates above for different
  domains (code, writing, analysis).
