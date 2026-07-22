package web

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var skipTextAtoms = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Svg: true, atom.Head: true,
	// Chrome/boilerplate regions: navigation, footers, sidebars, and forms
	// are almost never the answer and drown the real content in link/CTA
	// noise (e.g. "Join our Discord", newsletter promos).
	atom.Nav: true, atom.Footer: true, atom.Aside: true, atom.Form: true,
}

// ARIA landmarks that are site chrome rather than article body.
var skipARIARoles = map[string]bool{
	"navigation": true, "banner": true, "contentinfo": true,
	"complementary": true, "search": true,
}

// Matches common chrome class/id tokens used by sites that skip semantic tags.
var chromeClassOrID = regexp.MustCompile(
	`(^|[-_\s])(nav|navbar|menu|footer|sidebar|cookie|newsletter|promo|banner|social|share|subscribe)([-_\s]|$)`)

var blockAtoms = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Tr: true, atom.Table: true, atom.Section: true, atom.Article: true,
	atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Ul: true, atom.Ol: true,
}

func HTMLToText(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return stripTagsFallback(raw)
	}
	// Prefer the page's main content region when it marks one; this drops
	// site chrome (headers, promos) that lives as siblings of <main>/<article>.
	root := doc
	if main := findMainContent(doc); main != nil {
		root = main
	}
	var b strings.Builder
	var walk func(n *html.Node, skip bool)
	walk = func(n *html.Node, skip bool) {
		if n.Type == html.ElementNode {
			if skipTextAtoms[n.DataAtom] || isChromeElement(n) {
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
	walk(root, false)
	return collapseBlankLines(b.String())
}

// isChromeElement detects non-semantic site chrome via ARIA role or
// common class/id naming conventions.
func isChromeElement(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if role := strings.ToLower(attr(n, "role")); skipARIARoles[role] {
		return true
	}
	id := strings.ToLower(attr(n, "id"))
	class := strings.ToLower(attr(n, "class"))
	if id != "" && chromeClassOrID.MatchString(id) {
		return true
	}
	if class != "" && chromeClassOrID.MatchString(class) {
		return true
	}
	return false
}

// isBoilerplateText reports whether extracted text looks like site chrome /
// promo noise rather than an article (many short CTA lines, no substantial
// paragraphs). Callers should fall back to title/meta in that case.
// A single short paragraph is NOT treated as boilerplate — it may be the
// entire page content.
func isBoilerplateText(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return true
	}
	// One contiguous snippet is usually real content, even if short.
	if len(lines) == 1 {
		return false
	}
	substantial := 0
	for _, l := range lines {
		if len(l) >= 40 {
			substantial++
		}
	}
	// Lots of short CTA/link lines and almost no paragraph-length text.
	if len(lines) >= 3 && substantial == 0 {
		return true
	}
	if len(s) < 500 && float64(substantial)/float64(len(lines)) < 0.15 {
		return true
	}
	return false
}

// findMainContent returns the first <main> element, or failing that the
// largest <article> element, so extraction can focus on the real content.
// Returns nil when the page has neither (fall back to the whole document).
func findMainContent(doc *html.Node) *html.Node {
	var main *html.Node
	var bestArticle *html.Node
	bestArticleLen := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Main:
				if main == nil {
					main = n
				}
			case atom.Article:
				if l := textLen(n); l > bestArticleLen {
					bestArticleLen = l
					bestArticle = n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if main != nil {
		return main
	}
	return bestArticle
}

// textLen returns the approximate length of visible text under n, ignoring
// skipped regions, used to pick the most substantial <article>.
func textLen(n *html.Node) int {
	total := 0
	var walk func(n *html.Node, skip bool)
	walk = func(n *html.Node, skip bool) {
		if n.Type == html.ElementNode && skipTextAtoms[n.DataAtom] {
			skip = true
		}
		if n.Type == html.TextNode && !skip {
			total += len(strings.TrimSpace(n.Data))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, skip)
		}
	}
	walk(n, false)
	return total
}

// HTMLMetaFallback extracts a minimal, human-useful summary (title +
// description) from pages whose visible body is rendered client-side and thus
// yields no static text. Returns "" when nothing useful is found.
func HTMLMetaFallback(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	var title, desc string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Title:
				if title == "" {
					title = strings.TrimSpace(textContent(n))
				}
			case atom.Meta:
				name := strings.ToLower(attr(n, "name"))
				prop := strings.ToLower(attr(n, "property"))
				if desc == "" && (name == "description" || prop == "og:description") {
					desc = strings.TrimSpace(attr(n, "content"))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var parts []string
	if title != "" {
		parts = append(parts, title)
	}
	if desc != "" {
		parts = append(parts, desc)
	}
	return strings.Join(parts, "\n")
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
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
