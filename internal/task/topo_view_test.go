package task

import (
	"fmt"
	"strings"
	"testing"
)

// TestTopoView_SeparatesDisconnectedComponents is the regression test
// for a real bug: TopoView() ran a single Kahn pass over ALL tasks,
// merging every inDeg==0 node (including nodes from disjoint connected
// components / unrelated batches) into the same Stage. That made /dag
// falsely show unrelated work as one parallelizable plan.
//
// This test builds two unrelated sub-graphs:
//
//	Workflow A:  #1 -> #2 -> #3
//	Workflow B:  #7 -> #8
//
// The bug: #1 and #7 (both inDeg==0, from different components) land in
// the SAME "Stage 1". After the fix, TopoView must render them as
// separate workflows/stages (each component's roots never share a
// stage with another component's roots).
func TestTopoView_SeparatesDisconnectedComponents(t *testing.T) {
	tm, ds := newTestManagers(t)

	// Workflow A: linear chain 1 -> 2 -> 3
	tm.Create("A1", "desc", nil)
	tm.Create("A2", "desc", []int{1})
	tm.Create("A3", "desc", []int{2})

	// Workflow B: separate chain 7 -> 8 (disjoint from A)
	tm.Create("B7", "desc", nil)
	tm.Create("B8", "desc", []int{4}) // create order: 1,2,3,4,5 -> ids 1..5 (B7=4, B8=5)

	ds.AddEdge(1, 2)
	ds.AddEdge(2, 3)
	ds.AddEdge(4, 5)

	out := ds.TopoView()
	t.Logf("TopoView output:\n%s", out)

	// Parse workflow blocks: a line "Workflow N (M tasks):" starts a
	// block; the following lines (stages + task lines) until the next
	// "Workflow" belong to it.
	workflows := map[int][]string{}
	var cur int
	for _, line := range strings.Split(out, "\n") {
		if n, ok := workflowHeader(line); ok {
			cur = n
			continue
		}
		if cur != 0 {
			workflows[cur] = append(workflows[cur], line)
		}
	}

	if len(workflows) < 2 {
		t.Fatalf("expected >=2 workflows (one per disconnected component), got %d:\n%s", len(workflows), out)
	}

	// Find which workflow contains #1 (A's root) and which contains #4 (B's root).
	var wfOf1, wfOf4 int
	for w, lines := range workflows {
		for _, l := range lines {
			if strings.Contains(l, "#1:") {
				wfOf1 = w
			}
			if strings.Contains(l, "#4:") {
				wfOf4 = w
			}
		}
	}

	if wfOf1 == 0 || wfOf4 == 0 {
		t.Fatalf("could not locate roots in output:\n%s", out)
	}

	// THE ASSERTION: roots of disjoint components must be in DIFFERENT
	// workflows (not merged into one shared stage).
	if wfOf1 == wfOf4 {
		t.Fatalf("BUG STILL PRESENT: roots #1 (workflow A) and #4 (workflow B) "+
			"share Workflow %d — unrelated batches merged.\n%s", wfOf1, out)
	}
	t.Logf("OK: workflow A root in Workflow %d, workflow B root in Workflow %d (separated)", wfOf1, wfOf4)
}

// TestTopoView_DeterministicComponentOrder guards against a subtle
// nondeterminism: connectedComponents() picks an arbitrary unseen node
// as the BFS seed, and the component is later sorted by ids[0] (the
// seed). Because Go map iteration is randomized, when two components'
// ids interleave (e.g. A={1,4}, B={2,3}) the seed — and thus each
// component's ids[0] and the resulting Workflow order — could flip
// run-to-run. This test builds exactly that interleaved shape and
// asserts Workflow order is always by ascending minimum id, looping to
// outrun the map-randomization seed.
func TestTopoView_DeterministicComponentOrder(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		tm, ds := newTestManagers(t)

		// Two unrelated chains whose ids INTERLEAVE: A={1,4}, B={2,3}.
		tm.Create("A1", "desc", nil) // id 1
		tm.Create("B2", "desc", nil) // id 2
		tm.Create("B3", "desc", nil) // id 3
		tm.Create("A4", "desc", nil) // id 4
		ds.AddEdge(1, 4)             // chain A
		ds.AddEdge(2, 3)             // chain B

		out := ds.TopoView()

		// Locate the workflow index (1-based) of each component's root.
		workflowOf := map[int]int{} // task id -> workflow number
		var cur int
		for _, line := range strings.Split(out, "\n") {
			if n, ok := workflowHeader(line); ok {
				cur = n
				continue
			}
			if cur != 0 {
				for _, id := range []int{1, 2} {
					if strings.Contains(line, fmt.Sprintf("#%d:", id)) {
						workflowOf[id] = cur
					}
				}
			}
		}

		if workflowOf[1] == 0 || workflowOf[2] == 0 {
			t.Fatalf("iter %d: could not locate roots in output:\n%s", iter, out)
		}
		// Component A's min id (1) < component B's min id (2), so A must
		// render in an earlier Workflow than B — regardless of which
		// node happened to be picked as the BFS seed.
		if workflowOf[1] >= workflowOf[2] {
			t.Fatalf("iter %d: nondeterministic Workflow order: A(root #1) in %d, B(root #2) in %d (expected A before B)\n%s",
				iter, workflowOf[1], workflowOf[2], out)
		}
	}
}

// workflowHeader parses a "Workflow N (M tasks):" header line.
func workflowHeader(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "Workflow ") {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(line, "Workflow %d", &n); err == nil {
		return n, true
	}
	return 0, false
}
