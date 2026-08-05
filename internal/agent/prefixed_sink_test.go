package agent

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPrefixedSinkPrintsSubPrefixOnce(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	s := newPrefixedSink("explore")
	s.OnTextDelta("Code")
	s.OnTextDelta("x")
	s.OnTextDelta(" CLI")
	s.OnDone()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if n := strings.Count(out, "[sub]"); n != 1 {
		t.Fatalf("[sub] count = %d, want 1; out=%q", n, out)
	}
	if !strings.Contains(out, "Codex CLI") {
		t.Fatalf("missing streamed text, out=%q", out)
	}
}

func TestPrefixedSinkLeadHasNoSubPrefix(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	s := newPrefixedSink("lead")
	s.OnTextDelta("hello")
	s.OnTextDelta(" world")
	s.OnDone()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()
	if strings.Contains(out, "[sub]") {
		t.Fatalf("lead sink should not print [sub], out=%q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("missing streamed text, out=%q", out)
	}
}

func TestPrefixedSinkReasoningDimPrefixThenAnswer(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w

	s := newPrefixedSink("lead")
	s.OnReasoningDelta("plan ")
	s.OnReasoningDelta("steps")
	s.OnTextDelta("done")
	s.OnDone()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()
	if n := strings.Count(out, "[thinking]"); n != 1 {
		t.Fatalf("[thinking] count = %d, want 1; out=%q", n, out)
	}
	if !strings.Contains(out, "plan steps") {
		t.Fatalf("missing thinking text, out=%q", out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("missing answer text, out=%q", out)
	}
}
