package repo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func TestSelectOpsIndependentEdits(t *testing.T) {
	// old: [A, B, C]; new: [A, X, B, C] with an unrelated delete of C —
	// i.e. two edits that don't interact at all. Selecting only the
	// insert must leave the delete out, and vice versa.
	a, b, c := patches.Line{ID: "a", Content: "A"}, patches.Line{ID: "b", Content: "B"}, patches.Line{ID: "c", Content: "C"}
	ops, _ := patches.Diff([]patches.Line{a, b, c}, []string{"A", "X", "B"})

	var insertID string
	for _, op := range ops {
		if op.Kind == patches.OpInsert {
			insertID = op.ID
		}
	}
	if insertID == "" {
		t.Fatal("expected an insert op in the diff")
	}

	onlyInsert := SelectOps(ops, map[string]bool{insertID: true})
	if len(onlyInsert) != 1 || onlyInsert[0].Kind != patches.OpInsert {
		t.Fatalf("SelectOps(insert only) = %+v, want exactly the one insert", onlyInsert)
	}

	onlyDelete := SelectOps(ops, map[string]bool{"c": true})
	if len(onlyDelete) != 1 || onlyDelete[0].Kind != patches.OpDelete || onlyDelete[0].ID != "c" {
		t.Fatalf("SelectOps(delete only) = %+v, want exactly the delete of c", onlyDelete)
	}
}

