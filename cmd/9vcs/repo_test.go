package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// newTestRepo sets up a bare repo the way cmdInit does, without going
// through the CLI (no os.Chdir, no argument parsing) — just enough state
// for repo-method tests.
func newTestRepo(t *testing.T) *repo {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, dotDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := openRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.setHeadBranch(defaultBranch); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSetRefHashCASRefusesCheckedOutBranch(t *testing.T) {
	r := newTestRepo(t)
	h, err := r.store.Put(&patches.Patch{Message: "x"})
	if err != nil {
		t.Fatal(err)
	}

	// defaultBranch ("main") is checked out from newTestRepo's setup.
	err = r.setRefHashCAS(defaultBranch, patches.Hash{}, h)
	if err == nil {
		t.Fatal("expected setRefHashCAS to refuse updating the checked-out branch, got nil")
	}
	if !strings.Contains(err.Error(), "checked out") {
		t.Errorf("error doesn't mention the checked-out branch: %v", err)
	}
	if _, exists, _ := r.refHash(defaultBranch); exists {
		t.Error("ref should not have been created despite the refusal")
	}

	// A different branch name is unaffected by the check.
	if err := r.setRefHashCAS("other", patches.Hash{}, h); err != nil {
		t.Fatalf("setRefHashCAS on a non-checked-out branch: %v", err)
	}
	if got, exists, _ := r.refHash("other"); !exists || got != h {
		t.Errorf("refs[other] = %v, %v, want %s, true", got, exists, h)
	}
}

func TestSetRefHashCASConflict(t *testing.T) {
	r := newTestRepo(t)
	oldHash, err := r.store.Put(&patches.Patch{Message: "old"})
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := r.store.Put(&patches.Patch{Message: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.setRefHashCAS("feature", patches.Hash{}, oldHash); err != nil {
		t.Fatal(err)
	}

	// Stale expected-old: must fail, must not change the ref.
	if err := r.setRefHashCAS("feature", newHash /* wrong */, newHash); err == nil {
		t.Error("expected a CAS conflict for a stale expected-old, got nil")
	}
	if got, _, _ := r.refHash("feature"); got != oldHash {
		t.Errorf("ref changed despite a rejected CAS write: got %s, want unchanged %s", got, oldHash)
	}

	// Correct expected-old: must succeed.
	if err := r.setRefHashCAS("feature", oldHash, newHash); err != nil {
		t.Fatalf("setRefHashCAS with the correct expected-old: %v", err)
	}
	if got, _, _ := r.refHash("feature"); got != newHash {
		t.Errorf("refs[feature] = %s, want %s", got, newHash)
	}
}

func TestSetRefHashCASRejectsUnknownPatch(t *testing.T) {
	r := newTestRepo(t)
	var unknown patches.Hash
	for i := range unknown {
		unknown[i] = 0xAB
	}
	if err := r.setRefHashCAS("feature", patches.Hash{}, unknown); err == nil {
		t.Error("expected setRefHashCAS to reject a hash not present in the store, got nil")
	}
	if _, exists, _ := r.refHash("feature"); exists {
		t.Error("ref should not have been created for an unknown patch hash")
	}
}
