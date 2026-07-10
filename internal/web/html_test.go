package web

import (
	"strings"
	"testing"
)

func TestHTMLToText_StripsScriptAndStyle(t *testing.T) {
	in := `<html><head><title>T</title><style>.a{color:red}</style></head>
<body><script>alert(1)</script><p>Hello world</p></body></html>`
	out := HTMLToText(in)
	if strings.Contains(out, "alert(1)") {
		t.Errorf("script content leaked into extracted text: %q", out)
	}
	if strings.Contains(out, "color:red") {
		t.Errorf("style content leaked into extracted text: %q", out)
	}
	if !strings.Contains(out, "Hello world") {
		t.Errorf("expected body text preserved, got: %q", out)
	}
}

func TestHTMLToText_BasicStructure(t *testing.T) {
	in := `<html><body><h1>Title</h1><p>Para one.</p><p>Para two.</p></body></html>`
	out := HTMLToText(in)
	if !strings.Contains(out, "Title") || !strings.Contains(out, "Para one.") || !strings.Contains(out, "Para two.") {
		t.Errorf("expected all text segments present, got: %q", out)
	}
}

func TestHTMLToText_MalformedFallsBack(t *testing.T) {
	// Not well-formed, but html.Parse is tolerant of almost everything;
	// this mainly exercises that we don't panic and return something.
	in := `<div><p>Unclosed tags <span>nested`
	out := HTMLToText(in)
	if !strings.Contains(out, "Unclosed tags") {
		t.Errorf("expected text extracted despite malformed markup, got: %q", out)
	}
}

func TestStripTagsFallback(t *testing.T) {
	out := stripTagsFallback("<b>bold</b> and <i>italic</i> text")
	if out != "bold and italic text" {
		t.Errorf("stripTagsFallback = %q, want %q", out, "bold and italic text")
	}
}

func TestCollapseBlankLines(t *testing.T) {
	in := "a\n\n\n\n\nb\n\nc"
	out := collapseBlankLines(in)
	want := "a\n\nb\n\nc"
	if out != want {
		t.Errorf("collapseBlankLines(%q) = %q, want %q", in, out, want)
	}
}