// TestSelectOpsReanchorsInsertChain is the core correctness case: three
// new consecutive lines are inserted in one run, so Diff chains each
// insert's Prev to the previous insert's freshly-minted ID (X2's Prev is
// X1's ID, X3's Prev is X2's ID). Selecting a non-contiguous subset (X1
// and X3, skipping X2) must not leave X3 pointing at an ID that was never
// created — SelectOps has to re-point X3's Prev at X1 instead. Verified
// by actually replaying the result through the real Store/Materialize
// pipeline, not just inspecting the op structs.
func TestSelectOpsReanchorsInsertChain(t *testing.T) {
	r := newSelectTestRepo(t)

	baseOps, baseLines := patches.Diff(nil, []string{"A", "B"})
	baseHash, err := r.Store.Put(&patches.Patch{Message: "base", Changes: []patches.FileChange{
		{Path: "f.txt", Kind: patches.KindText, Ops: baseOps, TrailingNewline: true},
	}})
	if err != nil {
		t.Fatal(err)
	}

	ops, _ := patches.Diff(baseLines, []string{"A", "X1", "X2", "X3", "B"})
	if len(ops) != 3 {
		t.Fatalf("expected 3 inserts, got %d: %+v", len(ops), ops)
	}
	x1, x2, x3 := ops[0], ops[1], ops[2]
	if x2.Prev != x1.ID || x3.Prev != x2.ID {
		t.Fatalf("expected a chained Prev sequence, got %+v", ops)
	}

	selected := SelectOps(ops, map[string]bool{x1.ID: true, x3.ID: true})
	if len(selected) != 2 {
		t.Fatalf("SelectOps(x1,x3) = %+v, want 2 ops", selected)
	}
	if selected[0].ID != x1.ID || selected[0].Prev != x1.Prev {
		t.Errorf("x1 should be unchanged: got %+v", selected[0])
	}
	if selected[1].ID != x3.ID {
		t.Fatalf("expected x3 second, got %+v", selected[1])
	}
	if selected[1].Prev != x1.ID {
		t.Errorf("x3.Prev = %q, want re-anchored to x1 (%q)", selected[1].Prev, x1.ID)
	}
	if selected[1].Next != x3.Next {
		t.Errorf("x3.Next changed unexpectedly: got %q, want %q", selected[1].Next, x3.Next)
	}

	patchHash, err := r.Store.Put(&patches.Patch{Dependencies: []patches.Hash{baseHash}, Message: "x1,x3 only", Changes: []patches.FileChange{
		{Path: "f.txt", Kind: patches.KindText, Ops: selected, TrailingNewline: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	idx, err := r.Materialize(patchHash)
	if err != nil {
		t.Fatal(err)
	}
	lines, forks := patches.Linearize(idx["f.txt"].Graph)
	if len(forks) != 0 {
		t.Fatalf("unexpected forks after applying re-anchored selection: %v", forks)
	}
	got := contentOf(lines)
	want := []string{"A", "X1", "X3", "B"}
	if !equalStrings(got, want) {
		t.Errorf("materialized = %v, want %v", got, want)
	}
}

func TestSelectOpsEmptySelectionYieldsNoOps(t *testing.T) {
	a := patches.Line{ID: "a", Content: "A"}
	ops, _ := patches.Diff([]patches.Line{a}, []string{"A", "X"})
	if got := SelectOps(ops, nil); len(got) != 0 {
		t.Errorf("SelectOps(nil selection) = %+v, want empty", got)
	}
}

func TestSelectionApplyNarrowsTextOps(t *testing.T) {
	a := patches.Line{ID: "a", Content: "A"}
	ops, _ := patches.Diff([]patches.Line{a}, []string{"A", "X"})
	var insertID string
	for _, op := range ops {
		if op.Kind == patches.OpInsert {
			insertID = op.ID
		}
	}
	changes := map[string]patches.FileChange{
		"f.txt": {Path: "f.txt", Kind: patches.KindText, Ops: ops},
	}
	sel := Selection{Lines: map[string]map[string]bool{"f.txt": {insertID: true}}}
	out, err := sel.Apply(changes)
	if err != nil {
		t.Fatal(err)
	}
	if fc, ok := out["f.txt"]; !ok || len(fc.Ops) != 1 {
		t.Fatalf("out[f.txt] = %+v, ok=%v, want exactly 1 op", out["f.txt"], ok)
	}
}

func TestSelectionApplyWholeFileViaFiles(t *testing.T) {
	changes := map[string]patches.FileChange{
		"img.png": {Path: "img.png", Kind: patches.KindBlob, Blob: patches.Hash{1}},
	}
	sel := Selection{Files: map[string]bool{"img.png": true}}
	out, err := sel.Apply(changes)
	if err != nil {
		t.Fatal(err)
	}
	if fc, ok := out["img.png"]; !ok || fc.Kind != patches.KindBlob {
		t.Fatalf("out[img.png] = %+v, ok=%v", fc, ok)
	}
}

func TestSelectionApplyRejectsUnknownPath(t *testing.T) {
	sel := Selection{Files: map[string]bool{"missing.txt": true}}
	if _, err := sel.Apply(map[string]patches.FileChange{}); err == nil {
		t.Fatal("expected an error selecting a path with no pending changes")
	}
}

func TestSelectionApplyRejectsLinesOnNonText(t *testing.T) {
	changes := map[string]patches.FileChange{
		"img.png": {Path: "img.png", Kind: patches.KindBlob, Blob: patches.Hash{1}},
	}
	sel := Selection{Lines: map[string]map[string]bool{"img.png": {"x": true}}}
	if _, err := sel.Apply(changes); err == nil {
		t.Fatal("expected an error selecting lines on a non-text change")
	}
}

func TestSelectionApplyRejectsDoubleSelection(t *testing.T) {
	a := patches.Line{ID: "a", Content: "A"}
	ops, _ := patches.Diff([]patches.Line{a}, []string{"A", "X"})
	var insertID string
	for _, op := range ops {
		if op.Kind == patches.OpInsert {
			insertID = op.ID
		}
	}
	changes := map[string]patches.FileChange{"f.txt": {Path: "f.txt", Kind: patches.KindText, Ops: ops}}
	sel := Selection{
		Files: map[string]bool{"f.txt": true},
		Lines: map[string]map[string]bool{"f.txt": {insertID: true}},
	}
	if _, err := sel.Apply(changes); err == nil {
		t.Fatal("expected an error when a path is selected by both Files and Lines")
	}
}

func TestSelectionApplyRejectsEmptySelection(t *testing.T) {
	if _, err := (Selection{}).Apply(map[string]patches.FileChange{"f.txt": {}}); err == nil {
		t.Fatal("expected an error selecting nothing")
	}
}

// TestSelectiveRecordLeavesRemainderPending is the end-to-end proof of
// this feature's core design claim: selective record needs to construct
// nothing at all for the unselected side, because a later ChangedFiles
// call against the newly-advanced base regenerates it on its own. Two
// independent edits are made to one file; only one is selected and
// folded into a patch; ChangedFiles against the new base must then
// report exactly the other edit as still pending, unprompted.
func TestSelectiveRecordLeavesRemainderPending(t *testing.T) {
	r := newSelectTestRepo(t)

	baseHash, err := r.Store.Put(&patches.Patch{Message: "base"})
	if err != nil {
		t.Fatal(err)
	}
	base := patches.Index{}
	writeTestFile(t, r, "f.txt", "A\nB\nC\n")
	baseChanges, err := ChangedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	basePatch := &patches.Patch{Dependencies: []patches.Hash{baseHash}, Message: "add f.txt", Changes: valuesOf(baseChanges)}
	baseHash, err = r.Store.Put(basePatch)
	if err != nil {
		t.Fatal(err)
	}
	base, err = r.Materialize(baseHash)
	if err != nil {
		t.Fatal(err)
	}

	// Two independent edits: insert "X" after A, and delete C.
	writeTestFile(t, r, "f.txt", "A\nX\nB\n")
	changes, err := ChangedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := changes["f.txt"]
	if !ok || fc.Kind != patches.KindText {
		t.Fatalf("expected a pending text change at f.txt, got %+v ok=%v", fc, ok)
	}
	var insertID, deleteID string
	for _, op := range fc.Ops {
		switch op.Kind {
		case patches.OpInsert:
			insertID = op.ID
		case patches.OpDelete:
			deleteID = op.ID
		}
	}
	if insertID == "" || deleteID == "" {
		t.Fatalf("expected both an insert and a delete op, got %+v", fc.Ops)
	}

	sel := Selection{Lines: map[string]map[string]bool{"f.txt": {insertID: true}}}
	selected, err := sel.Apply(changes)
	if err != nil {
		t.Fatal(err)
	}
	patch := &patches.Patch{Dependencies: []patches.Hash{baseHash}, Message: "insert X only", Changes: valuesOf(selected)}
	newBaseHash, err := r.Store.Put(patch)
	if err != nil {
		t.Fatal(err)
	}
	newBase, err := r.Materialize(newBaseHash)
	if err != nil {
		t.Fatal(err)
	}

	lines, forks := patches.Linearize(newBase["f.txt"].Graph)
	if len(forks) != 0 {
		t.Fatalf("unexpected forks in recorded base: %v", forks)
	}
	if got := contentOf(lines); !equalStrings(got, []string{"A", "X", "B", "C"}) {
		t.Fatalf("recorded base content = %v, want [A X B C] (delete of C must still be pending, not recorded)", got)
	}

	// The disk file was never touched by this — status against the new
	// base must now report exactly the still-pending delete of C, with
	// no mention of X (already recorded) and no explicit bookkeeping from
	// SelectOps/Selection needed to make that happen.
	remaining, err := ChangedFiles(r, newBase)
	if err != nil {
		t.Fatal(err)
	}
	rfc, ok := remaining["f.txt"]
	if !ok {
		t.Fatal("expected f.txt to still show a pending change (the unselected delete)")
	}
	if len(rfc.Ops) != 1 || rfc.Ops[0].Kind != patches.OpDelete {
		t.Fatalf("remaining ops = %+v, want exactly one delete op", rfc.Ops)
	}
}

func newSelectTestRepo(t *testing.T) *Repo {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, DotDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch(DefaultBranch); err != nil {
		t.Fatal(err)
	}
	return r
}

func writeTestFile(t *testing.T, r *Repo, path, content string) {
	t.Helper()
	full := filepath.Join(r.Root, path)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func valuesOf(m map[string]patches.FileChange) []patches.FileChange {
	out := make([]patches.FileChange, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func contentOf(lines []patches.Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Content
	}
	return out
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
