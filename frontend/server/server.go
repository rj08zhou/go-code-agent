// Package server renders a Markdown file (README_zh.md by default) as a
// styled, self-contained HTML page and serves it on an ephemeral
// localhost port. It is used two ways:
//
//  1. As a standalone "frontend project": `go run ./frontend` starts the
//     server, renders README_zh.md from the repo root, and opens it in
//     the OS default browser.
//  2. Imported by the go-code-agent REPL command `/readme`, which starts
//     the same viewer and opens the browser without leaving the agent.
//
// The markdown is converted with github.com/russross/blackfriday/v2
// (already a dependency of this module, so no network fetch is needed).
// The file is read on every request, so refreshing the page reflects
// edits made to the source markdown.
package server

import (
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

	"github.com/russross/blackfriday/v2"
)

// DefaultFile is the markdown file rendered when none is supplied.
const DefaultFile = "README_zh.md"

// pageData feeds the HTML template.
type pageData struct {
	Title  string
	Body   template.HTML
	Source string
	Year   int
}

// pageTemplate is the page shell. Body is pre-rendered HTML, so we use
// template.HTML to avoid re-escaping it.
var pageTemplate = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 0;
    background: #f6f7f9;
    color: #1f2328;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto,
                 "Helvetica Neue", "PingFang SC", "Microsoft YaHei", sans-serif;
    line-height: 1.7;
  }
  .wrap {
    max-width: 880px;
    margin: 0 auto;
    padding: 48px 24px 96px;
    background: #fff;
    min-height: 100vh;
    box-shadow: 0 0 0 1px rgba(0,0,0,.04);
  }
  header.page-head {
    border-bottom: 1px solid #eaecef;
    padding-bottom: 16px; margin-bottom: 32px;
  }
  .badge {
    display: inline-block; font-size: 12px; letter-spacing: .04em;
    text-transform: uppercase; color: #57606a;
    background: #eef1f4; border-radius: 999px; padding: 2px 10px;
  }
  h1, h2, h3, h4 { line-height: 1.3; font-weight: 650; }
  h1 { font-size: 30px; margin: .2em 0 .3em; }
  h2 { font-size: 23px; margin: 1.8em 0 .6em; padding-top: 12px; border-top: 1px solid #eaecef; }
  h3 { font-size: 19px; margin: 1.5em 0 .5em; }
  a { color: #0969da; text-decoration: none; }
  a:hover { text-decoration: underline; }
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
</style>
</head>
<body>
  <div class="wrap">
    <header class="page-head">
      <span class="badge">go-code-agent · README</span>
      <h1>{{.Title}}</h1>
    </header>
    {{.Body}}
    <footer>Source: <code>{{.Source}}</code> · Rendered {{.Year}} · served by frontend/server</footer>
  </div>
</body>
</html>`))

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

	renderer := blackfriday.NewHTMLRenderer(blackfriday.HTMLRendererParameters{
		Flags: blackfriday.CommonHTMLFlags,
	})
	html := blackfriday.Run(data, blackfriday.WithRenderer(renderer))

	source := v.opts.File
	if len(v.opts.Embedded) > 0 {
		source = "README_zh.md (embedded)"
	}

	pd := pageData{
		Title:  titleFromMarkdown(data),
		Body:   template.HTML(html),
		Source: source,
		Year:   time.Now().Year(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, pd); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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
