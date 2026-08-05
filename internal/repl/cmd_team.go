package repl

import (
	"context"
	"fmt"
	"strings"
)

func (r *Loop) handleTeamCommand(parts []string) {
	switch parts[0] {
	case "/team":
		if len(parts) == 1 {
			fmt.Println(r.built.Team.Mgr.ListAll())
			return
		}
		switch parts[1] {
		case "spawn":
			if len(parts) < 5 {
				fmt.Println("Usage: /team spawn <name> <role> <prompt>")
			} else {
				fmt.Println(r.built.Team.Mgr.Spawn(context.Background(), parts[2], parts[3], strings.Join(parts[4:], " ")))
			}
		case "shutdown":
			if len(parts) < 3 {
				fmt.Println("Usage: /team shutdown <name>")
			} else {
				fmt.Println(r.built.Team.Mgr.ShutdownByName(parts[2]))
			}
		case "message":
			if len(parts) < 4 {
				fmt.Println("Usage: /team message <name> <content>")
			} else {
				fmt.Println(r.built.Team.Bus.Send("lead", parts[2], strings.Join(parts[3:], " "), "message", nil))
			}
		case "inbox":
			fmt.Println(r.readInbox("lead"))
		default:
			fmt.Println(r.built.Team.Mgr.ListAll())
		}
	case "/inbox":
		fmt.Println(r.readInbox(r.built.Session.AgentID))
	}
}
