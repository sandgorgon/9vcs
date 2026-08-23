package patches

// Node is one line in a file's line graph: a stable identity plus its
// text. Nodes are never removed once created, even after they die — a
// later patch, unaware the node died, may still reference it as an anchor
// (Prev/Next), and dead nodes stay in the graph so that reference still
// resolves.
type Node struct {
	Content     string
	Alive       bool
	reconnectTo *string // set when killed: the single node (or "" = EOF) traffic through this one now routes to
}

type edgeKey struct{ from, to string }

// FileGraph is one path's accumulated line graph, built by replaying every
// patch that ever touched it (see Materialize). More than one alive edge
// leaving a node is a fork nobody has recorded an order for — a conflict,
// found by graph structure rather than by comparing rendered text.
type FileGraph struct {
	Nodes map[string]*Node
	edges map[edgeKey]bool    // alive/dead per (from,to) ever mentioned
	out   map[string][]string // from -> every "to" ever mentioned (order of first mention)
}

func newFileGraph() *FileGraph {
	return &FileGraph{
		Nodes: map[string]*Node{},
		edges: map[edgeKey]bool{},
		out:   map[string][]string{},
	}
}

func (g *FileGraph) setEdge(from, to string, alive bool) {
	key := edgeKey{from, to}
	if _, seen := g.edges[key]; !seen {
		g.out[from] = append(g.out[from], to)
	}
	g.edges[key] = alive
}

// apply replays ops against g, mutating it in place.
func (g *FileGraph) apply(ops []LineOp) {
	for _, op := range ops {
		switch op.Kind {
		case OpInsert:
			g.Nodes[op.ID] = &Node{Content: op.Content, Alive: true}
			g.setEdge(op.Prev, op.ID, true)
			g.setEdge(op.ID, op.Next, true)
			g.setEdge(op.Prev, op.Next, false) // splitting whatever direct edge was there
		case OpDelete:
			n, ok := g.Nodes[op.ID]
			if !ok {
				n = &Node{}
				g.Nodes[op.ID] = n
			}
			n.Alive = false
			next := op.Next
			n.reconnectTo = &next
			g.setEdge(op.Prev, op.ID, false)
			g.setEdge(op.ID, op.Next, false)
			g.setEdge(op.Prev, op.Next, true) // reconnect around the gap
		case OpSever:
			g.setEdge(op.Prev, op.Next, false)
		case OpLink:
			g.setEdge(op.Prev, op.Next, true)
		}
	}
}

// resolveAlive follows id to whatever alive node traffic through it now
// resolves to: itself, if alive; otherwise its reconnect target,
// recursively. A dead node has exactly one pass-through route — the one
// recorded when it was deleted — never a fork hiding behind it. Returns ""
// with ok=false if the route dead-ends at end-of-file.
func resolveAlive(g *FileGraph, id string, depth int) (string, bool) {
	if depth > len(g.Nodes)+1 {
		return "", false // defensive cycle guard; shouldn't happen for well-formed ops
	}
	n := g.Nodes[id]
	if n == nil {
		return "", false
	}
	if n.Alive {
		return id, true
	}
	if n.reconnectTo == nil || *n.reconnectTo == "" {
		return "", false
	}
	return resolveAlive(g, *n.reconnectTo, depth+1)
}

// aliveSuccessors returns the alive nodes immediately reachable from id by
// following its alive direct edges (passing transparently through any dead
// node in between). More than one result is a fork.
func aliveSuccessors(g *FileGraph, id string) []string {
	seen := map[string]bool{}
	var out []string
	for _, to := range g.out[id] {
		if !g.edges[edgeKey{id, to}] {
			continue
		}
		resolved, ok := resolveAlive(g, to, 0)
		if !ok || seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}
