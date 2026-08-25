package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func TestMergeAbortNoMergeInProgressIsAnError(t *testing.T) {
	r := newTestRepo(t)
	if err := cmdMergeAbort(r); err == nil {
		t.Error("expected an error aborting when no merge is in progress, got nil")
	}
}

// TestMergeAbortRestoresWorkingTreeAndClearsState is the core property:
// after a real text conflict, abort must put the working tree back to
// exactly head's content (no conflict markers left behind) and clear
// MERGE_HEAD, regardless of whatever hand-editing happened in between.
func TestMergeAbortRestoresWorkingTreeAndClearsState(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "f.txt", []string{"one", "two", "three"}, patches.Index{})
	ours, oursIdx := recordTestPatch(t, r, []patches.Hash{base}, "f.txt", []string{"one", "OURS", "three"}, idx)
	theirs, _ := recordTestPatch(t, r, []patches.Hash{base}, "f.txt", []string{"one", "THEIRS", "three"}, idx)

	if err := r.setLocalRefCAS(defaultBranch, patches.Hash{}, ours); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkingTree(r, patches.Index{}, oursIdx); err != nil {
		t.Fatal(err)
	}

	merged, conflicts, err := computeMerge(r, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) == 0 {
		t.Fatal("test setup expected a real conflict")
	}
	if err := writeWorkingTree(r, oursIdx, merged); err != nil {
		t.Fatal(err)
	}
	if err := r.setMergeHeads([]patches.Hash{theirs}); err != nil {
		t.Fatal(err)
	}

	// Simulate the user having hand-edited the conflict-marker content
	// before deciding to bail — abort must discard this too, same as
	// git merge --abort.
	if err := os.WriteFile(filepath.Join(r.root, "f.txt"), []byte("some half-finished resolution attempt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdMergeAbort(r); err != nil {
		t.Fatalf("cmdMergeAbort: %v", err)
	}

	heads, err := r.mergeHeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 0 {
		t.Errorf("mergeHeads after abort = %v, want empty", heads)
	}

	got, err := os.ReadFile(filepath.Join(r.root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "one\nOURS\nthree\n"
	if string(got) != want {
		t.Errorf("f.txt after abort = %q, want %q (restored to head, no markers, no hand-edit survives)", got, want)
	}

	// changedFiles against head should now report a clean tree.
	head, _, err := r.headHash()
	if err != nil {
		t.Fatal(err)
	}
	headIdx, err := r.materialize(head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := changedFiles(r, headIdx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("changedFiles after abort = %v, want empty (clean tree)", changes)
	}
}
