package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// recordForTest is a minimal stand-in for cmdRecord's core: diff the
// working tree against head, store a patch for whatever changed, move the
// branch ref. No merge/conflict handling — these tests never need it.
func recordForTest(t *testing.T, r *repo, msg string) patches.Hash {
	t.Helper()
	head, _, err := r.headHash()
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.materialize(head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := changedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	var deps []patches.Hash
	if !head.IsZero() {
		deps = append(deps, head)
	}
	patch := &patches.Patch{Dependencies: deps, Author: "test", Time: time.Now(), Message: msg}
	for _, fc := range changes {
		patch.Changes = append(patch.Changes, fc)
	}
	hash, err := r.store.Put(patch)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := r.currentBranch()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.setLocalRefCAS(branch, head, hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func writeWorkingFile(t *testing.T, r *repo, path, content string) {
	t.Helper()
	full := filepath.Join(r.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestChangedFilesSkipsNewIgnoredFile is the actual gap this feature
// closes: a genuinely new, untracked file matching .9vcsignore must never
// be swept into changedFiles' output (and, since the check happens before
// any blob is stored, never even touches r.blobs for a binary file).
func TestChangedFilesSkipsNewIgnoredFile(t *testing.T) {
	r := newTestRepo(t)
	writeIgnoreFile(t, r.root, "*.log\n")
	writeWorkingFile(t, r, "debug.log", "noisy output")
	writeWorkingFile(t, r, "main.go", "package main")

	base, err := r.materialize(patches.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := changedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changes["debug.log"]; ok {
		t.Error("debug.log matches *.log and is untracked; it must not appear in changedFiles' output")
	}
	if _, ok := changes["main.go"]; !ok {
		t.Error("main.go doesn't match any ignore pattern; it must appear in changedFiles' output")
	}
}

// TestChangedFilesNeverDropsAlreadyTrackedFile pins the safety property
// the whole design hinges on: adding a pattern to .9vcsignore after a
// matching file was already recorded must not make changedFiles report it
// as deleted. Ignore patterns only ever gate new files.
func TestChangedFilesNeverDropsAlreadyTrackedFile(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "tracked.log", "already recorded before anyone ignored *.log")
	recordForTest(t, r, "add tracked.log")

	// Only now does .9vcsignore start matching it.
	writeIgnoreFile(t, r.root, "*.log\n")

	head, _, err := r.headHash()
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.materialize(head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := changedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	if fc, ok := changes["tracked.log"]; ok {
		t.Errorf("tracked.log is unmodified and untouched; it should not appear in changedFiles at all, got %+v", fc)
	}

	// Actually modify it, and it must still be picked up as a real edit —
	// not silently ignored just because it now matches a pattern.
	writeWorkingFile(t, r, "tracked.log", "an actual edit")
	changes, err = changedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := changes["tracked.log"]
	if !ok {
		t.Fatal("tracked.log was genuinely edited; it must still appear in changedFiles despite matching *.log")
	}
	if fc.Kind != patches.KindText {
		t.Errorf("tracked.log FileChange.Kind = %v, want KindText", fc.Kind)
	}
}
