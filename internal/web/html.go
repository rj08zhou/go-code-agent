package web

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipTextAtoms lists elements whose text content is never part of a
// page's readable body (script/style payloads, embedded SVG/noscript
// fallbacks) - text inside them is dropped entirely rather than
// concatenated into the extracted output.
var skipTextAtoms = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Noscript: true,
	atom.Svg:      true,
	atom.Head:     true,
	atom.Title:    false, // kept: title text is genuinely useful context
}

// blockAtoms are elements that should force a line break before/after
// their text content, so extracted text roughly preserves paragraph /
// heading / list-item structure instead of becoming one run-on line.
var blockAtoms = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Tr: true, atom.Table: true, atom.Section: true, atom.Article: true,
	atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Ul: true, atom.Ol: true,
}

// HTMLToText extracts a page's readable text content: it walks the
// parsed DOM depth-first, skips script/style/svg/head subtrees
// entirely, and inserts line breaks at block-level element boundaries
// so the result is reasonably paragraph-shaped rather than one
// enormous whitespace-collapsed line. This is intentionally a plain
// DOM walk rather than a "readability" main-content heuristic (which
// would need heavier scoring logic) - good enough for an LLM to read,
// not meant to rival browser reader-mode extraction quality.
func HTMLToText(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		// Malformed HTML: fall back to a crude tag-stripper rather than
		// returning nothing - a best-effort result beats an error for
		// what is, after all, best-effort text extraction.
		return stripTagsFallback(raw)
	}

	var b strings.Builder
	var walk func(n *html.Node, skip bool)
	walk = func(n *html.Node, skip bool) {
		if n.Type == html.ElementNode {
			if skipTextAtoms[n.DataAtom] {
				skip = true
			}
			if blockAtoms[n.DataAtom] {
				b.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode && !skip {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, skip)
		}
		if n.Type == html.ElementNode && blockAtoms[n.DataAtom] {
			b.WriteByte('\n')
		}
	}
	walk(doc, false)

	return collapseBlankLines(b.String())
}

// collapseBlankLines trims trailing whitespace on each line and
// collapses runs of 3+ blank lines into at most one, which is what
// the block-boundary '\n' insertions in HTMLToText tend to produce
// for deeply nested markup.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// stripTagsFallback is the zero-dependency, best-effort path used
// when html.Parse itself fails (rare - it is very tolerant of broken
// markup) or for plain-text-ish inputs; it just removes anything
// between angle brackets without attempting real DOM structure.
func stripTagsFallback(raw string) string {
	var b strings.Builder
	inTag := false
	for _, r := range raw {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return collapseBlankLines(b.String())
}
