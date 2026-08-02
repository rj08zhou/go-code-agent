package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

const providerHelpText = `
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

At least one of ANTHROPIC_API_KEY or OPENAI_API_KEY is required.`

type runOptions struct {
	workdir    string
	dataDir    string
	sessionID  string
	newSession bool
	human      bool
	humanMode  string
}

func parseFlags() (*runOptions, string, error) {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags]\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprint(flag.CommandLine.Output(), providerHelpText)
	}

	workdir := flag.String("workdir", "", "Working directory (default: current directory)")
	dataDir := flag.String("data-dir", "", "State directory (default: ~/.config/go-code-agent)")
	sessionID := flag.String("session", "", "Resume a specific session ID")
	newSession := flag.Bool("new-session", false, "Start a fresh session")
	human := flag.Bool("human", false, "Use manual approval mode")
	humanMode := flag.String("human-mode", "", "Advanced compatibility override: interactive|safe-only|auto-approve|auto-reject|notify-only")
	flag.Parse()

	if *sessionID != "" && *newSession {
		return nil, "", fmt.Errorf("invalid options: --session and --new-session cannot be used together")
	}
	if *sessionID != "" {
		if err := session.ValidateSessionID(*sessionID); err != nil {
			return nil, "", fmt.Errorf("invalid --session value: %w", err)
		}
	}

	wd := *workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}

	return &runOptions{
		workdir:    wd,
		dataDir:    *dataDir,
		sessionID:  *sessionID,
		newSession: *newSession,
		human:      *human,
		humanMode:  *humanMode,
	}, wd, nil
}

func resolveConfigDir(dataDir string) string {
	if dataDir != "" {
		return dataDir
	}
	dir := os.Getenv("HOME") + "/.config"
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		dir = d
	}
	return dir
}

func printProviderError() {
	fmt.Fprint(os.Stderr, `
No usable LLM provider. Set at least one API key, e.g.:

  export ANTHROPIC_API_KEY="sk-ant-..."   # for claude-* models (default)
  export OPENAI_API_KEY="sk-..."          # for gpt-* / OpenAI-compatible models

If a key is already set, make sure it matches MODEL_ID / LLM_PROVIDER.
Run with --help for the full list of environment variables.
`)
}

// setupTerminal initialises the diagnostic log file, readline instance,
// history file, and the security readline hook. Callers must defer the
// returned cleanup function to release resources in reverse order.
func setupTerminal(dataDir string) (readFunc func(string, bool) (string, error), cleanup func(), rerr error) {
	// Diagnostic log file — kept outside the session log to avoid
	// interleaving structured events with model output.
	logFile := filepath.Join(dataDir, "terminal", "agent.log")
	lf, err := store.OpenPrivateAppend(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] file logging disabled: %v\n", err)
	} else {
		logging.SetDefault(logging.New(lf, logging.LevelInfo, false))
	}

	// History file with restrictive permissions.
	histFile := filepath.Join(dataDir, "terminal", "history")
	if f, err := store.OpenPrivateAppend(histFile); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] repl history disabled: %v\n", err)
		histFile = ""
	} else {
		f.Close()
	}

	replPrompt := utils.Blue + "> " + utils.Reset
	rl, err := readline.NewEx(&readline.Config{
		Prompt:                 replPrompt,
		HistoryFile:            histFile,
		HistorySearchFold:      true,
		DisableAutoSaveHistory: true,
		AutoComplete:           newCompleter(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("terminal: %w", err)
	}

	var terminalMu sync.Mutex
	readFn := func(prompt string, saveHistory bool) (string, error) {
		terminalMu.Lock()
		defer terminalMu.Unlock()
		rl.SetPrompt(prompt)
		defer rl.SetPrompt(replPrompt)
		line, err := rl.Readline()
		if saveHistory && err == nil && strings.TrimSpace(line) != "" {
			_ = rl.SaveHistory(line)
		}
		return line, err
	}
	security.SetReadLine(func(prompt string) (string, error) {
		return readFn(prompt, false)
	})

	closers := []io.Closer{rl}
	if lf != nil {
		closers = append(closers, lf)
	}
	return readFn, func() {
		security.ResetReadLine()
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i].Close()
		}
	}, nil
}

func run() (exitCode int) {
	opts, _, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	cfgDir := resolveConfigDir(opts.dataDir)
	app, err := application.New(cfgDir, opts.workdir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize: %v\n", err)
		if errors.Is(err, provider.ErrNoProvider) {
			printProviderError()
		}
		return 1
	}
	defer func() {
		if err := app.Shutdown(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to shut down session: %v\n", err)
			exitCode = 1
		}
	}()

	replPrompt := utils.Blue + "> " + utils.Reset
	readTerminal, cleanup, err := setupTerminal(app.DataDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize terminal: %v\n", err)
		return 1
	}
	defer cleanup()

	next := &application.BuildOptions{
		SessionID:  opts.sessionID,
		NewSession: opts.newSession,
		Human:      opts.human,
		HumanMode:  opts.humanMode,
	}
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
			if closeErr := rt.Close(context.Background()); closeErr != nil {
				fmt.Fprintf(os.Stderr, "Failed to close current session: %v\n", closeErr)
				return 1
			}
			app.SetRuntime(nil)
		}
	}
	return 0
}
