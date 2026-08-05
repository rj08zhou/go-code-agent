package repl

import "fmt"

func (r *Loop) handleTaskCommand(parts []string) {
	switch parts[0] {
	case "/task":
		if len(parts) < 2 {
			fmt.Println("Usage: /task clear|reset")
			fmt.Println("  clear — mark all completed tasks as deleted (hide from /dag)")
			fmt.Println("  reset — remove all tasks and start fresh")
			return
		}
		switch parts[1] {
		case "clear":
			fmt.Println(r.built.Tasks.Service.ClearCompleted())
		case "reset":
			r.built.Tasks.Service.Reset()
			fmt.Println("All tasks cleared.")
		default:
			fmt.Printf("Unknown: %s\n", parts[1])
		}
	case "/tasks":
		if r.built.Tasks.Todos != nil {
			fmt.Println(r.built.Tasks.Todos.Render())
		} else {
			fmt.Println("No todos.")
		}
		fmt.Println("(persistent / dependency-tracked tasks are shown via /dag)")
	case "/dag":
		fmt.Println(r.built.Tasks.Service.TopoView())
		if progress := r.built.Tasks.Service.ProgressSummary(); progress != "" {
			fmt.Println(progress)
		}
	case "/memory":
		fmt.Println(r.built.Tasks.Memory.Stats())
	}
}
