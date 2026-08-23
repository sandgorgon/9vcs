package patches

import "sort"

const (
	conflictOpen = "<<<<<<< 9vcs conflict"
	conflictSep  = "======="
	conflictShut = ">>>>>>>"
	orphanOpen   = "<<<<<<< 9vcs unpositioned content (concurrent edit near a deleted line)"
)

// Fork records one unresolved conflict found while linearizing: From had
// more than one alive successor (Alternatives), each rendered as its own
// branch down to Tips (the last node of that branch actually rendered,
// which for a single-line alternative is the alternative itself), before
// reconverging at Reconverge ("" if the branches never reconverge).
//
// This is what Resolve needs to heal the conflict afterward — a
// linearized *rendering* alone doesn't carry enough information, since it
// only shows one arbitrary traversal, not every alternative edge that
// needs retracting once the user picks/keeps content.
type Fork struct {
	From         string
	Alternatives []string
	Tips         []string
	Reconverge   string
}

// Linearize renders g as an ordered sequence of lines, the way a file on
// disk needs one. Where the graph has an unresolved fork — a node with
// more than one alive outgoing edge, meaning two patches independently
// extended the same point without seeing each other's edit — it renders
// both alternatives with inline conflict markers rather than picking one
// silently, and reports the fork so a later Resolve call can heal it.
// Marker lines are synthetic (fresh random ids, never real graph nodes).
func Linearize(g *FileGraph) (lines []Line, forks []Fork) {
	visited := map[string]bool{}
	lines, forks = walkFrom(g, "", nil, visited)

	var orphans []string
	for id, n := range g.Nodes {
		if n.Alive && !visited[id] {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) == 0 {
		return lines, forks
	}
	// Safety net: content that's alive but structurally unreachable (can
	// happen when one branch deletes a line another branch concurrently
	// anchored an insert on) must never silently vanish. Surface it,
	// rather than lose it, even though its ideal position isn't known.
	// Not modeled as a Fork (there's no "From"/reconverge structure to
	// heal) — a real resolution has to touch the source lines directly.
	sort.Strings(orphans)
	lines = append(lines, Line{ID: newLineID(), Content: orphanOpen})
	for _, id := range orphans {
		lines = append(lines, Line{ID: id, Content: g.Nodes[id].Content})
	}
	lines = append(lines, Line{ID: newLineID(), Content: conflictShut})
	return lines, forks
}

// walkFrom renders the graph from start (inclusive, if start is a real,
// not-yet-visited node — the virtual start "" renders nothing for itself)
// up to but excluding *stop, or to end-of-file if stop is nil.
func walkFrom(g *FileGraph, start string, stop *string, visited map[string]bool) (lines []Line, forks []Fork) {
	cur := start
	for {
		if stop != nil && cur == *stop {
			return lines, forks
		}
		if cur != "" && !visited[cur] {
			if n := g.Nodes[cur]; n != nil && n.Alive {
				visited[cur] = true
				lines = append(lines, Line{ID: cur, Content: n.Content})
			}
		}

		var candidates []string
		for _, s := range aliveSuccessors(g, cur) {
			if !visited[s] {
				candidates = append(candidates, s)
			}
		}
		switch len(candidates) {
		case 0:
			return lines, forks
		case 1:
			cur = candidates[0] // rendered at the top of the next iteration
		default:
			sort.Strings(candidates)
			conv := findReconvergence(g, candidates)
			var convPtr *string
			if conv != "" {
				convPtr = &conv
			}

			lines = append(lines, Line{ID: newLineID(), Content: conflictOpen})
			var tips []string
			for i, s := range candidates {
				if i > 0 {
					lines = append(lines, Line{ID: newLineID(), Content: conflictSep})
				}
				branchLines, branchForks := walkFrom(g, s, convPtr, visited)
				forks = append(forks, branchForks...)
				lines = append(lines, branchLines...)
				tip := s
				if n := len(branchLines); n > 0 {
					tip = branchLines[n-1].ID
				}
				tips = append(tips, tip)
			}
			lines = append(lines, Line{ID: newLineID(), Content: conflictShut})

			forks = append(forks, Fork{From: cur, Alternatives: candidates, Tips: tips, Reconverge: conv})

			if conv == "" {
				return lines, forks
			}
			cur = conv // rendered at the top of the next iteration
		}
	}
}

// findReconvergence looks for the first node reachable from every branch
// in starts, via simultaneous multi-source BFS over aliveSuccessors.
// Returns "" if the branches never reconverge (all independently run to
// end-of-file) — the whole rest of the file is then part of the conflict.
func findReconvergence(g *FileGraph, starts []string) string {
	reached := make([]map[string]bool, len(starts))
	frontier := make([][]string, len(starts))
	for i, s := range starts {
		reached[i] = map[string]bool{s: true}
		frontier[i] = []string{s}
	}
	inAll := func(id string) bool {
		for _, r := range reached {
			if !r[id] {
				return false
			}
		}
		return true
	}
	for iter := 0; iter <= len(g.Nodes); iter++ {
		progressed := false
		for i := range starts {
			var next []string
			for _, f := range frontier[i] {
				for _, s := range aliveSuccessors(g, f) {
					if reached[i][s] {
						continue
					}
					reached[i][s] = true
					next = append(next, s)
					progressed = true
					if inAll(s) {
						return s
					}
				}
			}
			frontier[i] = next
		}
		if !progressed {
			break
		}
	}
	return ""
}

// IsMarker reports whether content is one of Linearize's synthetic
// conflict-marker lines, for callers that need to refuse finalizing a
// patch with unresolved markers still literally present as content.
func IsMarker(content string) bool {
	switch content {
	case conflictOpen, conflictSep, conflictShut, orphanOpen:
		return true
	}
	return false
}

// StripMarkers drops the synthetic marker lines Linearize inserts around a
// conflict, keeping every real line's original id. This — not the
// marker-included rendering itself — is the correct base to diff a merge
// resolution's final content against: diffing against markers still in
// place lets a deleted marker's own reconnect splice a live path through
// what should be a fully-dead discarded alternative, once its neighbor in
// the presented list also gets deleted.
func StripMarkers(lines []Line) []Line {
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		switch l.Content {
		case conflictOpen, conflictSep, conflictShut, orphanOpen:
			continue
		}
		out = append(out, l)
	}
	return out
}

