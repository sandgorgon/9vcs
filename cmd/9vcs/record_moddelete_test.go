package main

import (
	"fmt"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// lessHash reports whether a sorts before b — the same tie-break
// topoOrder uses (objstore/patches/replay.go) when two patches are
// simultaneously ready during replay, e.g. two direct children of the
// same base. Used here purely to label which of two orderings a given
// salt landed in, not to influence anything.
func lessHash(a, b patches.Hash) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// TestModifyDeleteKeptTextOpsIsOrderIndependent is the regression test
// for a real, live-proven bug: modifyDeleteKeptTextOps used to be a
// plain Diff(nil, ...) fresh insert, which forked (two alive nodes, same
// content) whenever the deleting side's topological tiebreak happened to
// land before the modifying side's own insert — see the function's doc
// comment for the full mechanism. Both orderings are deterministic given
// fixed patch content, so this samples several salts (varying only the
// patch messages, which changes their hashes and therefore which
// ordering topoOrder picks) until both have been observed, and requires
// every single one to materialize to exactly the kept content with no
// fork — not just "usually."
func TestModifyDeleteKeptTextOpsIsOrderIndependent(t *testing.T) {
	original := []string{"alpha", "beta", "gamma", "delta"}
	modified := []string{"alpha", "BETA-EDITED", "gamma", "delta"}

	var sawOursFirst, sawTheirsFirst bool
	for salt := range 20 {
		r := newTestRepo(t)

		baseHash, baseIdx := recordTestPatch(t, r, nil, "f.txt", original, patches.Index{})

		// Built manually rather than via recordTestPatch (which always
		// uses a fixed message "x") — the message needs to vary by salt
		// so ours/theirs' hashes, and therefore topoOrder's tiebreak
		// between them, actually differ from one iteration to the next.
		baseLines, _ := patches.Linearize(baseIdx["f.txt"].Graph)
		oursOps, _ := patches.Diff(baseLines, modified)
		oursHash, err := r.store.Put(&patches.Patch{
			Dependencies: []patches.Hash{baseHash},
			Message:      fmt.Sprintf("ours modifies %d", salt),
			Changes:      []patches.FileChange{{Path: "f.txt", Kind: patches.KindText, TrailingNewline: true, Ops: oursOps}},
		})
		if err != nil {
			t.Fatal(err)
		}
		theirsHash, err := r.store.Put(&patches.Patch{
			Dependencies: []patches.Hash{baseHash},
			Message:      fmt.Sprintf("theirs deletes %d", salt),
			Changes:      []patches.FileChange{{Path: "f.txt", Kind: patches.KindDelete}},
		})
		if err != nil {
			t.Fatal(err)
		}

		oursFirst := lessHash(oursHash, theirsHash)
		if oursFirst {
			sawOursFirst = true
		} else {
			sawTheirsFirst = true
		}

		mergedBase, conflicts, err := computeMerge(r, oursHash, theirsHash)
		if err != nil {
			t.Fatalf("salt %d: computeMerge: %v", salt, err)
		}
		var found bool
		for _, c := range conflicts {
			if c.Kind == "modify/delete" && c.Path == "f.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("salt %d: expected a modify/delete conflict on f.txt, got %v", salt, conflicts)
		}

		// The content on disk after a real `merge`/`apply` would already
		// match mergedBase's own rendering (writeWorkingTree wrote it) —
		// reconstruct that content directly, the same way a real
		// checkout would have.
		contentLines, _ := patches.Linearize(mergedBase["f.txt"].Graph)
		var content []byte
		for i, l := range contentLines {
			if i > 0 {
				content = append(content, '\n')
			}
			content = append(content, []byte(l.Content)...)
		}
		content = append(content, '\n')

		ops := modifyDeleteKeptTextOps(mergedBase, "f.txt", content)
		mergePatch := &patches.Patch{
			Dependencies: []patches.Hash{oursHash, theirsHash},
			Message:      "finalize modify/delete",
			Changes:      []patches.FileChange{{Path: "f.txt", Kind: patches.KindText, TrailingNewline: true, Ops: ops}},
		}
		mergeHash, err := r.store.Put(mergePatch)
		if err != nil {
			t.Fatalf("salt %d: %v", salt, err)
		}

		idx, err := patches.Materialize(r.store, mergeHash)
		if err != nil {
			t.Fatalf("salt %d: %v", salt, err)
		}
		st, ok := idx["f.txt"]
		if !ok {
			t.Fatalf("salt %d (oursFirst=%v): f.txt missing entirely after materializing the finalized merge", salt, oursFirst)
		}
		lines, forks := patches.Linearize(st.Graph)
		if len(forks) != 0 {
			t.Errorf("salt %d (oursFirst=%v): got %d unresolved fork(s), want 0", salt, oursFirst, len(forks))
		}
		got := make([]string, len(lines))
		for i, l := range lines {
			got[i] = l.Content
		}
		if !sameStrings(got, modified) {
			t.Errorf("salt %d (oursFirst=%v): materialized content = %v, want %v", salt, oursFirst, got, modified)
		}
	}
	if !sawOursFirst || !sawTheirsFirst {
		t.Errorf("only sampled one topological ordering across 20 salts (oursFirst seen=%v, theirsFirst seen=%v) — test isn't actually exercising both cases", sawOursFirst, sawTheirsFirst)
	}
}

func sameStrings(a, b []string) bool {
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
