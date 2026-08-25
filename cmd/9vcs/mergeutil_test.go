package main

import (
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// recordTestPatch builds a patch out of a plain-text edit against base's
// current content for path, stores it, and returns its hash plus the
// updated Index — the cmd/9vcs-side equivalent of objstore/patches'
// private test helper of the same shape, needed here since computeMerge
// lives in this package.
func recordTestPatch(t *testing.T, r *repo, deps []patches.Hash, path string, content []string, base patches.Index) (patches.Hash, patches.Index) {
	t.Helper()
	var oldLines []patches.Line
	if st, ok := base[path]; ok && st.Kind == patches.KindText {
		oldLines, _ = patches.Linearize(st.Graph)
	}
	ops, _ := patches.Diff(oldLines, content)
	p := &patches.Patch{Dependencies: deps, Message: "x", Changes: []patches.FileChange{{Path: path, Kind: patches.KindText, Ops: ops, TrailingNewline: true}}}
	h, err := r.store.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := r.materialize(h)
	if err != nil {
		t.Fatal(err)
	}
	return h, idx
}

func contentsOf(t *testing.T, idx patches.Index, path string) []string {
	t.Helper()
	st, ok := idx[path]
	if !ok || st.Kind != patches.KindText {
		return nil
	}
	lines, _ := patches.Linearize(st.Graph)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Content
	}
	return out
}

// TestComputeMergeThreeWayCleanDisjoint: three branches off one base,
// each editing a different, unrelated file. All three combine with no
// conflict — the case apply exists for.
func TestComputeMergeThreeWayCleanDisjoint(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "shared.txt", []string{"shared"}, patches.Index{})
	a, _ := recordTestPatch(t, r, []patches.Hash{base}, "a.txt", []string{"from a"}, idx)
	b, _ := recordTestPatch(t, r, []patches.Hash{base}, "b.txt", []string{"from b"}, idx)
	c, _ := recordTestPatch(t, r, []patches.Hash{base}, "c.txt", []string{"from c"}, idx)

	merged, conflicts, err := computeMerge(r, a, b, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts merging three disjoint edits, got %v", conflicts)
	}
	for path, want := range map[string][]string{
		"shared.txt": {"shared"},
		"a.txt":      {"from a"},
		"b.txt":      {"from b"},
		"c.txt":      {"from c"},
	} {
		if got := contentsOf(t, merged, path); !sameStringsHelper(got, want) {
			t.Errorf("merged[%s] = %v, want %v", path, got, want)
		}
	}
}

// TestComputeMergeThreeWayTextConflict: all three branches edit the same
// line differently. Linearize must report a genuine fork with all three
// alternatives, not just two — the N-way-ness has to reach all the way
// through, not stop at "detect a conflict exists."
func TestComputeMergeThreeWayTextConflict(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "f.txt", []string{"one", "two", "three"}, patches.Index{})
	a, _ := recordTestPatch(t, r, []patches.Hash{base}, "f.txt", []string{"one", "TWO-a", "three"}, idx)
	b, _ := recordTestPatch(t, r, []patches.Hash{base}, "f.txt", []string{"one", "TWO-b", "three"}, idx)
	c, _ := recordTestPatch(t, r, []patches.Hash{base}, "f.txt", []string{"one", "TWO-c", "three"}, idx)

	merged, conflicts, err := computeMerge(r, a, b, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Kind != "text" {
		t.Fatalf("expected exactly one text conflict, got %v", conflicts)
	}
	_, forks := patches.Linearize(merged["f.txt"].Graph)
	if len(forks) != 1 {
		t.Fatalf("expected exactly one fork, got %d", len(forks))
	}
	if len(forks[0].Alternatives) != 3 {
		t.Fatalf("expected 3 alternatives at the fork (one per branch), got %d: %v", len(forks[0].Alternatives), forks[0].Alternatives)
	}
}

