package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// newTestRepo sets up a bare repo the way cmdInit does, without going
// through the CLI (no os.Chdir, no argument parsing) — just enough state
// for repo-method tests.
func newTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, repo.DotDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetHeadBranch(repo.DefaultBranch); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSetRefHashCASRefusesCheckedOutBranch(t *testing.T) {
	r := newTestRepo(t)
	h, err := r.Store.Put(&patches.Patch{Message: "x"})
	if err != nil {
		t.Fatal(err)
	}

	// repo.DefaultBranch ("main") is checked out from newTestRepo's setup.
	err = r.SetRefHash(repo.DefaultBranch, patches.Hash{}, h)
	if err == nil {
		t.Fatal("expected SetRefHash to refuse updating the checked-out branch, got nil")
	}
	if !strings.Contains(err.Error(), "checked out") {
		t.Errorf("error doesn't mention the checked-out branch: %v", err)
	}
	if _, exists, _ := r.RefHash(repo.DefaultBranch); exists {
		t.Error("ref should not have been created despite the refusal")
	}

	// A different branch name is unaffected by the check.
	if err := r.SetRefHash("other", patches.Hash{}, h); err != nil {
		t.Fatalf("SetRefHash on a non-checked-out branch: %v", err)
	}
	if got, exists, _ := r.RefHash("other"); !exists || got != h {
		t.Errorf("refs[other] = %v, %v, want %s, true", got, exists, h)
	}
}

func TestSetRefHashCASConflict(t *testing.T) {
	r := newTestRepo(t)
	oldHash, err := r.Store.Put(&patches.Patch{Message: "old"})
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := r.Store.Put(&patches.Patch{Message: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetRefHash("feature", patches.Hash{}, oldHash); err != nil {
		t.Fatal(err)
	}

	// Stale expected-old: must fail, must not change the ref.
	if err := r.SetRefHash("feature", newHash /* wrong */, newHash); err == nil {
		t.Error("expected a CAS conflict for a stale expected-old, got nil")
	}
	if got, _, _ := r.RefHash("feature"); got != oldHash {
		t.Errorf("ref changed despite a rejected CAS write: got %s, want unchanged %s", got, oldHash)
	}

	// Correct expected-old: must succeed.
	if err := r.SetRefHash("feature", oldHash, newHash); err != nil {
		t.Fatalf("SetRefHash with the correct expected-old: %v", err)
	}
	if got, _, _ := r.RefHash("feature"); got != newHash {
		t.Errorf("refs[feature] = %s, want %s", got, newHash)
	}
}

func TestMergeHeadsRoundTrip(t *testing.T) {
	r := newTestRepo(t)

	if heads, err := r.MergeHeads(); err != nil || len(heads) != 0 {
		t.Fatalf("MergeHeads() on a fresh repo = %v, %v, want empty, nil", heads, err)
	}

	a, err := r.Store.Put(&patches.Patch{Message: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Store.Put(&patches.Patch{Message: "b"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Store.Put(&patches.Patch{Message: "c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []patches.Hash{a, b, c}
	if err := r.SetMergeHeads(want); err != nil {
		t.Fatal(err)
	}

	got, err := r.MergeHeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("mergeHeads() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mergeHeads()[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	if err := r.ClearMergeHeads(); err != nil {
		t.Fatal(err)
	}
	if heads, err := r.MergeHeads(); err != nil || len(heads) != 0 {
		t.Fatalf("MergeHeads() after ClearMergeHeads = %v, %v, want empty, nil", heads, err)
	}
}

// TestSetMergeHeadsAndSidecarsWriteAtomically is the regression test for
// setMergeHeads/setMergeSidecars using plain os.WriteFile — unlike every
// other ref/HEAD write in this package — which meant a crash mid-write
// could leave a truncated MERGE_HEAD/MERGE_SIDECARS behind. Both now go
// through atomicWriteFile (temp file + rename), same as
// TestAtomicWriteFileLeavesNoTempFile verifies for the primitive itself;
// this just confirms the two merge-state writers actually route through
// it and leave no leftover .tmp file.
func TestSetMergeHeadsAndSidecarsWriteAtomically(t *testing.T) {
	r := newTestRepo(t)

	a, err := r.Store.Put(&patches.Patch{Message: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetMergeHeads([]patches.Hash{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "MERGE_HEAD") + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no leftover MERGE_HEAD.tmp, stat err = %v", err)
	}

	if err := r.SetMergeSidecars([]string{"logo.png.abcdef123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "MERGE_SIDECARS") + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no leftover MERGE_SIDECARS.tmp, stat err = %v", err)
	}
}

func TestSetRefHashCASRejectsUnknownPatch(t *testing.T) {
	r := newTestRepo(t)
	var unknown patches.Hash
	for i := range unknown {
		unknown[i] = 0xAB
	}
	if err := r.SetRefHash("feature", patches.Hash{}, unknown); err == nil {
		t.Error("expected SetRefHash to reject a hash not present in the store, got nil")
	}
	if _, exists, _ := r.RefHash("feature"); exists {
		t.Error("ref should not have been created for an unknown patch hash")
	}
}
