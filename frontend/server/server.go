// Package server renders a Markdown file (README_zh.md by default) as a
// styled, self-contained HTML page with a clickable table of contents
// (TOC) and serves it on an ephemeral localhost port. It is used two
// ways:
//
//  1. As a standalone "frontend project": `go run ./frontend` starts the
//     server, renders README_zh.md from the repo root, and opens it in
//     the OS default browser.
//  2. Imported by the go-code-agent REPL command `/readme`, which starts
//     the same viewer and opens the browser without leaving the agent.
//
// The markdown is converted with github.com/yuin/goldmark (a modern,
// well-maintained renderer). Headings get stable auto-generated `id`
// attributes (extension.AutoHeadingID), which the TOC links to, so
// clicking an entry scrolls the matching section into view. The file is
// read on every request, so refreshing the page reflects edits made to
// the source markdown.
package server

import (
	"bytes"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// DefaultFile is the markdown file rendered when none is supplied.
const DefaultFile = "README_zh.md"

// tocItem is one entry in the table of contents.
type tocItem struct {
	Level int
	ID    string
	Text  string
}

// pageData feeds the HTML template.
type pageData struct {
	Title  string
	Body   template.HTML
	TOC    []tocItem
	Source string
	Year   int
}

// pageTemplate is the page shell. Body is pre-rendered HTML, so we use
// template.HTML to avoid re-escaping it. The TOC is rendered as nested
// <ul> lists client-side from the .TOC slice.
var pageTemplate = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  html { scroll-behavior: smooth; }
  body {
    margin: 0; padding: 0;
    background: #f6f7f9;
    color: #1f2328;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto,
                 "Helvetica Neue", "PingFang SC", "Microsoft YaHei", sans-serif;
    line-height: 1.7;
  }
  /* Layout: fixed TOC on the left, content on the right. */
  .layout { display: flex; align-items: flex-start; }
  .toc {
    position: sticky; top: 0;
    flex: 0 0 260px;
    height: 100vh;
    overflow-y: auto;
    padding: 32px 18px 32px 24px;
    border-right: 1px solid #eaecef;
    background: #fbfcfd;
    font-size: 14px;
  }
  .toc h2 {
    font-size: 13px; text-transform: uppercase; letter-spacing: .05em;
    color: #57606a; margin: 0 0 12px;
  }
  .toc ul { list-style: none; margin: 0; padding: 0; }
  .toc li { margin: 2px 0; }
  .toc a {
    display: block; padding: 4px 8px; border-radius: 6px;
    color: #57606a; text-decoration: none; line-height: 1.45;
    border-left: 2px solid transparent;
  }
  .toc a:hover { background: #eef1f4; color: #1f2328; }
  .toc a.active { background: #e7f0fb; color: #0969da; border-left-color: #0969da; }
  .toc .lvl-3 a { padding-left: 22px; font-size: 13px; }
  .toc .lvl-4 a { padding-left: 34px; font-size: 13px; }
  .content {
    flex: 1 1 auto; min-width: 0;
    padding: 48px 40px 96px;
    background: #fff;
  }
  .content-inner { max-width: 820px; margin: 0 auto; }
  header.page-head {
    border-bottom: 1px solid #eaecef;
    padding-bottom: 16px; margin-bottom: 32px;
  }
  .badge {
    display: inline-block; font-size: 12px; letter-spacing: .04em;
    text-transform: uppercase; color: #57606a;
    background: #eef1f4; border-radius: 999px; padding: 2px 10px;
  }
  h1, h2, h3, h4 { line-height: 1.3; font-weight: 650; scroll-margin-top: 16px; }
  h1 { font-size: 30px; margin: .2em 0 .3em; }
  h2 { font-size: 23px; margin: 1.8em 0 .6em; padding-top: 12px; border-top: 1px solid #eaecef; }
  h3 { font-size: 19px; margin: 1.5em 0 .5em; }
  h4 { font-size: 16px; margin: 1.3em 0 .5em; }
  a { color: #0969da; text-decoration: none; }
  a:hover { text-decoration: underline; }
  /* Anchor link shown on hover for any heading with an id. */
  .content h1[id], .content h2[id], .content h3[id], .content h4[id] { position: relative; }
  .content h1[id]:hover::after, .content h2[id]:hover::after,
  .content h3[id]:hover::after, .content h4[id]:hover::after {
    content: "#"; margin-left: 8px; color: #c4ccd4; font-weight: 400;
  }
  code {
    background: #eff1f3; border-radius: 5px; padding: .15em .4em;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 85%;
  }
  pre {
    background: #0d1117; color: #e6edf3; padding: 16px 18px;
    border-radius: 10px; overflow: auto; line-height: 1.55;
  }
  pre code { background: transparent; color: inherit; padding: 0; font-size: 13.5px; }
  blockquote {
    margin: 1em 0; padding: .4em 1em; color: #57606a;
    border-left: 4px solid #d0d7de; background: #f6f8fa;
  }
  table { border-collapse: collapse; width: 100%; margin: 1em 0; }
  th, td { border: 1px solid #d0d7de; padding: 8px 12px; text-align: left; }
  th { background: #f6f8fa; }
  img { max-width: 100%; }
  hr { border: none; border-top: 1px solid #eaecef; margin: 2em 0; }
  footer { margin-top: 56px; padding-top: 16px; border-top: 1px solid #eaecef;
           color: #8b949e; font-size: 13px; }
  /* Mobile: collapse TOC into a top bar. */
  .toc-toggle { display: none; }
  @media (max-width: 860px) {
    .layout { display: block; }
    .toc {
      position: static; height: auto; width: 100%; flex: none;
      border-right: none; border-bottom: 1px solid #eaecef;
    }
    .toc.collapsed ul { display: none; }
    .toc-toggle {
      display: inline-block; margin-bottom: 10px; cursor: pointer;
      background: #eef1f4; border: none; border-radius: 6px;
      padding: 6px 12px; font: inherit; color: #1f2328;
    }
    .content { padding: 32px 18px 80px; }
  }
</style>
</head>
<body>
  <div class="layout">
    <nav class="toc" id="toc">
      <button class="toc-toggle" onclick="document.getElementById('toc').classList.toggle('collapsed')">目录 ☰</button>
      <h2>目录</h2>
      <ul>{{range .TOC}}<li class="lvl-{{.Level}}"><a href="#{{.ID}}" data-target="{{.ID}}">{{.Text}}</a></li>{{end}}</ul>
    </nav>
    <main class="content">
      <div class="content-inner">
        <header class="page-head">
          <span class="badge">go-code-agent · README</span>
          <h1>{{.Title}}</h1>
        </header>
        {{.Body}}
        <footer>Source: <code>{{.Source}}</code> · Rendered {{.Year}} · served by frontend/server</footer>
      </div>
    </main>
  </div>
<script>
  // Smooth-scroll + active highlight (scrollspy) for the TOC.
  (function () {
    var links = Array.prototype.slice.call(document.querySelectorAll('.toc a'));
    var map = {};
    links.forEach(function (a) {
      var id = a.getAttribute('data-target');
      var el = document.getElementById(id);
      if (el) { map[id] = a; a.addEventListener('click', function () {
        // close mobile TOC after navigating
        document.getElementById('toc').classList.add('collapsed');
      }); }
    });
    var ids = Object.keys(map);
    if (!ids.length || !('IntersectionObserver' in window)) return;
    var observer = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          links.forEach(function (l) { l.classList.remove('active'); });
          var a = map[e.target.id];
          if (a) a.classList.add('active');
        }
      });
    }, { rootMargin: '0px 0px -70% 0px', threshold: 0 });
    ids.forEach(function (id) {
      var el = document.getElementById(id);
      if (el) observer.observe(el);
    });
  })();
</script>
</body>
</html>`))

// md is the goldmark converter configured the same way for both the
// TOC walk and the body render, so heading ids always match.
func newMarkdown() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,      // tables, strikethrough, task lists, autolinks
			extension.Footnote, // [^1] footnotes
			extension.DefinitionList,
			extension.Typographer,
		),
	)
}

// Options configures a Viewer.
type Options struct {
	// File is the markdown file to render. Defaults to DefaultFile.
	// Ignored when Embedded is set.
	File string
	// Embedded, when non-nil, is rendered directly from memory instead
	// of reading a file from disk. This makes the viewer usable from
	// anywhere (e.g. a packed binary that embedded README_zh.md).
	Embedded []byte
	// Host to bind. Defaults to 127.0.0.1.
	Host string
	// Port to bind. 0 means "pick an ephemeral free port".
	Port int
	// OpenBrowser opens the OS default browser once listening.
	OpenBrowser bool
}

func (o *Options) norm() {
	if o.File == "" {
		o.File = DefaultFile
	}
	if o.Host == "" {
		o.Host = "127.0.0.1"
	}
}

// Viewer serves a rendered markdown page.
type Viewer struct {
	opts   Options
	ln     net.Listener
	server *http.Server
}

// New creates a Viewer. It does not start listening until Start is called.
//
// If opts.Embedded is set, the markdown is served directly from memory.
// Otherwise the file is resolved relative to workdir (walking up a few
// parent directories) so it works whether launched from the repo root,
// ./frontend, or inside the agent's workdir.
func New(workdir string, opts Options) (*Viewer, error) {
	opts.norm()

	if len(opts.Embedded) > 0 {
		v := &Viewer{opts: opts}
		return v, nil
	}

	path := resolveMarkdown(workdir, opts.File)
	if path == "" {
		return nil, fmt.Errorf("could not locate %q", opts.File)
	}
	opts.File = path

	v := &Viewer{opts: opts}
	return v, nil
}

// Addr returns the listener address; valid only after Start.
func (v *Viewer) Addr() string {
	if v.ln == nil {
		return ""
	}
	return "http://" + v.ln.Addr().String()
}

// Start begins serving in a background goroutine and opens the browser if
// requested. It returns as soon as the listener is bound (or an error).
func (v *Viewer) Start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", v.opts.Host, v.opts.Port))
	if err != nil {
		return fmt.Errorf("frontend: listen: %w", err)
	}
	v.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/", v.handleIndex)
	v.server = &http.Server{Handler: mux}

	go func() {
		_ = v.server.Serve(ln)
	}()

	if v.opts.OpenBrowser {
		_ = openBrowser(v.Addr())
	}
	return nil
}

// Stop shuts the server down.
func (v *Viewer) Stop() error {
	if v.server == nil {
		return nil
	}
	return v.server.Close()
}

func (v *Viewer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	var data []byte
	if len(v.opts.Embedded) > 0 {
		data = v.opts.Embedded
	} else {
		b, err := os.ReadFile(v.opts.File)
		if err != nil {
			http.Error(w, "read markdown: "+err.Error(), http.StatusInternalServerError)
			return
		}
		data = b
	}

	md := newMarkdown()

	// Parse once, then assign our own clean, unique ids to every heading
	// (goldmark's default auto-id slugger mishandles non-ASCII titles and
	// produces ugly slugs like "heading-12"). Using our ids guarantees the
	// TOC targets and the rendered anchors always match.
	doc := md.Parser().Parse(text.NewReader(data))
	assignHeadingIDs(doc, data)

	toc := buildTOC(doc, data)

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, data, doc); err != nil {
		http.Error(w, "render markdown: "+err.Error(), http.StatusInternalServerError)
		return
	}

	source := v.opts.File
	if len(v.opts.Embedded) > 0 {
		source = "README_zh.md (embedded)"
	}

	pd := pageData{
		Title:  titleFromMarkdown(data),
		Body:   template.HTML(buf.String()),
		TOC:    toc,
		Source: source,
		Year:   time.Now().Year(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, pd); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// assignHeadingIDs walks the AST and sets a clean, unique "id" attribute
// on every heading (h1..h6). We do this ourselves instead of relying on
// goldmark's auto-id because the built-in slugger produces poor slugs
// (e.g. "heading-12") and skips non-ASCII (Chinese) titles entirely,
// which would break TOC anchors. Using our ids for both the TOC and the
// rendered body guarantees the two always match.
func assignHeadingIDs(doc ast.Node, src []byte) {
	seen := map[string]int{}
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if h, ok := n.(*ast.Heading); ok {
			base := slugify(headingText(h, src))
			if base == "" {
				base = "section"
			}
			id := base
			if c, dup := seen[base]; dup {
				seen[base] = c + 1
				id = fmt.Sprintf("%s-%d", base, c+1)
			} else {
				seen[base] = 1
			}
			h.SetAttributeString("id", []byte(id))
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			walk(c)
		}
	}
	walk(doc)
}

// slugify turns heading text into a URL-safe fragment id. ASCII runs are
// lowercased and separated by hyphens; non-ASCII characters (e.g. CJK) are
// preserved as-is (modern browsers accept Unicode fragment identifiers, and
// Go's html/template will correctly escape them in href="#...").
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			b.WriteRune(unicode.ToLower(r))
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '.' || r == '·':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		case r > 127: // keep CJK and other non-ASCII verbatim
			b.WriteRune(r)
			prevDash = false
		default:
			// drop other punctuation
		}
	}
	out := strings.Trim(b.String(), "-")
	// collapse repeated dashes
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

// buildTOC walks the AST and collects heading nodes (h1..h4) into a flat
// list. The id comes from the node's "id" attribute so it is identical
// to the rendered anchor target.
func buildTOC(doc ast.Node, src []byte) []tocItem {
	var items []tocItem
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if h, ok := n.(*ast.Heading); ok && h.Level >= 1 && h.Level <= 4 {
			if id, ok := h.AttributeString("id"); ok {
				items = append(items, tocItem{
					Level: h.Level,
					ID:    string(id.([]byte)),
					Text:  headingText(n, src),
				})
			}
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			walk(c)
		}
	}
	walk(doc)
	return items
}

// headingText extracts the plain-text content of a heading node.
// It must be given the original source bytes because goldmark stores
// text as segments (offsets) into that source.
func headingText(n ast.Node, src []byte) string {
	var sb strings.Builder
	var collect func(c ast.Node)
	collect = func(c ast.Node) {
		switch t := c.(type) {
		case *ast.Text:
			sb.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				sb.WriteByte(' ')
			}
		case *ast.String:
			sb.Write(t.Value)
		case *ast.CodeSpan:
			// leave code spans visible in the TOC text
			for cc := c.FirstChild(); cc != nil; cc = cc.NextSibling() {
				collect(cc)
			}
		}
		for cc := c.FirstChild(); cc != nil; cc = cc.NextSibling() {
			collect(cc)
		}
	}
	collect(n)
	return strings.TrimSpace(sb.String())
}

// titleFromMarkdown extracts the first H1 ("# Title") for the <h1>/<title>.
func titleFromMarkdown(md []byte) string {
	for _, line := range strings.Split(string(md), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return "README"
}

// resolveMarkdown finds file starting at workdir and stepping up a few
// parent directories (so it works whether launched from repo root,
// ./frontend, or somewhere inside the tree).
func resolveMarkdown(workdir, file string) string {
	dir := workdir
	for i := 0; i < 6; i++ {
		if dir == "" || dir == "/" || dir == "." {
			break
		}
		cand := filepath.Join(dir, file)
		if _, err := os.Stat(cand); err == nil {
			if abs, err := filepath.Abs(cand); err == nil {
				return abs
			}
			return cand
		}
		dir = filepath.Dir(dir)
	}
	// Last resort: workdir + file (absolute).
	if abs, err := filepath.Abs(filepath.Join(workdir, file)); err == nil {
		return abs
	}
	return ""
}

// openBrowser opens url in the OS default browser, cross-platform.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, freebsd, etc.
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stderr = os.Stderr
	return cmd.Start()
}
