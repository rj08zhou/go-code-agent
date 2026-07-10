// Command frontend is a small standalone web app that renders the
// go-code-agent README (README_zh.md) as a styled HTML page, serves it
// on an ephemeral localhost port, and opens it in your default browser.
//
// Run it with:
//
//	go run ./frontend            # serves ../README_zh.md
//	go run ./frontend --file README.md --no-open
//
// Flags:
//
//	--file <path>   markdown file to render (default: README_zh.md)
//	--addr <host>   host to bind (default: 127.0.0.1)
//	--port <n>      port to bind; 0 = ephemeral (default: 0)
//	--no-open       do not open the browser automatically
//
// It blocks (serving) until interrupted with Ctrl-C. The same viewer is
// also reused by the agent's `/readme` REPL command.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go-code-agent/frontend/server"
)

func main() {
	workdir, _ := os.Getwd()
	file := flag.String("file", server.DefaultFile, "markdown file to render")
	host := flag.String("addr", "127.0.0.1", "host to bind")
	port := flag.Int("port", 0, "port to bind (0 = ephemeral)")
	noOpen := flag.Bool("no-open", false, "do not open the browser automatically")
	flag.Parse()

	// Default: open the browser unless --no-open was given.
	openBrowser := !*noOpen

	v, err := server.New(workdir, server.Options{
		File:        *file,
		Host:        *host,
		Port:        *port,
		OpenBrowser: openBrowser,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "frontend:", err)
		os.Exit(1)
	}

	if err := v.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "frontend:", err)
		os.Exit(1)
	}

	fmt.Printf("Serving %s\n", v.Addr())
	fmt.Println("Press Ctrl-C to stop.")

	// Block until interrupted.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	_ = v.Stop()
	fmt.Println("\nStopped.")
}
