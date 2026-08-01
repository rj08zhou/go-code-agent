package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/chzyer/readline"

	"go-code-agent/internal/application"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/model/provider"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/store"
	"go-code-agent/internal/utils"
)

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), `
Model / LLM environment variables (export before running):

  # Anthropic (default when MODEL_ID is claude-*)
  export ANTHROPIC_API_KEY="sk-ant-..."
  export ANTHROPIC_BASE_URL=""          # optional gateway/proxy

  # OpenAI or OpenAI-compatible (Zhipu, DeepSeek, Ollama, ...)
  export OPENAI_API_KEY="sk-..."
  export OPENAI_BASE_URL=""            # e.g. https://api.openai.com/v1
  export MODEL_ID="gpt-4o"             # default: claude-opus-4.7

  # Force provider regardless of MODEL_ID prefix
  export LLM_PROVIDER="anthropic"      # openai | anthropic

  # Optional: LLM-as-Judge
  export JUDGE_ENABLED=1
  export JUDGE_MODEL="claude-haiku-4.5"
  export JUDGE_MIN_SCORE=7
  export JUDGE_PROVIDER=""             # optional; openai | anthropic
  export JUDGE_API_KEY=""              # optional; defaults to main key
  export JUDGE_BASE_URL=""             # optional; defaults to main URL

  # Optional: rate limits / context
  export LLM_MAX_QPS=4.0
  export LLM_MAX_BURST=8
  export LLM_MAX_CONCURRENCY=4
  export CONTEXT_WINDOW_TOKENS=0       # 0 = model default

At least one of ANTHROPIC_API_KEY or OPENAI_API_KEY is required.
`)
	}

	workdir := flag.String("workdir", "", "Working directory (default: current directory)")
	dataDir := flag.String("data-dir", "", "State directory (default: ~/.config/go-code-agent)")
	sessionID := flag.String("session", "", "Resume a specific session ID")
	newSession := flag.Bool("new-session", false, "Start a fresh session")
	human := flag.Bool("human", false, "Use manual approval mode")
	humanMode := flag.String("human-mode", "", "Advanced compatibility override: interactive|safe-only|auto-approve|auto-reject|notify-only")
	flag.Parse()
	if *sessionID != "" && *newSession {
		fmt.Fprintln(os.Stderr, "Invalid options: --session and --new-session cannot be used together.")
		return 2
	}
	if *sessionID != "" {
		if err := session.ValidateSessionID(*sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid --session value: %v\n", err)
			return 2
		}
	}

	wd := *workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	cfgDir := *dataDir
	if cfgDir == "" {
		cfgDir = os.Getenv("HOME") + "/.config"
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			cfgDir = d
		}
	}

	app, err := application.New(cfgDir, wd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize: %v\n", err)
		if errors.Is(err, provider.ErrNoProvider) {
			fmt.Fprint(os.Stderr, `
No usable LLM provider. Set at least one API key, e.g.:

  export ANTHROPIC_API_KEY="sk-ant-..."   # for claude-* models (default)
  export OPENAI_API_KEY="sk-..."          # for gpt-* / OpenAI-compatible models

If a key is already set, make sure it matches MODEL_ID / LLM_PROVIDER.
Run with --help for the full list of environment variables.
`)
		}
		return 1
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to shut down session: %v\n", err)
			exitCode = 1
		}
	}()

	// Route non-event runtime diagnostics to a private file instead of
	// stdout so they cannot interleave with streamed model output. Structured
	// audit and usage events remain authoritative in the session log.
	logFile := filepath.Join(app.DataDir(), "terminal", "agent.log")
	if lf, err := store.OpenPrivateAppend(logFile); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] file logging disabled: %v\n", err)
	} else {
		defer lf.Close()
		logging.SetDefault(logging.New(lf, logging.LevelInfo, false))
	}

	replPrompt := utils.Blue + "> " + utils.Reset

	// Pre-create the history file with owner-only permissions (0700 dir,
	// 0600 file); readline itself would create it world-readable (0666).
	histFile := filepath.Join(app.DataDir(), "terminal", "history")
	if f, err := store.OpenPrivateAppend(histFile); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] repl history disabled: %v\n", err)
		histFile = "" // fall back to in-memory history
	} else {
		f.Close()
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:                 replPrompt,
		HistoryFile:            histFile,
		HistorySearchFold:      true, // case-insensitive Ctrl-R
		DisableAutoSaveHistory: true, // approval y/n/m answers must not pollute history
		AutoComplete:           newCompleter(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize terminal: %v\n", err)
		return 1
	}
	defer rl.Close()

	var terminalMu sync.Mutex
	readTerminal := func(prompt string, saveHistory bool) (string, error) {
		terminalMu.Lock()
		defer terminalMu.Unlock()
		rl.SetPrompt(prompt)
		defer rl.SetPrompt(replPrompt)
		line, err := rl.Readline()
		if saveHistory && err == nil && strings.TrimSpace(line) != "" {
			_ = rl.SaveHistory(line) // write error only affects persistence
		}
		return line, err
	}
	security.SetReadLine(func(prompt string) (string, error) {
		return readTerminal(prompt, false)
	})
	defer security.ResetReadLine()

	next := &application.BuildOptions{SessionID: *sessionID, NewSession: *newSession, Human: *human, HumanMode: *humanMode}
	for next != nil {
		built, rt, err := app.Build(*next)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start session: %v\n", err)
			if next.SessionID != "" {
				fmt.Fprintln(os.Stderr, "Start without --session, then use /session list to view available sessions.")
			}
			return 1
		}
		loop := newRepl(built, rt.Ctx, func() (string, error) {
			return readTerminal(replPrompt, true)
		})
		loop.run()
		next = loop.nextBuild()
		if next != nil {
			closeErr := rt.Close(context.Background())
			app.SetRuntime(nil)
			if closeErr != nil {
				fmt.Fprintf(os.Stderr, "Failed to close current session: %v\n", closeErr)
				return 1
			}
		}
	}
	return 0
}
