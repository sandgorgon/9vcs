package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// textPathState builds a PathState for content, materialized through a
// real FileGraph via patches.Diff/apply — same mechanism graph.go itself
// uses — so linesOf(oldSt) behaves exactly as it would against a real
// repo's base, not a hand-faked stand-in.
func textPathState(t *testing.T, content []string) patches.PathState {
	t.Helper()
	ops, _ := patches.Diff(nil, content)
	// FileGraph.apply is unexported; go through a real Store/Materialize
	// round trip instead, mirroring what every other test in this package
	// already does via recordTestPatch.
	r := newTestRepo(t)
	p := &patches.Patch{Message: "seed", Changes: []patches.FileChange{{Path: "x", Kind: patches.KindText, Ops: ops, TrailingNewline: true}}}
	h, err := r.store.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := r.materialize(h)
	if err != nil {
		t.Fatal(err)
	}
	return idx["x"]
}

func newAddChange(t *testing.T, path string, content []string) patches.FileChange {
	t.Helper()
	ops, _ := patches.Diff(nil, content)
	return patches.FileChange{Path: path, Kind: patches.KindText, Ops: ops, TrailingNewline: true}
}

func TestRenameCandidatePureRenameIsUnmodified(t *testing.T) {
	oldSt := textPathState(t, []string{"one", "two", "three"})
	newFc := newAddChange(t, "new.txt", []string{"one", "two", "three"})

	score, pair, ok := renameCandidate("old.txt", oldSt, "new.txt", newFc)
	if !ok {
		t.Fatal("expected a match for identical content")
	}
	if score != 1.0 {
		t.Errorf("score = %v, want 1.0 for identical content", score)
	}
	if pair.modified {
		t.Error("pair.modified = true, want false for byte-identical content")
	}
}

func TestRenameCandidateRenameWithModification(t *testing.T) {
	oldSt := textPathState(t, []string{"one", "two", "three", "four"})
	newFc := newAddChange(t, "new.txt", []string{"one", "two", "THREE-EDITED", "four"})

	score, pair, ok := renameCandidate("old.txt", oldSt, "new.txt", newFc)
	if !ok {
		t.Fatalf("expected a match for mostly-similar content, score would have been computed")
	}
	if score < renameThreshold {
		t.Errorf("score = %v, want >= %v", score, renameThreshold)
	}
	if !pair.modified {
		t.Error("pair.modified = false, want true — one line differs")
	}
	if len(pair.diffOps) == 0 {
		t.Error("diffOps should describe the one-line change, got none")
	}
}

func TestRenameCandidateBelowThresholdIsNotAMatch(t *testing.T) {
	oldSt := textPathState(t, []string{"completely", "unrelated", "content", "here"})
	newFc := newAddChange(t, "new.txt", []string{"totally", "different", "stuff", "instead"})

	_, _, ok := renameCandidate("old.txt", oldSt, "new.txt", newFc)
	if ok {
		t.Error("expected no match for dissimilar content")
	}
}

func TestRenameCandidateBlobExactMatch(t *testing.T) {
	r := newTestRepo(t)
	h, err := r.blobs.Put([]byte("binary content"))
	if err != nil {
		t.Fatal(err)
	}
	oldSt := patches.PathState{Kind: patches.KindBlob, Blob: h}
	newFc := patches.FileChange{Path: "new.bin", Kind: patches.KindBlob, Blob: h}

	score, pair, ok := renameCandidate("old.bin", oldSt, "new.bin", newFc)
	if !ok {
		t.Fatal("expected a match for identical blob content")
	}
	if score != 1.0 || pair.modified {
		t.Errorf("score=%v modified=%v, want 1.0/false for an exact blob match", score, pair.modified)
	}
}

