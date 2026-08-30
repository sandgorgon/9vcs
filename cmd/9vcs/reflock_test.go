package main

import (
	"errors"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func TestSetLocalRefCASAllowsCheckedOutBranch(t *testing.T) {
	r := newTestRepo(t)
	h, err := r.Store.Put(&patches.Patch{Message: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Unlike SetRefHash, the local variant must be able to move the
	// currently-checked-out branch — that's exactly what record/merge/
	// apply do on every ordinary local command.
	if err := r.SetLocalRefCAS(repo.DefaultBranch, patches.Hash{}, h); err != nil {
		t.Fatalf("SetLocalRefCAS on the checked-out branch: %v", err)
	}
	if got, exists, _ := r.RefHash(repo.DefaultBranch); !exists || got != h {
		t.Errorf("refs[%s] = %v, %v, want %s, true", repo.DefaultBranch, got, exists, h)
	}
}

func TestSetLocalRefCASConflict(t *testing.T) {
	r := newTestRepo(t)
	oldHash, err := r.Store.Put(&patches.Patch{Message: "old"})
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := r.Store.Put(&patches.Patch{Message: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetLocalRefCAS("feature", patches.Hash{}, oldHash); err != nil {
		t.Fatal(err)
	}

	if err := r.SetLocalRefCAS("feature", newHash /* stale */, newHash); err == nil {
		t.Error("expected a CAS conflict for a stale expected-old, got nil")
	} else if !errors.Is(err, repo.ErrRefConflict) {
		t.Errorf("error doesn't wrap repo.ErrRefConflict: %v", err)
	}
	if got, _, _ := r.RefHash("feature"); got != oldHash {
		t.Errorf("ref changed despite a rejected CAS write: got %s, want unchanged %s", got, oldHash)
	}

	if err := r.SetLocalRefCAS("feature", oldHash, newHash); err != nil {
		t.Fatalf("SetLocalRefCAS with the correct expected-old: %v", err)
	}
	if got, _, _ := r.RefHash("feature"); got != newHash {
		t.Errorf("refs[feature] = %s, want %s", got, newHash)
	}
}
