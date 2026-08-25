package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func TestWriteSidecarFileCreatesFileWithContent(t *testing.T) {
	r := newTestRepo(t)
	if err := writeSidecarFile(r, "logo.png.abc123456789", []byte("binary content")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(r.root, "logo.png.abc123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary content" {
		t.Errorf("content = %q, want %q", got, "binary content")
	}
}

// TestWriteSidecarFileRefusesSymlinkPathEscape and
// TestRemoveSidecarFileRefusesSymlinkPathEscape are the regression tests
// for a real, live-proven vulnerability: merge/apply's binary-conflict
// sidecar write, and merge-abort/record's sidecar cleanup, each used a
// plain filepath.Join + os.WriteFile/os.MkdirAll/os.Remove — none of it
// routed through os.Root, unlike writeWorkingTree (fixed earlier for the
// same bug class). c.Path is only string-validated (no "..", no leading
// "/"); nothing about that says what currently sits on disk at an
// intermediate path component.
//
// The realistic trigger needs no attacker-crafted patch at all: an
// ordinary symlinked cache/vendor directory already sitting in the
// working tree, plus a completely mundane two-sided binary conflict at
// a path underneath it (e.g. "vendor/pwned.bin"), sends the sidecar
// write straight through the symlink and outside the repo. Confirmed
// live (then reverted) against the pre-fix code: cmdMergeAbort's
// sidecar-removal loop deleted a file entirely outside the repo through
// exactly this kind of symlink.
func TestWriteSidecarFileRefusesSymlinkPathEscape(t *testing.T) {
	r := newTestRepo(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(r.root, "evil")); err != nil {
		t.Fatal(err)
	}

	if err := writeSidecarFile(r, "evil/pwned.bin.abc123456789", []byte("ATTACKER PAYLOAD")); err == nil {
		t.Fatal("expected writeSidecarFile to refuse writing through a symlink escaping the repo")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.bin.abc123456789")); !os.IsNotExist(err) {
		t.Errorf("sidecar must not escape to %s; stat err = %v", outside, err)
	}
}

func TestRemoveSidecarFileRefusesSymlinkPathEscape(t *testing.T) {
	r := newTestRepo(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "innocent.txt")
	if err := os.WriteFile(victim, []byte("do not delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.root, "evil")); err != nil {
		t.Fatal(err)
	}

	if err := removeSidecarFile(r, "evil/innocent.txt"); err == nil {
		t.Fatal("expected removeSidecarFile to refuse removing through a symlink escaping the repo")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim file outside the repo must survive: %v", err)
	}
}

// TestMergeAbortRefusesSidecarRemovalThroughSymlinkEscape drives the
// same bug through the real production entrypoint (cmdMergeAbort, which
// merge/apply's own abort path and MERGE_SIDECARS state actually use),
// not just the extracted helper — proving the fix is actually wired in,
// not just correct in isolation.
func TestMergeAbortRefusesSidecarRemovalThroughSymlinkEscape(t *testing.T) {
	r := newTestRepo(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "innocent.txt")
	if err := os.WriteFile(victim, []byte("do not delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.root, "evil")); err != nil {
		t.Fatal(err)
	}

	base, _ := recordTestPatch(t, r, nil, "f.txt", []string{"one"}, patches.Index{})
	if err := r.setLocalRefCAS(defaultBranch, patches.Hash{}, base); err != nil {
		t.Fatal(err)
	}
	if err := r.setMergeHeads([]patches.Hash{base}); err != nil {
		t.Fatal(err)
	}
	if err := r.setMergeSidecars([]string{"evil/innocent.txt"}); err != nil {
		t.Fatal(err)
	}

	// cmdMergeAbort surfaces the removal failure as an error rather than
	// silently skipping it — that's acceptable (it's not the happy path),
	// what matters is that the victim file outside the repo survives
	// either way.
	_ = cmdMergeAbort(r)
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim file outside the repo must survive cmdMergeAbort: %v", err)
	}
}
