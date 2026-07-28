package main

import (
	"context"
	"flag"
	"fmt"
	"go-code-agent/internal/application"
	"go-code-agent/internal/utils"
	"os"
	"strings"

	"github.com/chzyer/readline"
)

func main() {
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
	human := flag.Bool("human", false, "Escalate HITL to interactive (all tools require confirmation)")
	humanMode := flag.String("human-mode", "", "Override HITL mode: interactive|safe-only|auto-approve|auto-reject|notify-only (default: safe-only)")
	flag.Parse()

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
		os.Exit(1)
	}
	defer app.Shutdown(context.Background())

	rl, err := readline.New(utils.Blue + "> " + utils.Reset)
	if err != nil {
		panic(err)
	}
	defer rl.Close()

	next := &application.BuildOptions{SessionID: *sessionID, NewSession: *newSession, Human: *human, HumanMode: *humanMode}
	for next != nil {
		built, rt := app.Build(*next)
		printBanner(built)
		loop := newRepl(built, rt.Ctx, rl.Readline)
		loop.run()
		next = loop.nextBuild()
		if next != nil {
			_ = rt.Close(context.Background())
		}
	}
}

func printBanner(b *application.BuiltRunner) {
	judgeStatus := "off"
	if b.Runtime.JudgeEnabled {
		judgeStatus = "on"
	}

	divider := strings.Repeat("=", 60)
	fmt.Println(utils.Bold + utils.Cyan + divider + utils.Reset)
	fmt.Printf("%s  go-code-agent%s\n", utils.Bold+utils.Cyan, utils.Reset)
	fmt.Printf("  Model: %s  |  Workspace: %s\n", b.Session.ModelID, b.Session.Workdir)
	fmt.Printf("  Session: %s - %s\n", b.Session.ID[:13], b.Session.Title)
	fmt.Printf("  HITL: %s  |  Judge: %s\n", hitlStatus(b), judgeStatus)
	fmt.Println(utils.Bold + utils.Cyan + divider + utils.Reset)
	fmt.Println()
}

func hitlStatus(b *application.BuiltRunner) string {
	if b.Security.HITL == nil || !b.Security.HITL.IsEnabled() {
		return "off"
	}
	return fmt.Sprintf("on (%s)", b.Security.HITL.Mode())
}