func TestRenameCandidateBlobMismatchIsNotAMatch(t *testing.T) {
	r := newTestRepo(t)
	h1, err := r.blobs.Put([]byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := r.blobs.Put([]byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	oldSt := patches.PathState{Kind: patches.KindBlob, Blob: h1}
	newFc := patches.FileChange{Path: "new.bin", Kind: patches.KindBlob, Blob: h2}

	if _, _, ok := renameCandidate("old.bin", oldSt, "new.bin", newFc); ok {
		t.Error("expected no match — binary content has no partial similarity in this design")
	}
}

func TestRenameCandidateKindMismatchIsNotAMatch(t *testing.T) {
	oldSt := textPathState(t, []string{"text content"})
	newFc := patches.FileChange{Path: "new.bin", Kind: patches.KindBlob, Blob: patches.Hash{1}}
	if _, _, ok := renameCandidate("old.txt", oldSt, "new.bin", newFc); ok {
		t.Error("a text file becoming a binary one under a new path isn't a rename")
	}
}

// TestDetectRenamesGreedyPicksHighestSimilarity: two deleted paths, two
// added paths, where one added path is a strong match for one deleted
// path and only a weak (still-above-threshold) match for the other —
// each path must be consumed by at most one pairing, and the assignment
// should prefer the higher-scoring match.
func TestDetectRenamesGreedyPicksHighestSimilarity(t *testing.T) {
	base := patches.Index{
		"a.txt": textPathState(t, []string{"alpha", "one", "two", "three"}),
		"b.txt": textPathState(t, []string{"beta", "one", "two", "three"}),
	}
	changes := map[string]patches.FileChange{
		"a.txt":  {Path: "a.txt", Kind: patches.KindDelete},
		"b.txt":  {Path: "b.txt", Kind: patches.KindDelete},
		"a2.txt": newAddChange(t, "a2.txt", []string{"alpha", "one", "two", "three"}), // exact match for a.txt
		"b2.txt": newAddChange(t, "b2.txt", []string{"beta", "one", "two", "three"}),  // exact match for b.txt
	}

	renames, remaining := detectRenames(changes, base)
	if len(renames) != 2 {
		t.Fatalf("got %d renames, want 2: %+v", len(renames), renames)
	}
	got := map[string]string{}
	for _, rp := range renames {
		got[rp.oldPath] = rp.newPath
	}
	if got["a.txt"] != "a2.txt" || got["b.txt"] != "b2.txt" {
		t.Errorf("pairing = %v, want a.txt->a2.txt, b.txt->b2.txt", got)
	}
	if len(remaining) != 0 {
		t.Errorf("remaining = %v, want empty (every path consumed by a pairing)", remaining)
	}
}

func TestDetectRenamesNoDeletesOrNoAddsIsANoop(t *testing.T) {
	changes := map[string]patches.FileChange{
		"a.txt": {Path: "a.txt", Kind: patches.KindText}, // modified, not new (already in base) and not deleted
	}
	base := patches.Index{"a.txt": {Kind: patches.KindText}}
	renames, remaining := detectRenames(changes, base)
	if len(renames) != 0 {
		t.Errorf("expected no renames with no delete/add pair, got %v", renames)
	}
	if len(remaining) != 1 {
		t.Errorf("remaining should be untouched when nothing is detected, got %v", remaining)
	}
}

// TestRenameCandidateSkipsExpensiveDiffForHugeFiles is the regression
// test for a real O(n*m) time-and-memory cost (patches.Diff's
// underlying LCS is a full 2D table, not just O(min(n,m))) that
// detectRenames could otherwise multiply across every deleted×added
// pair in a changeset. A pair whose line-count product exceeds
// maxRenameDiffCells must be rejected as a candidate without attempting
// the diff at all — checked here by using a size well over the
// threshold and requiring the call to return quickly, not by measuring
// memory directly.
func TestRenameCandidateSkipsExpensiveDiffForHugeFiles(t *testing.T) {
	big := make([]string, 6000)
	for i := range big {
		big[i] = fmt.Sprintf("line %d", i)
	}
	oldSt := textPathState(t, big)
	newFc := newAddChange(t, "new.txt", append(append([]string{}, big...), "one more line"))

	if int64(len(big))*int64(len(big)+1) <= maxRenameDiffCells {
		t.Fatalf("test setup: %d*%d must exceed maxRenameDiffCells (%d) for this test to actually exercise the guard", len(big), len(big)+1, maxRenameDiffCells)
	}

	done := make(chan struct{})
	var ok bool
	go func() {
		_, _, ok = renameCandidate("old.txt", oldSt, "new.txt", newFc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renameCandidate did not return quickly — the size guard doesn't seem to be short-circuiting before the diff")
	}
	if ok {
		t.Error("expected the oversized pair to be rejected as a candidate, got a match")
	}
}
