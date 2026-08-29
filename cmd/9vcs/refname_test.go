package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func TestValidRefNameRejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		"/etc/passwd",
		"..",
		"../evil",
		"a/../../evil",
		"a/..",
		"a/./b", // path.Clean would collapse this, same as validPath
		"a//b",  // empty segment
		"a/b/",  // trailing slash
	}
	for _, name := range bad {
		if repo.ValidRefName(name) {
			t.Errorf("ValidRefName(%q) = true, want false", name)
		}
	}
}

func TestValidRefNameAllowsOrdinaryAndNestedNames(t *testing.T) {
	good := []string{"main", "feature/foo", "release/2026.08", "a/b/c"}
	for _, name := range good {
		if !repo.ValidRefName(name) {
			t.Errorf("ValidRefName(%q) = false, want true", name)
		}
	}
}

func TestRefHashRejectsInvalidName(t *testing.T) {
	r := newTestRepo(t)
	if _, _, err := r.RefHash("../evil"); err == nil {
		t.Fatal("expected RefHash to refuse a traversal name, got nil error")
	}
}

// TestSetRefHashRejectsTraversalEscape is the regression test for a
// real, live-proven vulnerability: SetRefHash is the exact method a
// peer's 9P Twalk/Tcreate drives directly over the network (*repo.Repo
// satisfies vcsfs.RefWriter/RefReader directly — vcsfs itself has no
// path logic for refs at all), and the 9p server library performs no
// validation of its own on a wname/create-name element (each is passed
// straight to File.Walk; see server/dispatch.go's tWalk), so a single
// crafted ref name containing its own embedded "/../" sequences reached
// refPath's plain filepath.Join(r.Dir, "refs", name) unfiltered before
// this fix. Confirmed live (then reverted): a real filepath.Rel-computed
// payload wrote a ref file entirely outside the repo via exactly this
// call.
func TestSetRefHashRejectsTraversalEscape(t *testing.T) {
	r := newTestRepo(t)
	outside := t.TempDir()

	target, err := r.Store.Put(&patches.Patch{Message: "attacker patch"})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(r.Dir, "refs"), outside)
	if err != nil {
		t.Fatal(err)
	}
	evilName := filepath.ToSlash(rel) + "/pwned"

	if err := r.SetRefHash(evilName, patches.Hash{}, target); err == nil {
		t.Fatal("expected SetRefHash to refuse a traversal ref name, got nil error")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Errorf("no file should have been written outside the repo; stat err = %v", err)
	}
}

// TestRefHashRejectsTraversalEscape is the read-side twin: the same
// unvalidated name reaching RefHash could otherwise probe for the
// existence/content of arbitrary files outside .9vcs/refs.
func TestRefHashRejectsTraversalEscape(t *testing.T) {
	r := newTestRepo(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("not a ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(r.Dir, "refs"), outside)
	if err != nil {
		t.Fatal(err)
	}
	evilName := filepath.ToSlash(rel) + "/secret"

	if _, ok, err := r.RefHash(evilName); err == nil {
		t.Fatalf("expected RefHash to refuse a traversal ref name, got ok=%v err=nil", ok)
	}
}

// TestSetLocalRefCASRejectsTraversalName confirms the same fix protects
// the purely-local path (branch/checkout -b both go through
// setLocalRefCAS), not just the network boundary — a local user's own
// typo or mistake shouldn't be able to write outside .9vcs/refs either.
func TestSetLocalRefCASRejectsTraversalName(t *testing.T) {
	r := newTestRepo(t)
	base, _ := recordTestPatch(t, r, nil, "f.txt", []string{"one"}, patches.Index{})
	if err := r.SetLocalRefCAS("../../evil", patches.Hash{}, base); err == nil {
		t.Error("expected SetLocalRefCAS to refuse a traversal branch name, got nil")
	}
}
