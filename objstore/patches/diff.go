package patches

import (
	"crypto/rand"
	"encoding/hex"
)

// Line is one line in a file's line graph: a stable identity plus its
// current text. The identity survives edits to *other* lines, which is what
// lets independent patches touching different lines commute.
type Line struct {
	ID      string
	Content string
}

// newLineID generates a fresh, globally-unique line identity. It is
// intentionally independent of content or position: duplicate lines (e.g.
// two blank lines) must still get distinct, stable identities.
func newLineID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failing is not a recoverable condition
	}
	return hex.EncodeToString(b[:])
}

// Diff computes the ops that transform old (the last-recorded line graph
// for a file, in order) into new (its current raw text content, split into
// lines), along with the resulting line list after those ops are applied.
//
// This is a plain LCS alignment on line content, not Pijul's full
// categorical patch construction — a deliberately simplified starting point
// (see PLAN.md "Open items to revisit"). Each op's Prev/Next name old's
// immediate neighbors at that point, which is what lets graph.go replay it
// as a real edge operation rather than a flat-list splice.
func Diff(old []Line, new []string) (ops []LineOp, newIndex []Line) {
	oldContent := make([]string, len(old))
	for i, l := range old {
		oldContent[i] = l.Content
	}

	// nextID(i) is the id immediately after position i in old ("" at EOF) —
	// exactly what Prev/Next need, independent of what else is deleted.
	nextID := func(i int) string {
		if i < len(old) {
			return old[i].ID
		}
		return ""
	}
	prevOldID := func(i int) string {
		if i > 0 {
			return old[i-1].ID
		}
		return ""
	}

	// common holds the exact (old-index, new-index) pairs the LCS DP chose,
	// not just the shared content — see lcs's doc comment for why matching
	// pairs back up by content alone (the previous approach) corrupts the
	// graph the moment old has a duplicate line.
	common := lcs(oldContent, new)

	var (
		oi, ni, ci int
		prevID     string // id of the most recently emitted line in the new sequence
	)

	// emitGap deletes old[oi:oldEnd] and inserts new[ni:newEnd] — everything
	// sitting in one gap between two retained lines (or a file boundary) —
	// advancing oi, ni and prevID as it goes. next is that gap's far edge:
	// the retained line right after it, or "" at EOF.
	//
	// A run of two or more consecutive deletes needs an extra repair pass
	// first. graph.go's OpDelete reconnects strictly around its own
	// immediate neighbors, so within a same-patch run only the *first*
	// delete's reconnect edge originates from a node this patch didn't
	// also delete — every later one starts from a node that's now dead
	// and unreachable, so it's inert. But that surviving first edge still
	// only spans one hop, from the gap's near edge to the run's *second*
	// node, not all the way to next — a shortcut an insert sharing this
	// same gap doesn't know to retract, because an insert's own "splitting
	// whatever direct edge was there" step only ever targets the literal
	// (near edge, next) pair it was given, never a multi-hop route reached
	// by chaining through dead nodes. Left alone, that shortcut and the
	// insert's own new edge both stay alive and reachable from the same
	// node — a structural fork indistinguishable from an unresolved merge
	// conflict, out of a single linear edit with no concurrent patch
	// involved. Confirmed live: `old=[x,y,z] new=[a,z]` forks every time
	// on the pre-fix code, with no duplicate content anywhere in sight.
	//
	// Collapsing the run's contribution down to one direct edge spanning
	// the whole gap first — sever the stray one-hop shortcut, link the
	// gap's near and far edges directly — means a following insert's split
	// retires it cleanly, exactly as already happens when only one line is
	// deleted (there, the single delete's own reconnect *is* already the
	// direct (near edge, next) edge, so nothing extra is needed). Emitted
	// as plain OpSever/OpLink, not a special case in graph.go's apply — the
	// same primitives Resolve already uses to heal a real fork.
	emitGap := func(oldEnd, newEnd int, next string) {
		runStart := oi
		for oi < oldEnd {
			ops = append(ops, LineOp{Kind: OpDelete, ID: old[oi].ID, Prev: prevOldID(oi), Next: nextID(oi + 1)})
			oi++
		}
		if run := oldEnd - runStart; run >= 2 {
			near := prevOldID(runStart)
			ops = append(ops,
				LineOp{Kind: OpSever, Prev: near, Next: old[runStart+1].ID},
				LineOp{Kind: OpLink, Prev: near, Next: next},
			)
		}
		for ni < newEnd {
			id := newLineID()
			ops = append(ops, LineOp{Kind: OpInsert, ID: id, Prev: prevID, Next: next, Content: new[ni]})
			newIndex = append(newIndex, Line{ID: id, Content: new[ni]})
			prevID = id
			ni++
		}
	}

	for ci < len(common) {
		emitGap(common[ci].oldIdx, common[ci].newIdx, old[common[ci].oldIdx].ID)
		// The common line itself: retained unchanged, keep its old id.
		newIndex = append(newIndex, Line{ID: old[oi].ID, Content: old[oi].Content})
		prevID = old[oi].ID
		oi++
		ni++
		ci++
	}
	emitGap(len(old), len(new), "")
	return ops, newIndex
}

// lcsPair is one line of the alignment lcs picks: a[oldIdx] and b[newIdx]
// hold equal content, and both indices are exact positions in a and b —
// never re-derived from content after the fact.
type lcsPair struct{ oldIdx, newIdx int }

// lcs returns the longest common subsequence of a and b, as the exact index
// pairs the alignment picked. O(n*m) dynamic program — fine for the file
// sizes this scaffold targets; not intended as the final word on diff
// performance.
//
// Returning indices rather than content strings matters the moment a or b
// has a duplicate line: Diff used to re-scan a and b for "the next line
// whose content equals this common entry," which can land on a different
// occurrence than the one the DP actually aligned (e.g. old totally
// containing two "b" lines and new reordering them relative to a "c").
// That mismatch fabricated Prev/Next anchors for a real (non-duplicate)
// graph node that didn't reflect the alignment at all, and replaying them
// against the graph — which tracks exact identity, not content — produced
// a node with more than one alive successor: a structural fork
// indistinguishable from an unresolved merge conflict, out of a single
// linear edit with no concurrent history involved. Confirmed live via a
// property fuzz test (old/new sequences over a 3-symbol alphabet) and by
// hand with `old=[b,b,c] new=[a,c,b]`, both before and after this fix.
func lcs(a, b []string) []lcsPair {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []lcsPair
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, lcsPair{oldIdx: i, newIdx: j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}