// Resolve computes the extra ops a merge-conflict resolution patch needs
// beyond the ordinary content diff, to heal each fork. finalLines is the
// user's edited content, diffed by the caller (via Diff, against
// StripMarkers of Linearize's presented rendering — never the
// marker-included rendering itself, see StripMarkers). That diff alone
// correctly threads together content that was actually inserted or
// deleted, but two failure modes remain that only the graph-structural
// knowledge in Fork can fix:
//
//   - An alternative's original edge to the fork point or reconvergence
//     point survives even once something else sits between them in the
//     resolved content — nothing in a flat content diff names that edge to
//     retract it. Healed by severing every original Fork edge that isn't
//     adjacent in finalLines.
//   - Two surviving alternatives that are content-wise identical to
//     before (the user kept both, unedited, just reordered) produce no
//     diff ops at all, so no edge ever connects them. Healed by linking
//     every adjacent pair within the fork's own span in finalLines,
//     regardless of whether Diff already handled it — redundant links are
//     harmless no-ops.
func Resolve(forks []Fork, finalLines []Line) []LineOp {
	pos := make(map[string]int, len(finalLines))
	for i, l := range finalLines {
		pos[l.ID] = i
	}
	// posOf treats "" (virtual start/end) as the position just outside
	// the slice on whichever side it's used; ok is false only when a real
	// id no longer appears at all (shouldn't happen — Fork's own nodes
	// are never deleted by a resolution, only reordered around).
	posOf := func(id string) (int, bool) {
		if id == "" {
			return -1, true
		}
		p, ok := pos[id]
		return p, ok
	}
	idAt := func(i int) string {
		if i < 0 || i >= len(finalLines) {
			return ""
		}
		return finalLines[i].ID
	}

	var ops []LineOp
	for _, f := range forks {
		fromPos, ok := posOf(f.From)
		if !ok {
			continue
		}
		toPos := len(finalLines)
		if f.Reconverge != "" {
			p, ok := posOf(f.Reconverge)
			if !ok {
				continue
			}
			toPos = p
		}

		for _, alt := range f.Alternatives {
			if p, ok := posOf(alt); !ok || p != fromPos+1 {
				ops = append(ops, LineOp{Kind: OpSever, Prev: f.From, Next: alt})
			}
		}
		if f.Reconverge != "" {
			for _, tip := range f.Tips {
				if p, ok := posOf(tip); !ok || p != toPos-1 {
					ops = append(ops, LineOp{Kind: OpSever, Prev: tip, Next: f.Reconverge})
				}
			}
		}

		prevID := f.From
		for i := fromPos + 1; i <= toPos; i++ {
			curID := idAt(i)
			ops = append(ops, LineOp{Kind: OpLink, Prev: prevID, Next: curID})
			prevID = curID
		}
	}
	return ops
}
