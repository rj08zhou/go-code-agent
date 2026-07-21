package model

import "fmt"

// StdoutStreamSink prints streaming output to the terminal.
type StdoutStreamSink struct{ started bool }

func (s *StdoutStreamSink) OnTextDelta(text string) {
	if !s.started {
		fmt.Print("\n>>> ")
		s.started = true
	}
	fmt.Print(text)
}

func (s *StdoutStreamSink) OnDone() {
	if s.started {
		fmt.Println()
	}
}
