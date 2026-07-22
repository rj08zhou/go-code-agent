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
	if b.JudgeEnabled {
		judgeStatus = "on"
	}

	divider := strings.Repeat("=", 60)
	fmt.Println(utils.Bold + utils.Cyan + divider + utils.Reset)
	fmt.Printf("%s  go-code-agent%s\n", utils.Bold+utils.Cyan, utils.Reset)
	fmt.Printf("  Model: %s  |  Workspace: %s\n", b.ModelID, b.Workdir)
	fmt.Printf("  Session: %s - %s\n", b.SessionID[:13], b.SessionTitle)
	fmt.Printf("  HITL: %s  |  Judge: %s\n", hitlStatus(b), judgeStatus)
	fmt.Println(utils.Bold + utils.Cyan + divider + utils.Reset)
	fmt.Println()
}

func hitlStatus(b *application.BuiltRunner) string {
	if b.HitlMgr == nil || !b.HitlMgr.IsEnabled() {
		return "off"
	}
	return fmt.Sprintf("on (%s)", b.HitlMgr.Mode())
}
