package model

import "fmt"

// StdoutStreamSink prints streaming output to the terminal.
type StdoutStreamSink struct {
	started   bool
	reasoning bool
}

func (s *StdoutStreamSink) OnReasoningDelta(text string) {
	if text == "" {
		return
	}
	if !s.reasoning {
		fmt.Print("\n[thinking] ")
		s.reasoning = true
	}
	fmt.Print(text)
}

func (s *StdoutStreamSink) OnTextDelta(text string) {
	if s.reasoning && !s.started {
		fmt.Println()
		s.reasoning = false
	}
	if !s.started {
		fmt.Print("\n>>> ")
		s.started = true
	}
	fmt.Print(text)
}

func (s *StdoutStreamSink) OnDone() {
	if s.started || s.reasoning {
		fmt.Println()
	}
}