// TestComputeMergeThreeWayModifyDeleteIgnoresUninvolvedSide: root A
// deletes a path, root B genuinely modifies it, root C never touches it
// at all. Must still be detected as a modify/delete race between A and
// B specifically — C's presence in the merge must not mask or alter it.
func TestComputeMergeThreeWayModifyDeleteIgnoresUninvolvedSide(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "victim.txt", []string{"one", "two"}, patches.Index{})
	base, idx = recordTestPatch(t, r, []patches.Hash{base}, "untouched.txt", []string{"keep"}, idx)

	delPatch := &patches.Patch{Dependencies: []patches.Hash{base}, Message: "delete", Changes: []patches.FileChange{{Path: "victim.txt", Kind: patches.KindDelete}}}
	a, err := r.store.Put(delPatch)
	if err != nil {
		t.Fatal(err)
	}
	b, bIdx := recordTestPatch(t, r, []patches.Hash{base}, "victim.txt", []string{"one", "TWO-modified"}, idx)
	_ = bIdx
	c, _ := recordTestPatch(t, r, []patches.Hash{base}, "someone-else.txt", []string{"c's own file"}, idx)

	merged, conflicts, err := computeMerge(r, a, b, c)
	if err != nil {
		t.Fatal(err)
	}
	var found *mergeConflict
	for i := range conflicts {
		if conflicts[i].Path == "victim.txt" {
			found = &conflicts[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a modify/delete conflict on victim.txt, got %v", conflicts)
	}
	if found.Kind != "modify/delete" {
		t.Errorf("conflict kind = %q, want modify/delete", found.Kind)
	}
	// a deleted it; N > 2 roots, so DeletedBy is a's own short hash, not "ours"/"theirs".
	if found.DeletedBy != a.String()[:12] {
		t.Errorf("DeletedBy = %q, want a's short hash %q", found.DeletedBy, a.String()[:12])
	}
	if got := contentsOf(t, merged, "victim.txt"); !sameStringsHelper(got, []string{"one", "TWO-modified"}) {
		t.Errorf("victim.txt should keep the modified content, got %v", got)
	}
	if got := contentsOf(t, merged, "untouched.txt"); !sameStringsHelper(got, []string{"keep"}) {
		t.Errorf("untouched.txt should be unaffected: got %v", got)
	}
}

// TestComputeMergeThreeWayBinaryConflict: three roots set the same path
// to three different blobs. Must be flagged as a binary conflict and
// keep roots[0]'s content, same policy as the original two-way version.
func TestComputeMergeThreeWayBinaryConflict(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "keep.txt", []string{"unrelated"}, patches.Index{})

	blobA, err := r.blobs.Put([]byte("content A"))
	if err != nil {
		t.Fatal(err)
	}
	blobB, err := r.blobs.Put([]byte("content B"))
	if err != nil {
		t.Fatal(err)
	}
	blobC, err := r.blobs.Put([]byte("content C"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "a", Changes: []patches.FileChange{{Path: "logo.png", Kind: patches.KindBlob, Blob: blobA}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "b", Changes: []patches.FileChange{{Path: "logo.png", Kind: patches.KindBlob, Blob: blobB}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "c", Changes: []patches.FileChange{{Path: "logo.png", Kind: patches.KindBlob, Blob: blobC}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = idx

	merged, conflicts, err := computeMerge(r, a, b, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Kind != "binary" || conflicts[0].Path != "logo.png" {
		t.Fatalf("expected one binary conflict on logo.png, got %v", conflicts)
	}
	if merged["logo.png"].Blob != blobA {
		t.Errorf("expected roots[0]'s (a's) blob kept, got a different one")
	}
}

// TestComputeMergeThreeWayBinaryConflictAbsentFromOurs is the regression
// test for a real gap: roots[0] ("ours") never had the path at all, but
// roots[1] and roots[2] both introduce it with different blobs. The
// conflict-detection loop used to anchor solely on idxs[0], so a path
// missing from "ours" was never even looked at — this silently dropped
// one side's content instead of flagging a conflict. Must still be
// detected, keeping the first root that does have the path (roots[1]'s
// blob) rather than whichever Materialize's union happened to pick.
func TestComputeMergeThreeWayBinaryConflictAbsentFromOurs(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "keep.txt", []string{"unrelated"}, patches.Index{})
	_ = idx

	blobB, err := r.blobs.Put([]byte("content B"))
	if err != nil {
		t.Fatal(err)
	}
	blobC, err := r.blobs.Put([]byte("content C"))
	if err != nil {
		t.Fatal(err)
	}
	// a never touches new.png at all.
	a, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "a", Changes: nil})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "b", Changes: []patches.FileChange{{Path: "new.png", Kind: patches.KindBlob, Blob: blobB}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "c", Changes: []patches.FileChange{{Path: "new.png", Kind: patches.KindBlob, Blob: blobC}}})
	if err != nil {
		t.Fatal(err)
	}

	merged, conflicts, err := computeMerge(r, a, b, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Kind != "binary" || conflicts[0].Path != "new.png" {
		t.Fatalf("expected one binary conflict on new.png, got %v", conflicts)
	}
	if merged["new.png"].Blob != blobB {
		t.Errorf("expected roots[1]'s (b's) blob kept as the first root with the path, got a different one")
	}
}

// TestComputeMergeThreeWayTypeMismatchConflict is the regression test
// for a gap the fix above (TestComputeMergeThreeWayBinaryConflictAbsentFromOurs)
// didn't fully close: the anchor (the first root that has the path at
// all) only got compared against other roots' values when their Kind
// matched the anchor's — a root with the *same path* under a
// completely different Kind (text vs. blob) was never flagged at all.
// merged[p] silently kept whichever kind Materialize's union happened
// to pick, discarding the other side's content entirely with no
// reported conflict — the same class of silent data loss, just for a
// kind mismatch instead of an "absent from ours" gap.
func TestComputeMergeThreeWayTypeMismatchConflict(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "keep.txt", []string{"unrelated"}, patches.Index{})

	// a introduces "thing" as text.
	a, _ := recordTestPatch(t, r, []patches.Hash{base}, "thing", []string{"hello", "world"}, idx)

	// b introduces the same path as a binary blob instead.
	blobB, err := r.blobs.Put([]byte("binary content"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "b", Changes: []patches.FileChange{{Path: "thing", Kind: patches.KindBlob, Blob: blobB}}})
	if err != nil {
		t.Fatal(err)
	}

	merged, conflicts, err := computeMerge(r, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Kind != "type" || conflicts[0].Path != "thing" {
		t.Fatalf("expected one type conflict on thing, got %v", conflicts)
	}
	if merged["thing"].Kind != patches.KindText {
		t.Errorf("expected roots[0]'s (a's) text content kept, got Kind=%v", merged["thing"].Kind)
	}
}

func sameStringsHelper(a, b []string) bool {
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
