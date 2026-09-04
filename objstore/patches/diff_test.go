package patches

import (
	"math/rand"
	"testing"
)

// applyToFreshGraph builds a graph as Materialize would from a single
// insert-only patch for old, then replays ops against it — the minimal
// setup that reproduces how a second Diff call's ops actually land on a
// real file's history.
func applyToFreshGraph(old []Line, ops []LineOp) *FileGraph {
	g := newFileGraph()
	var insertOps []LineOp
	prev := ""
	for _, l := range old {
		insertOps = append(insertOps, LineOp{Kind: OpInsert, ID: l.ID, Prev: prev, Content: l.Content})
		prev = l.ID
	}
	g.apply(insertOps)
	g.apply(ops)
	return g
}

// linearizeContent is Linearize plus a plain []string of its content, for
// tests that only care about the rendered text and whether it forked.
func linearizeContent(g *FileGraph) (content []string, forks []Fork) {
	lines, forks := Linearize(g)
	content = make([]string, len(lines))
	for i, l := range lines {
		content[i] = l.Content
	}
	return content, forks
}

// TestDiffMultiLineDeleteRunDoesNotFork is the regression test for a real
// bug: deleting two or more consecutive lines in one Diff call, with a
// fresh insert landing in the same gap, left a stray one-hop reconnect
// edge alive alongside the insert's own edge — a structural fork out of a
// single linear edit, no concurrent patch involved. `9vcs status` then
// reported the file as permanently modified after every future record
// (ChangedFiles refuses to call a path clean while its graph has any
// fork), and `9vcs diff` rendered an empty "--- / +++" header with no
// content under it, since the healing ops patches.Resolve appends are
// OpSever/OpLink, which diff.go's renderer didn't handle. Neither symptom
// needed duplicate line content — see diff.go's emitGap doc comment for
// the mechanism.
func TestDiffMultiLineDeleteRunDoesNotFork(t *testing.T) {
	cases := []struct {
		name string
		old  []string
		new  []string
	}{
		{"no duplicates", []string{"x", "y", "z"}, []string{"a", "z"}},
		{"duplicate content", []string{"b", "b", "c"}, []string{"a", "c", "b"}},
		{"delete run at EOF, no trailing common line", []string{"x", "y", "z"}, []string{"z", "a"}},
		{"whole file replaced", []string{"x", "y", "z"}, []string{"a", "b"}},
		{"three-line delete run", []string{"w", "x", "y", "z"}, []string{"a", "z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, old := Diff(nil, tc.old)
			ops, _ := Diff(old, tc.new)
			g := applyToFreshGraph(old, ops)

			got, forks := linearizeContent(g)
			if len(forks) != 0 {
				t.Fatalf("got %d spurious fork(s) from a single linear edit: %+v", len(forks), forks)
			}
			if !equalStrings(got, tc.new) {
				t.Fatalf("linearized content = %v, want %v", got, tc.new)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDiffRoundtripFuzz property-tests Diff against a small alphabet, which
// forces heavy line-content duplication: for random old/new pairs, replay
// old's own ops onto a fresh graph, then replay Diff(old, new)'s ops on
// top, and check the result linearizes to exactly new with no forks. This
// is what caught both bugs fixed by lcs returning index pairs instead of
// content, and by emitGap's multi-line-delete-run repair.
func TestDiffRoundtripFuzz(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", "c", "d"}
	randomLines := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = alphabet[r.Intn(len(alphabet))]
		}
		return out
	}

	const trials = 20000
	for trial := range trials {
		oldStrs := randomLines(r.Intn(8))
		newStrs := randomLines(r.Intn(8))

		_, old := Diff(nil, oldStrs)
		ops, _ := Diff(old, newStrs)
		g := applyToFreshGraph(old, ops)

		got, forks := linearizeContent(g)
		if len(forks) != 0 {
			t.Fatalf("trial %d: old=%v new=%v got a spurious fork: %+v", trial, oldStrs, newStrs, forks)
		}
		if !equalStrings(got, newStrs) {
			t.Fatalf("trial %d: old=%v new=%v got=%v", trial, oldStrs, newStrs, got)
		}
	}
}
