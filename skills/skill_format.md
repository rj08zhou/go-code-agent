# Skill Format

Skills are Markdown files placed in the `skills/` directory.
They define reusable knowledge that the agent can load on demand.

## Structure

```markdown
# {skill-name}
Triggers: keyword1, keyword2, keyword3

{content in any format}
```

## Example

```markdown
# go-testing
Triggers: test, testing, unittest, go test

## Go Testing Conventions
- Use `go test ./...` for full test suite
- Name test files `*_test.go`
- Use table-driven tests where possible
```
